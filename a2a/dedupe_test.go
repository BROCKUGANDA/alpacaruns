package a2a

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSameIdSamePayloadStaysADuplicate pins the behaviour that already
// worked: a true replay is dropped quietly and is not counted as a
// refusal.
func TestSameIdSamePayloadStaysADuplicate(t *testing.T) {
	b := testBridge(t, Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
	})
	if dec := b.Ingest(sampleSignal()); !dec.Accepted {
		t.Fatalf("first signal must be accepted: %+v", dec)
	}
	dec := b.Ingest(sampleSignal())
	if dec.Accepted {
		t.Fatal("a replayed id must not be accepted twice")
	}
	if dec.Reason != "duplicate signal id" {
		t.Fatalf("an identical replay must keep the duplicate reason, got %q", dec.Reason)
	}
	if st := stateOf(t, b, "research"); st.Refused != 0 {
		t.Fatalf("a plain duplicate is not a refusal, refused=%d", st.Refused)
	}
}

// TestSameIdAlteredPayloadIsRefusedAsReuse is the core of the fix: a peer
// that reuses a spent id for a DIFFERENT order is refused with its own
// reason, instead of having the second signal silently dropped.
func TestSameIdAlteredPayloadIsRefusedAsReuse(t *testing.T) {
	mutations := []struct {
		name string
		mut  func(*Signal)
	}{
		{"symbol", func(s *Signal) { s.Symbol = "NVDA" }},
		{"side", func(s *Signal) { s.Side = "sell" }},
		{"qty", func(s *Signal) { s.Qty = "999999" }},
		{"notional", func(s *Signal) { s.Notional = "9000" }},
		{"confidence", func(s *Signal) { s.Confidence = 0.1 }},
	}
	for _, tc := range mutations {
		b := testBridge(t, Config{
			Enabled:     true,
			Token:       "secret",
			Agents:      []string{"research"},
			DefaultMode: ModeOn,
		})
		if dec := b.Ingest(sampleSignal()); !dec.Accepted {
			t.Fatalf("%s: first signal must be accepted: %+v", tc.name, dec)
		}
		alt := sampleSignal()
		tc.mut(&alt)
		dec := b.Ingest(alt)
		if dec.Accepted {
			t.Fatalf("%s: a reused id carrying a different payload must be refused: %+v", tc.name, dec)
		}
		if dec.Reason != "signal id reused with a different payload" {
			t.Fatalf("%s: want the reuse reason, got %q", tc.name, dec.Reason)
		}
		if strings.Contains(dec.Reason, "duplicate") {
			t.Fatalf("%s: a reuse must not be reported as a duplicate replay", tc.name)
		}
		if st := stateOf(t, b, "research"); st.Refused != 1 {
			t.Fatalf("%s: the reuse must be counted as a refusal, refused=%d", tc.name, st.Refused)
		}
	}
}

// TestReuseAttemptLeavesTheOriginalSignalIntact proves the refusal is not
// a swap: the accepted signal still sits in the queue unchanged, the
// attacker's payload never got near the state file, and the original's
// dedupe record is still the original's.
func TestReuseAttemptLeavesTheOriginalSignalIntact(t *testing.T) {
	dir := t.TempDir()
	b := NewBridge(Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
	}, NewStore(filepath.Join(dir, "trades.jsonl")))

	if dec := b.Ingest(sampleSignal()); !dec.Accepted {
		t.Fatalf("first signal must be accepted: %+v", dec)
	}
	hijack := sampleSignal()
	hijack.Symbol = "ZZZ"
	hijack.Side = "sell"
	hijack.Notional = "9000"
	hijack.Reason = "totally different idea"
	if dec := b.Ingest(hijack); dec.Accepted {
		t.Fatalf("the reuse attempt must be refused: %+v", dec)
	}

	// The queued signal is the original, byte for byte.
	drained, err := b.Drain(b.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(drained) != 1 {
		t.Fatalf("exactly one signal may be queued, got %d", len(drained))
	}
	if drained[0].Symbol != "AAPL" || drained[0].Side != "buy" || drained[0].Notional != "500" {
		t.Fatalf("the queued signal was altered: %+v", drained[0])
	}
	if drained[0].Reason != "momentum" {
		t.Fatalf("the queued signal's rationale changed: %q", drained[0].Reason)
	}

	// The rejected payload left nothing behind in the state file — only
	// the hash of the original does.
	raw, err := os.ReadFile(b.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, leak := range []string{"9000", "ZZZ", "totally different idea"} {
		if strings.Contains(text, leak) {
			t.Fatalf("the refused payload leaked into the state file: %q", leak)
		}
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("state file must still parse: %v", err)
	}
	entry, ok := st.Seen["sig-1"]
	if !ok {
		t.Fatal("the accepted signal id must still be remembered")
	}
	if entry.Fingerprint != Fingerprint(sampleSignal()) {
		t.Fatalf("the dedupe record must hold the ORIGINAL payload's fingerprint")
	}
	if len(entry.Fingerprint) != 64 {
		t.Fatalf("the record must be a sha256 hex digest, got %q", entry.Fingerprint)
	}

	// And the original id is still a plain duplicate afterwards, not a
	// reuse: the record was not overwritten by the failed attempt.
	if dec := b.Ingest(sampleSignal()); dec.Reason != "duplicate signal id" {
		t.Fatalf("replaying the original must still read as a duplicate, got %q", dec.Reason)
	}
}

// TestLegacySeenEntryWithoutFingerprintStillDedupes guards the upgrade
// path: a state file written before fingerprinting mapped an id straight
// to a timestamp. It must still load (or the bridge would refuse
// everything) and still dedupe, and without a stored payload it cannot
// accuse anyone of reuse.
func TestLegacySeenEntryWithoutFingerprintStillDedupes(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "a2a-state.json")
	legacy := `{"version":1,"agents":{"research":{"agent":"research","mode":"on"}},"seen":{"sig-1":"2026-03-02T10:00:00Z"}}`
	if err := os.WriteFile(statePath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	b := NewBridge(Config{
		Enabled:     true,
		Token:       "secret",
		Agents:      []string{"research"},
		DefaultMode: ModeOn,
	}, NewStore(filepath.Join(dir, "trades.jsonl")))

	st, err := b.store.Load()
	if err != nil {
		t.Fatalf("a legacy state file must load: %v", err)
	}
	if got := st.Seen["sig-1"].At.UTC().Format("2006-01-02T15:04:05Z"); got != "2026-03-02T10:00:00Z" {
		t.Fatalf("legacy timestamp = %q", got)
	}
	if st.Seen["sig-1"].Fingerprint != "" {
		t.Fatal("a legacy record has no fingerprint")
	}
	dec := b.Ingest(sampleSignal())
	if dec.Accepted {
		t.Fatal("a seen id must still be dropped")
	}
	if dec.Reason != "duplicate signal id" {
		t.Fatalf("an unverifiable legacy record must not be called reuse, got %q", dec.Reason)
	}
}

// TestFingerprintIsFieldSensitiveAndStable is the unit test behind the
// dedupe record.
func TestFingerprintIsFieldSensitiveAndStable(t *testing.T) {
	base := sampleSignal()
	if Fingerprint(base) != Fingerprint(sampleSignal()) {
		t.Fatal("the same payload must fingerprint the same")
	}
	alters := []struct {
		name string
		mut  func(*Signal)
	}{
		{"agent", func(s *Signal) { s.Agent = "momentum" }},
		{"symbol", func(s *Signal) { s.Symbol = "MSFT" }},
		{"side", func(s *Signal) { s.Side = "sell" }},
		{"qty", func(s *Signal) { s.Qty = "11" }},
		{"notional", func(s *Signal) { s.Notional = "501" }},
		{"confidence", func(s *Signal) { s.Confidence = 0.81 }},
		{"field boundary", func(s *Signal) { s.Qty = "1"; s.Notional = "0500" }},
	}
	for _, tc := range alters {
		sig := sampleSignal()
		tc.mut(&sig)
		if Fingerprint(sig) == Fingerprint(base) {
			t.Errorf("%s: a changed payload must change the fingerprint", tc.name)
		}
	}
	// The free-text reason is not part of what would be traded.
	sig := sampleSignal()
	sig.Reason = "a different story entirely"
	if Fingerprint(sig) != Fingerprint(base) {
		t.Error("reason is free text and must not change the fingerprint")
	}
	// Cosmetic noise must not be read as an attack.
	noise := sampleSignal()
	noise.Agent = " Research "
	noise.Symbol = "aapl"
	noise.Side = " BUY"
	if Fingerprint(noise) != Fingerprint(base) {
		t.Error("trim/case must not be reported as a payload change")
	}
}
