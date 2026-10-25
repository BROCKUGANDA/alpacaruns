package universe

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/picker"
)

func inst(sym string, class picker.Class) picker.Instrument {
	return picker.Instrument{Symbol: sym, Class: class, Tradable: true}
}

func freshCaps() Caps {
	return Caps{AsOf: time.Now().Add(-time.Hour), Source: "test", Caps: map[string]float64{
		"AAPL": 3.5e12, "TINY": 4.0e8,
	}}
}

// The floor excludes small caps and keeps large ones; funds and crypto are
// exempt by class, because neither has a market cap to screen.
func TestScreenAppliesFloorAndExemptsClasses(t *testing.T) {
	in := []picker.Instrument{
		inst("AAPL", picker.ClassEquity),
		inst("TINY", picker.ClassEquity),
		inst("SPY", picker.ClassETF),
		inst("QQQ", picker.ClassIndex),
		inst("BTC/USD", picker.ClassCrypto),
	}
	kept, excluded := Screen(in, freshCaps(), 2e9, 0, 30*24*time.Hour, time.Now())

	var got []string
	for _, k := range kept {
		got = append(got, k.Symbol)
	}
	want := map[string]bool{"AAPL": true, "SPY": true, "QQQ": true, "BTC/USD": true}
	if len(got) != len(want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	for _, s := range got {
		if !want[s] {
			t.Fatalf("unexpected survivor %s", s)
		}
	}
	if len(excluded) != 1 || excluded[0].Symbol != "TINY" {
		t.Fatalf("expected TINY excluded, got %+v", excluded)
	}
	if excluded[0].Reason == "" {
		t.Fatal("exclusion carried no reason")
	}
}

// Fail closed: a screen that cannot verify a cap must not admit the name.
func TestScreenFailsClosedOnMissingStaleOrUnverifiedCaps(t *testing.T) {
	in := []picker.Instrument{inst("AAPL", picker.ClassEquity), inst("NEWCO", picker.ClassEquity)}

	// Missing entry for NEWCO.
	kept, excluded := Screen(in, freshCaps(), 2e9, 0, 30*24*time.Hour, time.Now())
	if len(excluded) != 1 || excluded[0].Symbol != "NEWCO" {
		t.Fatalf("an unknown cap was admitted: kept=%v excluded=%+v", kept, excluded)
	}

	// A stale table excludes even the names it lists.
	old := freshCaps()
	old.AsOf = time.Now().Add(-90 * 24 * time.Hour)
	kept, excluded = Screen([]picker.Instrument{inst("AAPL", picker.ClassEquity)}, old, 2e9, 0, 7*24*time.Hour, time.Now())
	if len(kept) != 0 || len(excluded) != 1 {
		t.Fatalf("stale caps were trusted: kept=%v excluded=%+v", kept, excluded)
	}
	if want := "cannot verify market cap"; len(excluded[0].Reason) < len(want) || excluded[0].Reason[:len(want)] != want {
		t.Fatalf("stale reason not explained: %q", excluded[0].Reason)
	}

	// No table at all is the same fact.
	empty, excluded := Screen([]picker.Instrument{inst("AAPL", picker.ClassEquity)}, Caps{}, 2e9, 0, 7*24*time.Hour, time.Now())
	if len(empty) != 0 || len(excluded) != 1 {
		t.Fatalf("an empty cap table admitted a name: kept=%v", empty)
	}
}

// A floor of zero disables screening, so a deployment that never configured it
// behaves exactly as before.
func TestScreenDisabledByZeroFloor(t *testing.T) {
	in := []picker.Instrument{inst("TINY", picker.ClassEquity)}
	kept, excluded := Screen(in, Caps{}, 0, 0, 0, time.Now())
	if len(kept) != 1 || len(excluded) != 0 {
		t.Fatalf("disabled screen filtered: kept=%v excluded=%+v", kept, excluded)
	}
}

func TestScreenUpperBound(t *testing.T) {
	kept, excluded := Screen([]picker.Instrument{inst("AAPL", picker.ClassEquity)}, freshCaps(), 2e9, 1e12, 30*24*time.Hour, time.Now())
	if len(kept) != 0 || len(excluded) != 1 {
		t.Fatalf("ceiling did not apply: kept=%v", kept)
	}
}

// EDGAR returns several periods; the cap must use the latest by period end,
// with a later filing superseding an earlier one for the same period.
func TestLatestSharesPicksMostRecentPeriod(t *testing.T) {
	recs := []SharesRecord{
		{End: "2025-10-17", Val: 14_840_000_000, Filed: "2025-10-31", Form: "10-K"},
		{End: "2026-07-17", Val: 14_594_180_000, Filed: "2026-07-31", Form: "10-Q"},
		{End: "2026-07-17", Val: 14_600_000_000, Filed: "2026-08-05", Form: "10-Q/A"},
		{End: "", Val: 1, Filed: "2026-08-06"},
	}
	shares, end, err := LatestShares(recs)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if shares != 14_600_000_000 {
		t.Fatalf("shares = %v, want the re-filed figure for the latest period", shares)
	}
	if end.Format("2006-01-02") != "2026-07-17" {
		t.Fatalf("period end = %v", end)
	}
	if _, _, err := LatestShares(nil); err == nil {
		t.Fatal("empty records produced a share count")
	}
}

func TestCapsRoundTrip(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "trades.jsonl"))
	if c, err := LoadCaps(path); err != nil || len(c.Caps) != 0 {
		t.Fatalf("missing file should read as empty: %+v %v", c, err)
	}
	if err := SaveCaps(path, freshCaps()); err != nil {
		t.Fatalf("save: %v", err)
	}
	back, err := LoadCaps(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if v, ok := back.Get("aapl"); !ok || v != 3.5e12 {
		t.Fatalf("round trip lost data: %+v", back.Caps)
	}
}
