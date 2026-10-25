package main

// rename_test.go: a broker-confirmed rename must move stored levels with the
// instrument, not orphan them under a ticker that no longer exists.

import (
	"path/filepath"
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
)

func TestMigrateLevelsMovesProtection(t *testing.T) {
	dir := t.TempDir()
	jpath := filepath.Join(dir, "trades.jsonl")
	j, err := pnl.Open(jpath)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	defer j.Close()
	st := strategy.NewStateStore(jpath)
	l := &autoLoop{cfg: &config.Config{}, journal: j, exec: &strategy.Executor{State: st}}

	if err := st.SetLevel(strategy.PositionLevels{
		Symbol: "FB", EntryPrice: 300, TakeProfit: 330, StopLoss: 285, Qty: "10",
	}); err != nil {
		t.Fatalf("set level: %v", err)
	}
	// A second book for the same symbol must move too.
	if err := st.SetLevel(strategy.PositionLevels{
		Symbol: "FB", Track: "intraday", EntryPrice: 301, TakeProfit: 305, StopLoss: 299, Qty: "1",
	}); err != nil {
		t.Fatalf("set intraday level: %v", err)
	}

	l.migrateLevels("FB", "META")

	if old := st.Level("FB", ""); old != nil {
		t.Fatalf("the old ticker still carries a level: %+v", old)
	}
	if old := st.Level("FB", "intraday"); old != nil {
		t.Fatalf("the old ticker still carries an intraday level: %+v", old)
	}
	moved := st.Level("META", "")
	if moved == nil {
		t.Fatal("the new ticker has no level: protection was orphaned")
	}
	if moved.StopLoss != 285 || moved.TakeProfit != 330 || moved.Qty != "10" {
		t.Fatalf("the level did not survive the move intact: %+v", moved)
	}
	if intra := st.Level("META", "intraday"); intra == nil || intra.StopLoss != 299 {
		t.Fatalf("the intraday book did not move: %+v", intra)
	}
	// A rename with nothing stored is a no-op, not an error.
	l.migrateLevels("NONE", "STILLNONE")
}
