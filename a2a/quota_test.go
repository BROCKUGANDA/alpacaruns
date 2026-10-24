package a2a

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeClock drives the bridge's clock so window behaviour is tested by
// moving time, not by sleeping in it.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// clockedBridge builds a bridge whose clock the test controls.
func clockedBridge(t *testing.T, cfg Config) (*Bridge, *fakeClock) {
	t.Helper()
	b := testBridge(t, cfg)
	clk := &fakeClock{t: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)}
	b.now = clk.now
	return b, clk
}

// signalWith returns the shared fixture under a fresh id.
func signalWith(id string) Signal {
	sig := sampleSignal()
	sig.ID = id
	return sig
}

// stateOf pulls one agent's counters.
func stateOf(t *testing.T, b *Bridge, agent string) AgentState {
	t.Helper()
	states, err := b.States()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range states {
		if a.Agent == agent {
			return a
		}
	}
	t.Fatalf("no state recorded for %q", agent)
	return AgentState{}
}

// TestQuotaIsARollingWindowNotALifetimeCounter is the regression test for
// the lifetime counter: 60 signals are accepted, the 61st is refused
// while it is still inside the hour, and the very same 61st signal is
// accepted once the window has rolled. A counter that only ever went up
// would refuse it forever.
func TestQuotaIsARollingWindowNotALifetimeCounter(t *testing.T) {
	b, clk := clockedBridge(t, Config{
		Enabled:           true,
		Token:             "secret",
		Agents:            []string{"research"},
		DefaultMode:       ModeOn,
		MaxSignalsPerHour: 60,
	})
	for i := 1; i <= 60; i++ {
		dec := b.Ingest(signalWith(fmt.Sprintf("s%02d", i)))
		if !dec.Accepted {
			t.Fatalf("signal %d is inside the window and must be accepted: %+v", i, dec)
		}
	}
	dec := b.Ingest(signalWith("s61"))
	if dec.Accepted {
		t.Fatalf("the 61st signal inside the hour must be refused: %+v", dec)
	}
	if !strings.Contains(dec.Reason, "rolling") || !strings.Contains(dec.Reason, "limit 60") {
		t.Fatalf("refusal must name the rolling window, got %q", dec.Reason)
	}
	if strings.Contains(dec.Reason, "hourly limit") {
		t.Fatalf("the old reason claimed a permanent hourly limit: %q", dec.Reason)
	}
	if st := stateOf(t, b, "research"); st.Accepted != 60 || st.Refused != 1 {
		t.Fatalf("counters = accepted %d refused %d, want 60/1", st.Accepted, st.Refused)
	}

	// Still inside the window: refused again, and the refusal itself must
	// not consume quota or a signal id.
	clk.advance(59 * time.Minute)
	if dec := b.Ingest(signalWith("s61")); dec.Accepted {
		t.Fatalf("signal still inside the window must stay refused: %+v", dec)
	}

	// Past the window the 60 stamps have aged out. Same signal, same id,
	// now admissible.
	clk.advance(2 * time.Minute)
	dec = b.Ingest(signalWith("s61"))
	if !dec.Accepted {
		t.Fatalf("after the window rolls the 61st must be accepted: %+v", dec)
	}
	if st := stateOf(t, b, "research"); st.Accepted != 61 {
		t.Fatalf("accepted = %d, want 61", st.Accepted)
	}
}

// TestQuotaWindowRollsPerSignal proves the window is genuinely rolling:
// one of the two counted signals ages out while the other is still
// inside, so the third signal is admitted on the strength of the stamp
// times and not of a fixed clock boundary.
func TestQuotaWindowRollsPerSignal(t *testing.T) {
	b, clk := clockedBridge(t, Config{
		Enabled:           true,
		Token:             "secret",
		Agents:            []string{"research"},
		DefaultMode:       ModeOn,
		MaxSignalsPerHour: 2,
		SignalWindow:      30 * time.Minute,
		MaxPending:        50,
	})
	if dec := b.Ingest(signalWith("a1")); !dec.Accepted {
		t.Fatalf("first signal must be accepted: %+v", dec)
	}
	clk.advance(20 * time.Minute)
	if dec := b.Ingest(signalWith("a2")); !dec.Accepted {
		t.Fatalf("second signal must be accepted: %+v", dec)
	}
	clk.advance(5 * time.Minute)
	if dec := b.Ingest(signalWith("a3")); dec.Accepted {
		t.Fatalf("third signal inside the window must be refused: %+v", dec)
	}
	// 10:25 + 10m = 10:35: a1 (10:00) has aged out of a 30m window, a2
	// (10:20) has not. Only one stamp remains, so there is room for one.
	clk.advance(10 * time.Minute)
	dec := b.Ingest(signalWith("a3"))
	if !dec.Accepted {
		t.Fatalf("with one stamp aged out the signal must be accepted: %+v", dec)
	}
	if dec := b.Ingest(signalWith("a4")); dec.Accepted {
		t.Fatalf("the window is full again (a2 + a3): %+v", dec)
	}
}

// TestPendingQueueIsCappedAndHoldsWhatItHeld covers the unbounded queue:
// the newcomer is refused once the cap is reached, nothing already
// queued is evicted to make room, and the refusal is counted.
func TestPendingQueueIsCappedAndHoldsWhatItHeld(t *testing.T) {
	b, clk := clockedBridge(t, Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
		MaxPending:  2,
	})
	first := signalWith("q1")
	second := signalWith("q2")
	if dec := b.Ingest(first); !dec.Accepted {
		t.Fatalf("q1 must be accepted: %+v", dec)
	}
	if dec := b.Ingest(second); !dec.Accepted {
		t.Fatalf("q2 must be accepted: %+v", dec)
	}
	dec := b.Ingest(signalWith("q3"))
	if dec.Accepted {
		t.Fatal("a signal arriving at a full queue must be refused, not queued")
	}
	if !strings.Contains(dec.Reason, "queue is full") || !strings.Contains(dec.Reason, "cap 2") {
		t.Fatalf("refusal must name the queue cap, got %q", dec.Reason)
	}
	if st := stateOf(t, b, "research"); st.Refused != 1 {
		t.Fatalf("the queue-full refusal must be counted, refused=%d", st.Refused)
	}
	// The two signals that were already waiting are still there, intact.
	drained, err := b.Drain(b.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(drained) != 2 {
		t.Fatalf("the queue must still hold both accepted signals, got %d", len(drained))
	}
	if drained[0].ID != "q1" || drained[1].ID != "q2" {
		t.Fatalf("queued signals were dropped or reordered: %s, %s", drained[0].ID, drained[1].ID)
	}
	// Drain hands the signals to the consumer but, by this bridge's
	// existing contract, keeps them until they expire — so the queue is
	// still full and the cap still bites.
	if dec := b.Ingest(signalWith("q3")); dec.Accepted {
		t.Fatalf("the cap must hold while the queue is full: %+v", dec)
	}
	// Room frees when the waiting signals age out — never by evicting a
	// queued signal to make room for a newcomer.
	clk.advance(16 * time.Minute)
	cleared, err := b.Drain(b.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 0 {
		t.Fatalf("expired signals must not be handed to the consumer, got %d", len(cleared))
	}
	if dec := b.Ingest(signalWith("q3")); !dec.Accepted {
		t.Fatalf("once the queue emptied the signal must be accepted: %+v", dec)
	}
}

// TestNewBridgeDefaultsPendingCap pins the constructor default, which is
// what protects every construction path (API and bot loop) from an
// unbounded queue even when the caller never sets MaxPending.
func TestNewBridgeDefaultsPendingCap(t *testing.T) {
	b := testBridge(t, Config{Enabled: true, Token: "secret", Agents: []string{"research"}, DefaultMode: ModeOn})
	if b.cfg.MaxPending <= 0 {
		t.Fatalf("MaxPending must default to a positive cap, got %d", b.cfg.MaxPending)
	}
	if b.cfg.SignalWindow != time.Hour {
		t.Fatalf("the quota window must default to one hour, got %s", b.cfg.SignalWindow)
	}
}
