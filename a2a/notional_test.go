package a2a

import (
	"strings"
	"testing"
)

// TestParseNotionalOnlyClaimsToKnowAValueItCanSee is the unit test for the
// cap bypass: a qty-only signal used to read as notional 0, which made
// every "0 > cap" check false and waved any size through. It now reports
// that the exposure is unknown instead of pretending it is free.
func TestParseNotionalOnlyClaimsToKnowAValueItCanSee(t *testing.T) {
	cases := []struct {
		name      string
		sig       Signal
		wantValue float64
		wantKnown bool
	}{
		{"explicit notional", Signal{Notional: "1250.50"}, 1250.50, true},
		{"notional with spaces", Signal{Notional: "  80  "}, 80, true},
		{"notional beats qty", Signal{Notional: "90", Qty: "100000"}, 90, true},
		{"qty only is unknowable", Signal{Qty: "100000"}, 0, false},
		{"unparsable notional is unknowable", Signal{Notional: "lots"}, 0, false},
		{"nothing at all is unknowable", Signal{}, 0, false},
	}
	for _, tc := range cases {
		v, known := parseNotional(tc.sig)
		if v != tc.wantValue || known != tc.wantKnown {
			t.Errorf("%s: parseNotional = (%v, %v), want (%v, %v)", tc.name, v, known, tc.wantValue, tc.wantKnown)
		}
	}
}

// TestQtyOnlySignalIsRefusedUnderTheCap covers the bypass end to end: a
// huge qty with no notional must not be queued, because the bridge cannot
// price it and must not guess a price.
func TestQtyOnlySignalIsRefusedUnderTheCap(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
		MaxNotional: 1000,
	})
	sig := sampleSignal()
	sig.Notional = ""
	sig.Qty = "900000"
	dec := b.Ingest(sig)
	if dec.Accepted {
		t.Fatalf("a qty-only signal under a notional cap must be refused, not waved through: %+v", dec)
	}
	if !strings.Contains(dec.Reason, "notional") {
		t.Fatalf("refusal must explain that the exposure is uncheckable, got %q", dec.Reason)
	}
	if st := stateOf(t, b, "research"); st.Refused != 1 {
		t.Fatalf("the refusal must be counted, refused=%d", st.Refused)
	}
	pending, err := b.Drain(b.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("a refused signal must never be queued, got %d", len(pending))
	}
}

// TestUnparsableNotionalIsRefused: "notional" is only a value if it
// parses. Garbage is not a zero-cost signal.
func TestUnparsableNotionalIsRefused(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
		MaxNotional: 1000,
	})
	sig := sampleSignal()
	sig.Qty = ""
	sig.Notional = "all-in"
	if dec := b.Ingest(sig); dec.Accepted {
		t.Fatalf("an unparseable notional must be refused: %+v", dec)
	}
}

// TestExplicitNotionalAboveCapRefusedBelowCapAccepted is the pair that
// shows the cap actually bites: the same shape of signal, one over the
// line and one under it.
func TestExplicitNotionalAboveCapRefusedBelowCapAccepted(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
		MaxNotional: 1000,
	})
	over := sampleSignal()
	over.ID = "over"
	over.Qty = ""
	over.Notional = "1000.01"
	dec := b.Ingest(over)
	if dec.Accepted {
		t.Fatalf("notional 1000.01 over a 1000 cap must be refused: %+v", dec)
	}
	if !strings.Contains(dec.Reason, "exceeds the bridge cap") {
		t.Fatalf("refusal must name the cap, got %q", dec.Reason)
	}

	under := sampleSignal()
	under.ID = "under"
	under.Qty = ""
	under.Notional = "999.99"
	if dec := b.Ingest(under); !dec.Accepted {
		t.Fatalf("notional 999.99 under a 1000 cap must be accepted: %+v", dec)
	}

	// Exactly at the cap is still inside it.
	at := sampleSignal()
	at.ID = "at"
	at.Qty = ""
	at.Notional = "1000"
	if dec := b.Ingest(at); !dec.Accepted {
		t.Fatalf("notional equal to the cap must be accepted: %+v", dec)
	}
}
