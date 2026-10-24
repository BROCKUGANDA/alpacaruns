package main

// baseline_test.go: the adaptive-sizing baseline must survive the per-tick
// heartbeat, which is what destroyed it before.

import (
	"path/filepath"
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
)

func TestStartingEquitySurvivesHeartbeat(t *testing.T) {
	dir := t.TempDir()
	jpath := filepath.Join(dir, "trades.jsonl")
	j, err := pnl.Open(jpath)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	defer j.Close()
	st := strategy.NewStateStore(jpath)
	l := &autoLoop{
		cfg:     &config.Config{},
		journal: j,
		exec:    &strategy.Executor{State: st},
	}

	l.persistStartingEquity(100000)
	if got := l.startingEquity(); got != 100000 {
		t.Fatalf("baseline not persisted: %v", got)
	}
	// The heartbeat is a full typed Save of the state document. It used to
	// drop the key entirely, so the next tick saw 0 and re-based the
	// baseline on current equity — adaptive sizing was inert forever.
	for i := int64(1); i <= 3; i++ {
		if err := st.Heartbeat(i); err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
		if got := l.startingEquity(); got != 100000 {
			t.Fatalf("baseline lost on heartbeat %d: %v", i, got)
		}
	}
	// And a level write must not disturb it either.
	if err := st.SetLevel(strategy.PositionLevels{Symbol: "AAPL", Qty: "1", StopLoss: 100}); err != nil {
		t.Fatalf("set level: %v", err)
	}
	if got := l.startingEquity(); got != 100000 {
		t.Fatalf("baseline lost on a level write: %v", got)
	}
	// One-shot: a second call must not re-base an existing baseline.
	l.persistStartingEquity(50000)
	if got := l.startingEquity(); got != 100000 {
		t.Fatalf("baseline was re-based by a second call: %v", got)
	}
}

// A non-positive equity is not a baseline; writing it would make the P/L
// ratio meaningless.
func TestPersistStartingEquityIgnoresNonPositive(t *testing.T) {
	dir := t.TempDir()
	jpath := filepath.Join(dir, "trades.jsonl")
	st := strategy.NewStateStore(jpath)
	l := &autoLoop{exec: &strategy.Executor{State: st}}
	l.persistStartingEquity(0)
	l.persistStartingEquity(-10)
	if got := l.startingEquity(); got != 0 {
		t.Fatalf("wrote a non-positive baseline: %v", got)
	}
}

// The dashboard's knobs must actually reach the trading process. They used
// to be written into a file the bot never read.
func TestConfigOverridesReachTheLoop(t *testing.T) {
	dir := t.TempDir()
	jpath := filepath.Join(dir, "trades.jsonl")
	st := strategy.NewStateStore(jpath)
	j, err := pnl.Open(jpath)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	defer j.Close()
	cfg := &config.Config{MaxPositionUSD: 1000, MaxPortfolioPct: 0.1, MinConfidence: 0.5}
	mon := &strategy.PositionMonitor{Settings: strategy.Settings{DailyDD: 0.05, WeeklyDD: 0.1, TotalDD: 0.15}}
	l := &autoLoop{cfg: cfg, journal: j, exec: &strategy.Executor{State: st}, monitor: mon, set: strategy.Settings{DailyDD: 0.05}}

	// No overrides file: env stands.
	l.applyConfigOverrides()
	if cfg.MaxPositionUSD != 1000 {
		t.Fatalf("env cap changed with no overrides file: %v", cfg.MaxPositionUSD)
	}

	path := strategy.ConfigOverridesPath(jpath)
	if err := strategy.SaveConfigOverrides(path, strategy.ConfigOverrides{
		MaxPositionUSD: 2500, MaxPortfolioPct: 0.25, MinConfidence: 0.7, DailyDDHalt: 0.03,
	}); err != nil {
		t.Fatalf("save overrides: %v", err)
	}
	l.applyConfigOverrides()
	if cfg.MaxPositionUSD != 2500 || cfg.MaxPortfolioPct != 0.25 || cfg.MinConfidence != 0.7 {
		t.Fatalf("overrides not applied to cfg: %+v", cfg)
	}
	if mon.Settings.DailyDD != 0.03 {
		t.Fatalf("drawdown halt not applied to the monitor: %+v", mon.Settings)
	}

	// Re-applying the same file is a no-op (mtime gate), and a key left at
	// zero keeps the current value rather than zeroing a cap.
	if err := strategy.SaveConfigOverrides(path, strategy.ConfigOverrides{MaxPositionUSD: 2500}); err != nil {
		t.Fatalf("save overrides: %v", err)
	}
	l.overridesAt = l.overridesAt.Add(-1) // force a re-read despite mtime granularity
	l.applyConfigOverrides()
	if cfg.MaxPositionUSD != 2500 {
		t.Fatalf("cap changed on re-read: %v", cfg.MaxPositionUSD)
	}
	if cfg.MaxPortfolioPct != 0.25 {
		t.Fatalf("an unset key zeroed a cap: %v", cfg.MaxPortfolioPct)
	}
	if mon.Settings.DailyDD != 0.03 {
		t.Fatalf("an unset key zeroed the halt: %v", mon.Settings.DailyDD)
	}
}
