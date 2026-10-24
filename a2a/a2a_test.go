package a2a

import (
	"path/filepath"
	"testing"
	"time"
)

func testBridge(t *testing.T, cfg Config) *Bridge {
	t.Helper()
	dir := t.TempDir()
	return NewBridge(cfg, NewStore(filepath.Join(dir, "trades.jsonl")))
}

func sampleSignal() Signal {
	return Signal{
		ID:     "sig-1",
		Agent:  "research",
		Symbol: "AAPL",
		Side:   "buy",
		Qty:    "10",
		// The bridge will not accept a qty-only signal while a notional
		// cap is configured (it cannot price one — see parseNotional),
		// so the shared fixture carries the exposure the cap is checked
		// against. Ramp tests are about the ramp, not the cap.
		Notional:   "500",
		Confidence: 0.8,
		Reason:     "momentum",
	}
}

// TestRampRefusesWhenOff proves an agent that has not been ramped on
// cannot get a signal through: the ingress refuses it outright.
func TestRampRefusesWhenOff(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOff,
	})
	dec := b.Ingest(sampleSignal())
	if dec.Accepted {
		t.Fatalf("a ramped-off agent must not be accepted: %+v", dec)
	}
	pending, err := b.Drain(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("no signal may be queued while off, got %d", len(pending))
	}
}

// TestRampShadowNeverTrades proves shadow mode accepts and journals a
// signal but queues nothing that could become an order.
func TestRampShadowNeverTrades(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeShadow,
	})
	dec := b.Ingest(sampleSignal())
	if !dec.Accepted {
		t.Fatalf("shadow mode must accept the signal: %+v", dec)
	}
	// The consumer drains it and evaluates it; the outcome tells the
	// bridge whether the shadow streak advanced.
	b.RecordOutcome("research", true, "shadow ok")
	b.RecordOutcome("research", true, "shadow ok")
	states, err := b.States()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].ShadowPasses != 2 {
		t.Fatalf("shadow streak = %+v, want 2 passes", states)
	}
	if states[0].Traded != 0 {
		t.Fatalf("shadow mode must never count a trade: %+v", states[0])
	}
}

// TestRampOnRequiresEarnedPasses proves an agent cannot be ramped on
// without the shadow streak it was supposed to earn.
func TestRampOnRequiresEarnedPasses(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:      true,
		Token:        "secret",
		Agents:       []string{"research"},
		DefaultMode:  ModeShadow,
		ShadowPasses: 3,
	})
	if _, err := b.Ramp("research", ModeOn, false); err == nil {
		t.Fatal("ramping on without shadow passes must be refused")
	}
	for i := 0; i < 2; i++ {
		b.RecordOutcome("research", true, "ok")
	}
	if _, err := b.Ramp("research", ModeOn, false); err == nil {
		t.Fatal("ramping on with 2 of 3 passes must be refused")
	}
	b.RecordOutcome("research", true, "ok")
	state, err := b.Ramp("research", ModeOn, false)
	if err != nil {
		t.Fatalf("ramping on with 3 of 3 passes must be allowed: %v", err)
	}
	if state.Mode != ModeOn {
		t.Fatalf("mode = %s, want on", state.Mode)
	}
}

// TestRampRejectionResetsStreak proves a rejected shadow evaluation
// wipes the progress, so an agent cannot bank two good signals and
// then ride a failing one into production.
func TestRampRejectionResetsStreak(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:      true,
		Token:        "secret",
		Agents:       []string{"research"},
		DefaultMode:  ModeShadow,
		ShadowPasses: 3,
	})
	b.RecordOutcome("research", true, "ok")
	b.RecordOutcome("research", true, "ok")
	b.RecordOutcome("research", false, "risk rejected")
	states, _ := b.States()
	if states[0].ShadowPasses != 0 {
		t.Fatalf("a rejected evaluation must reset the streak, got %d", states[0].ShadowPasses)
	}
	if states[0].ShadowFails != 1 {
		t.Fatalf("shadow failures = %d, want 1", states[0].ShadowFails)
	}
}

// TestRampDownIsAlwaysAvailable proves the off ramp can never be
// blocked — it is the safety mechanism.
func TestRampDownIsAlwaysAvailable(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
	})
	if _, err := b.Ramp("research", ModeOn, false); err == nil {
		t.Fatal("ramping on from on-mode without shadow passes must be refused")
	}
	state, err := b.Ramp("research", ModeOff, false)
	if err != nil {
		t.Fatalf("ramping off must always work: %v", err)
	}
	if state.Mode != ModeOff {
		t.Fatalf("mode = %s, want off", state.Mode)
	}
}

// TestIngestRefusesUnlistedAgent proves the allow-list is enforced.
func TestIngestRefusesUnlistedAgent(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
	})
	sig := sampleSignal()
	sig.Agent = "stranger"
	sig.ID = "sig-2"
	if dec := b.Ingest(sig); dec.Accepted {
		t.Fatal("an agent outside the allow-list must be refused")
	}
}

// TestIngestRefusesWithoutToken proves there is no anonymous mode.
func TestIngestRefusesWithoutToken(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:     true,
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
	})
	if dec := b.Ingest(sampleSignal()); dec.Accepted {
		t.Fatal("a bridge with no token must refuse every signal")
	}
}

// TestIngestDropsDuplicates proves a replayed signal id is dropped.
func TestIngestDropsDuplicates(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
	})
	if dec := b.Ingest(sampleSignal()); !dec.Accepted {
		t.Fatal("first signal must be accepted")
	}
	dec := b.Ingest(sampleSignal())
	if dec.Accepted {
		t.Fatal("a duplicate signal id must be refused")
	}
}

// TestIngestRefusesDisabledBridge proves the ingress is opt-in.
func TestIngestRefusesDisabledBridge(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:     false,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
	})
	if dec := b.Ingest(sampleSignal()); dec.Accepted {
		t.Fatal("a disabled bridge must refuse every signal")
	}
}

// TestSignalValidation proves malformed signals are rejected before
// they can reach the queue.
func TestSignalValidation(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		mut  func(*Signal)
	}{
		{"no id", func(s *Signal) { s.ID = "" }},
		{"no agent", func(s *Signal) { s.Agent = "" }},
		{"no symbol", func(s *Signal) { s.Symbol = "" }},
		{"bad side", func(s *Signal) { s.Side = "long" }},
		{"no size", func(s *Signal) { s.Qty = ""; s.Notional = "" }},
		{"bad confidence", func(s *Signal) { s.Confidence = 2 }},
		{"expired", func(s *Signal) { s.ExpiresAt = now.Add(-time.Hour) }},
	}
	for _, tc := range cases {
		sig := sampleSignal()
		tc.mut(&sig)
		if err := sig.Validate(now); err == nil {
			t.Errorf("%s: expected a validation error", tc.name)
		}
	}
}
