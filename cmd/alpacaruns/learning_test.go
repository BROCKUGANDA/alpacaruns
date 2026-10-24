package main

// learning_test.go covers the loop's side of the learner: when it speaks,
// and exactly what it is allowed to change about a live entry.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/learning"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
)

func learnCfg(mut func(*config.Config)) *config.Config {
	c := &config.Config{
		FactorMinScore:        0.5,
		LearnEnabled:          true,
		LearnMinTrades:        4,
		LearnEveryTrades:      3,
		LearnMinSizeMult:      0.25,
		LearnMaxTightenMult:   0.75,
		LearnQuarantineWinPct: 0.25,
		LearnQuarantineTrades: 6,
		LearnMinPerClass:      4,
		LearnScoreBuckets:     5,
	}
	if mut != nil {
		mut(c)
	}
	return c
}

func losingSamples(n int, score, pct float64, sym string) []learning.Sample {
	out := make([]learning.Sample, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, learning.Sample{
			Symbol: sym, Class: "equity", Score: score,
			PnL: pct * 100, Pct: pct, Hold: time.Hour,
			Closed: time.Now().UTC().Add(time.Duration(i) * time.Minute),
		})
	}
	return out
}

// The operator's bounds must reach the engine; a config key that is
// parsed and then dropped is worse than no key at all.
func TestLearningConfigMapsEveryBound(t *testing.T) {
	cfg := learnCfg(nil)
	cfg.LearnMinTrades = 9
	cfg.LearnEveryTrades = 4
	cfg.LearnMinSizeMult = 0.6
	cfg.LearnMaxTightenMult = 0.8
	cfg.LearnQuarantineWinPct = 0.4
	cfg.LearnQuarantineTrades = 7
	cfg.LearnMinPerClass = 5
	cfg.LearnScoreBuckets = 6
	got := learningConfig(cfg)
	for _, tc := range []struct {
		name string
		got  float64
		want float64
	}{
		{"MinTrades", float64(got.MinTrades), 9},
		{"EveryTrades", float64(got.EveryTrades), 4},
		{"MinSizeMultiplier", got.MinSizeMultiplier, 0.6},
		{"MaxTightenFactor", got.MaxTightenFactor, 0.8},
		{"QuarantineWinRate", got.QuarantineWinRate, 0.4},
		{"QuarantineTrades", float64(got.QuarantineTrades), 7},
		{"MinSamplesPerClass", float64(got.MinSamplesPerClass), 5},
		{"ScoreBuckets", float64(got.ScoreBuckets), 6},
		{"Enabled", boolFloat(got.Enabled), 1},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Nothing is computed until the trade bar is met, and recomputation is
// driven by new closed trades rather than by ticks.
func TestCalibrationCadenceFollowsClosedTrades(t *testing.T) {
	l, err := newLearningLedger(learnCfg(nil), filepath.Join(t.TempDir(), "trades.jsonl"))
	if err != nil {
		t.Fatalf("newLearningLedger: %v", err)
	}
	now := time.Now().UTC()

	if _, changed, err := l.maybeCalibrate(losingSamples(3, 0.7, -0.04, "AAPL"), 0.5, now); err != nil {
		t.Fatalf("maybeCalibrate: %v", err)
	} else if changed {
		t.Fatal("calibrated below MinTrades")
	}
	adj, changed, err := l.maybeCalibrate(losingSamples(4, 0.7, -0.04, "AAPL"), 0.5, now)
	if err != nil {
		t.Fatalf("maybeCalibrate: %v", err)
	}
	if !changed {
		t.Fatal("first calibration at MinTrades did not run")
	}
	if !adj.Active {
		t.Fatalf("snapshot inactive at %d samples: %+v", adj.SamplesSeen, adj)
	}
	// Two more trades: below EveryTrades (3), so the conclusion stands.
	if _, changed, _ := l.maybeCalibrate(losingSamples(6, 0.7, -0.04, "AAPL"), 0.5, now); changed {
		t.Fatal("recomputed before EveryTrades new closed trades arrived")
	}
	if _, changed, _ := l.maybeCalibrate(losingSamples(7, 0.7, -0.04, "AAPL"), 0.5, now); !changed {
		t.Fatal("refused to recompute once EveryTrades new trades arrived")
	}
}

// The snapshot must survive a restart: the point of persisting it is that
// a bot that has learned something does not forget it on deploy.
func TestLedgerRestoresSnapshotAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trades.jsonl")
	cfg := learnCfg(nil)
	l, err := newLearningLedger(cfg, path)
	if err != nil {
		t.Fatalf("newLearningLedger: %v", err)
	}
	adj, changed, err := l.maybeCalibrate(losingSamples(10, 0.6, -0.05, "AAPL"), 0.5, time.Now().UTC())
	if err != nil || !changed {
		t.Fatalf("calibrate: changed=%v err=%v", changed, err)
	}
	if adj.SizeMultiplierFor("equity") >= 1 {
		t.Fatalf("test premise: no shrink was earned: %+v", adj.Classes["equity"])
	}
	back, err := newLearningLedger(cfg, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	restored := back.snapshot()
	if !restored.Active {
		t.Fatal("restored snapshot lost its active flag")
	}
	if got, want := restored.SizeMultiplierFor("equity"), adj.SizeMultiplierFor("equity"); got != want {
		t.Fatalf("restored multiplier %v != saved %v", got, want)
	}
	if back.atSamples != adj.SamplesSeen {
		t.Fatalf("cadence counter reset on restart: %d vs %d", back.atSamples, adj.SamplesSeen)
	}
}

// A ledger built from an unsafe config must fail loudly at startup rather
// than trade under a learner that can grow positions.
func TestNewLedgerRejectsUnsafeConfig(t *testing.T) {
	cfg := learnCfg(nil)
	cfg.LearnMinSizeMult = 1.5
	if _, err := newLearningLedger(cfg, filepath.Join(t.TempDir(), "trades.jsonl")); err == nil {
		t.Fatal("ledger accepted a learner that could size positions up")
	}
}

// An inactive snapshot must not block anything: LEARN_ENABLED=false (or
// too few trades) changes no behaviour at all.
func TestGateIsInertUntilActive(t *testing.T) {
	l, err := newLearningLedger(learnCfg(func(c *config.Config) { c.LearnEnabled = false }),
		filepath.Join(t.TempDir(), "trades.jsonl"))
	if err != nil {
		t.Fatalf("newLearningLedger: %v", err)
	}
	adj, changed, _ := l.maybeCalibrate(losingSamples(10, 0.6, -0.05, "AAPL"), 0.5, time.Now().UTC())
	if !changed {
		t.Fatal("disabled ledger did not record the evidence")
	}
	if adj.Active {
		t.Fatal("disabled ledger reported an active snapshot")
	}
	if ok, why := l.gate("AAPL", 0.1, 0.5); !ok {
		t.Fatalf("inactive snapshot blocked a trade: %s", why)
	}
}

// Once active, the floor it earned has to actually bite — including on
// signals that the configured floor would have waved through.
func TestGateBlocksBelowRaisedFloorAndQuarantinedSymbols(t *testing.T) {
	l, err := newLearningLedger(learnCfg(nil), filepath.Join(t.TempDir(), "trades.jsonl"))
	if err != nil {
		t.Fatalf("newLearningLedger: %v", err)
	}
	// Spread the losses across the range so the calibration table has
	// evidence above the operator's 0.5 floor, and give TSLA enough
	// closed trades to clear the quarantine bar.
	var s []learning.Sample
	s = append(s, losingSamples(4, 0.55, -0.05, "AAPL")...)
	s = append(s, losingSamples(4, 0.65, -0.05, "AAPL")...)
	s = append(s, losingSamples(7, 0.75, -0.05, "TSLA")...)
	adj, changed, err := l.maybeCalibrate(s, 0.5, time.Now().UTC())
	if err != nil || !changed {
		t.Fatalf("calibrate changed=%v err=%v", changed, err)
	}
	if adj.ScoreFloorAdd <= 0 {
		t.Fatalf("test premise: no floor uplift earned: %+v", adj)
	}
	scoreJustAboveConfigured := 0.5 + adj.ScoreFloorAdd/2
	ok, why := l.gate("MSFT", scoreJustAboveConfigured, 0.5)
	if ok {
		t.Fatalf("a score under the raised floor passed: add=%v score=%v", adj.ScoreFloorAdd, scoreJustAboveConfigured)
	}
	if !strings.Contains(why, "raised the entry bar") {
		t.Fatalf("gate reason does not name the floor: %q", why)
	}
	// The quarantined symbol is blocked regardless of its score.
	if _, ok := adj.IsQuarantined("TSLA"); !ok {
		t.Fatalf("test premise: TSLA not quarantined: %+v", adj.Quarantined)
	}
	if ok, why := l.gate("TSLA", 0.99, 0.5); ok {
		t.Fatal("quarantined symbol passed the gate")
	} else if !strings.Contains(why, "quarantine") {
		t.Fatalf("gate reason does not name the quarantine: %q", why)
	}
}

// Sizing shrink and stop tightening must land on the plan, and only on
// the plan fields the learner is allowed to touch.
func TestApplyToPlanShrinksSizeAndTightensStop(t *testing.T) {
	long := &strategy.EntryPlan{
		Symbol: "AAPL", Qty: 10, Price: 100, Budget: 1000,
		Brackets: strategy.BracketLevels{TakeProfit: 110, StopLoss: 95},
	}
	l := &learningLedger{adj: learning.Adjustments{
		Active:  true,
		Classes: map[string]learning.ClassAdjustment{"equity": {Class: "equity", SizeMultiplier: 0.5, StopMultiplier: 0.8}},
	}}
	why, ok := l.applyToPlan(long, "equity")
	if !ok {
		t.Fatalf("a halved position at 5 units refused to trade: %s", why)
	}
	if long.Qty != 5 {
		t.Fatalf("qty not shrunk: %d", long.Qty)
	}
	if long.Budget != 500 {
		t.Fatalf("budget not resized with qty: %v", long.Budget)
	}
	if long.Brackets.StopLoss != 96 {
		t.Fatalf("long stop not tightened toward entry: %v", long.Brackets.StopLoss)
	}
	if long.Brackets.TakeProfit != 110 {
		t.Fatalf("learner moved the take-profit: %v", long.Brackets.TakeProfit)
	}
	if why == "" {
		t.Fatal("an applied adjustment logged nothing")
	}

	// A short's stop sits ABOVE entry; tightening must pull it down.
	short := &strategy.EntryPlan{
		Symbol: "TSLA", Qty: 10, Price: 100,
		Brackets: strategy.BracketLevels{TakeProfit: 90, StopLoss: 110},
	}
	if _, ok := l.applyToPlan(short, "equity"); !ok {
		t.Fatal("short rejected")
	}
	if short.Brackets.StopLoss != 108 {
		t.Fatalf("short stop not tightened: %v", short.Brackets.StopLoss)
	}
	if short.Brackets.TakeProfit != 90 {
		t.Fatalf("short take-profit moved: %v", short.Brackets.TakeProfit)
	}

	// Crypto notional shrinks in dollars, not in whole-unit qty.
	notional := &strategy.EntryPlan{Symbol: "BTC/USD", Crypto: true, Notional: 3000, Price: 60000, Budget: 3000}
	if _, ok := l.applyToPlan(notional, "equity"); !ok {
		t.Fatal("notional plan refused at half of $3000")
	}
	if notional.Notional != 1500 {
		t.Fatalf("notional not shrunk: %v", notional.Notional)
	}
}

// A shrink that erases the position must refuse the trade instead of
// sending a zero-unit order to the broker.
func TestApplyToPlanRefusesWhenShrinkErasesTheTrade(t *testing.T) {
	l := &learningLedger{adj: learning.Adjustments{
		Active:  true,
		Classes: map[string]learning.ClassAdjustment{"equity": {SizeMultiplier: 0.25}},
	}}
	single := &strategy.EntryPlan{Symbol: "AAPL", Qty: 1, Price: 100, Budget: 100}
	why, ok := l.applyToPlan(single, "equity")
	if ok {
		t.Fatal("a one-share position shrank to 0 and was still allowed to trade")
	}
	if why == "" {
		t.Fatal("refusal carried no reason")
	}
	if single.Qty != 1 {
		t.Fatalf("refused plan was still mutated: qty=%d", single.Qty)
	}
	tiny := &strategy.EntryPlan{Symbol: "ETH/USD", Crypto: true, Notional: 3, Price: 3000, Budget: 3}
	if _, ok := l.applyToPlan(tiny, "equity"); ok {
		t.Fatal("a $3 notional shrank to nothing and was allowed through")
	}
}

// No evidence means no opinion means no change.
func TestApplyToPlanIsInertWithoutAnOpinion(t *testing.T) {
	before := strategy.EntryPlan{Symbol: "AAPL", Qty: 10, Price: 100, Budget: 1000,
		Brackets: strategy.BracketLevels{TakeProfit: 110, StopLoss: 95}}
	after := before
	l := &learningLedger{adj: learning.Adjustments{Active: false}}
	if why, ok := l.applyToPlan(&after, "equity"); !ok || why != "" {
		t.Fatalf("inactive snapshot interfered: ok=%v why=%q", ok, why)
	}
	if after.Qty != 10 || after.Brackets.StopLoss != 95 {
		t.Fatalf("inactive snapshot mutated the plan: %+v", after)
	}
	// Active, but this class earned nothing.
	l2 := &learningLedger{adj: learning.Adjustments{Active: true, Classes: map[string]learning.ClassAdjustment{}}}
	if why, ok := l2.applyToPlan(&after, "equity"); !ok || why != "" {
		t.Fatalf("no-opinion class interfered: ok=%v why=%q", ok, why)
	}
	if after.Qty != 10 {
		t.Fatalf("no-opinion class shrank the plan: %d", after.Qty)
	}
}

// A multiplier smuggled in above one (tampered state file) must not be
// able to enlarge a live position.
func TestApplyToPlanCannotEnlargeAPosition(t *testing.T) {
	l := &learningLedger{adj: learning.Adjustments{
		Active:  true,
		Classes: map[string]learning.ClassAdjustment{"equity": {SizeMultiplier: 4, StopMultiplier: 2}},
	}}
	p := strategy.EntryPlan{Symbol: "AAPL", Qty: 10, Price: 100, Budget: 1000,
		Brackets: strategy.BracketLevels{TakeProfit: 110, StopLoss: 90}}
	if _, ok := l.applyToPlan(&p, "equity"); !ok {
		t.Fatal("tampered multiplier refused the trade instead of neutralising itself")
	}
	if p.Qty != 10 {
		t.Fatalf("a 4x multiplier changed qty to %d", p.Qty)
	}
	if p.Brackets.StopLoss != 90 {
		t.Fatalf("a 2x stop factor widened the stop to %v", p.Brackets.StopLoss)
	}
}

// classOf must agree with the ledger's own inference when the picker does
// not know the symbol, and override it when it does.
func TestClassOfPrefersTheRegistry(t *testing.T) {
	l := &autoLoop{}
	if got := l.classOf("SPY"); got != "equity" {
		t.Fatalf("with no registry the symbol-shape floor should answer, got %q", got)
	}
	if got := l.classOf("BTC/USD"); got != "crypto" {
		t.Fatalf("crypto inference: %q", got)
	}
	if got := l.classOf("AAPL260101C00100000"); got != "option" {
		t.Fatalf("option inference: %q", got)
	}
}
