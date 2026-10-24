package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// writeStateFile writes a raw strategy-state.json into the state
// store's directory.
func writeStateFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "strategy-state.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSwingAndIntradayLevelsCoexist proves the two books can hold the
// same symbol at the same time. The swing and intraday books both trade
// the same symbols, so a single flat "symbol -> levels" map let an
// intraday entry silently overwrite the swing book's TP/SL (and the
// reverse), which is exactly the track-isolation contract the README
// claims.
func TestSwingAndIntradayLevelsCoexist(t *testing.T) {
	dir := t.TempDir()
	st := NewStateStore(filepath.Join(dir, "trades.jsonl"))

	if err := st.SetLevel(PositionLevels{Symbol: "AAPL", EntryPrice: 100,
		TakeProfit: 108, StopLoss: 96, Qty: "10"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLevel(PositionLevels{Symbol: "AAPL", EntryPrice: 101,
		TakeProfit: 109, StopLoss: 95, Qty: "5", Track: "intraday"}); err != nil {
		t.Fatal(err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	swing, okSwing := got.Levels[levelKey("AAPL", "")]
	intra, okIntra := got.Levels[levelKey("AAPL", "intraday")]
	if !okSwing || !okIntra {
		t.Fatalf("both books must keep their own levels: %+v", got.Levels)
	}
	if swing.TakeProfit != 108 {
		t.Fatalf("swing take-profit = %v, want 108", swing.TakeProfit)
	}
	if intra.TakeProfit != 109 {
		t.Fatalf("intraday take-profit = %v, want 109", intra.TakeProfit)
	}

	// Clearing one track must leave the other intact.
	if err := st.ClearLevelTrack("AAPL", "intraday"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Load()
	if _, ok := got.Levels[levelKey("AAPL", "intraday")]; ok {
		t.Fatal("intraday level must be gone")
	}
	if _, ok := got.Levels[levelKey("AAPL", "")]; !ok {
		t.Fatal("swing level must survive the intraday clear")
	}
}

// TestMigrateLegacyLevelKeys covers both legacy shapes: the
// "intraday:SYM" prefix and a plain key carrying an intraday track.
func TestMigrateLegacyLevelKeys(t *testing.T) {
	dir := t.TempDir()
	st := NewStateStore(filepath.Join(dir, "trades.jsonl"))
	legacy := `{
      "version": 1,
      "levels": {
        "intraday:AAPL": {"symbol": "AAPL", "entry_price": 100, "take_profit": 108, "stop_loss": 96, "qty": "10"},
        "NVDA": {"symbol": "NVDA", "entry_price": 50, "take_profit": 55, "stop_loss": 48, "qty": "2", "track": "intraday"}
      }
    }`
	writeStateFile(t, dir, legacy)

	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{levelKey("AAPL", "intraday"), levelKey("NVDA", "intraday")} {
		if _, ok := got.Levels[key]; !ok {
			t.Fatalf("legacy level not migrated to %q: %+v", key, got.Levels)
		}
	}
	if _, bad := got.Levels["intraday:AAPL"]; bad {
		t.Fatal("prefixed key must be removed after migration")
	}
}

// TestLevelSymbolIsAlwaysBrokerUsable pins the invariant three call
// sites depend on: the map KEY may carry a track suffix, but
// PositionLevels.Symbol must always be the venue symbol the broker
// accepts. Iterating levels and using the key as a symbol sends
// "AAPL|intraday" to the order endpoint.
func TestLevelSymbolIsAlwaysBrokerUsable(t *testing.T) {
	dir := t.TempDir()
	st := NewStateStore(filepath.Join(dir, "trades.jsonl"))
	if err := st.SetLevel(PositionLevels{Symbol: "AAPL", EntryPrice: 100,
		TakeProfit: 108, StopLoss: 96, Qty: "10", Track: "intraday"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	for key, lv := range got.Levels {
		if lv.Symbol != "AAPL" {
			t.Fatalf("stored symbol must be broker-usable, got %q", lv.Symbol)
		}
		if key == lv.Symbol {
			t.Fatalf("an intraday level must not share the swing book's key: key=%q", key)
		}
		// The key is NOT a symbol for a tracked level — that is the whole
		// point, and why a caller holding the key must not order against
		// it. Assert the distinction so nobody "fixes" it by using the key.
		if tools.SameSymbol(key, lv.Symbol) {
			t.Fatalf("tracked key %q must not resolve as the venue symbol %q", key, lv.Symbol)
		}
	}
}
