package research

// research_test.go pins the four properties that make a backtest worth
// reading: no look-ahead, costs that actually bite, conservative handling of
// an ambiguous bar, and folds that cover the range exactly once.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/factors"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

func baseConfig(from, to time.Time) Config {
	return Config{
		From: from, To: to, Folds: 1,
		NotionalPerTrade: 10000,
		Strategy: Strategy{
			MinScore: 0.0, TrendBuy: 0.0, MomentumBuy: 0.0,
			ExitCompos: -1, ExitMomentum: -1, // never exit on signal
			TPPct: 0.08, SLPct: 0.04, MaxHoldDays: 3,
		},
		Weights: map[string]float64{
			"trend": 1, "momentum": 1, "volatility": 1, "volume": 1,
		},
		FactorOptions: factors.Options{MomentumDays: 10, VolWindow: 20, VolThreshold: 0.03},
	}
}

func mkBar(at time.Time, o, h, l, c float64) tools.Bar {
	return tools.Bar{Time: at.Unix(), Open: o, High: h, Low: l, Close: c, Volume: 1000}
}

// rising builds n ascending daily bars starting at start, +1% a day.
func rising(start time.Time, n int, p0 float64) []tools.Bar {
	out := make([]tools.Bar, n)
	p := p0
	for i := 0; i < n; i++ {
		at := start.AddDate(0, 0, i)
		o := p
		c := p * 1.01
		out[i] = mkBar(at, o, c*1.002, o*0.998, c)
		p = c
	}
	return out
}

func barsOf(b []tools.Bar) map[string][]tools.Bar { return map[string][]tools.Bar{"TEST": b} }

// A signal that exists only on the final bar has no bar to execute on. If the
// replay fills it anyway, it is reading the future.
func TestNoTradeWithoutAnExecutionBar(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 80
	bars := make([]tools.Bar, n)
	for i := 0; i < n; i++ {
		at := start.AddDate(0, 0, i)
		bars[i] = mkBar(at, 100, 100.1, 99.9, 100) // dead flat: trend neutral
	}
	// A single strong up-day at the very end is the only thing that could
	// produce a signal, and there is no next bar to act on.
	last := start.AddDate(0, 0, n-1)
	bars[n-1] = mkBar(last, 100, 120, 100, 118)

	cfg := baseConfig(start.AddDate(0, 0, 70), last.AddDate(0, 0, 10))
	cfg.Strategy.MinScore = 0.55
	cfg.Strategy.TrendBuy = 0.7
	rep, err := Run(barsOf(bars), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Overall.Trades != 0 {
		t.Fatalf("a signal on the last bar produced %d trade(s): the replay executed without a bar to execute on",
			rep.Overall.Trades)
	}
}

// Costs must reduce the result by exactly what the model says, and a strategy
// that only wins before costs must lose after.
func TestCostsAreChargedAndReduceNet(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := rising(start, 120, 100)
	cfg := baseConfig(start.AddDate(0, 0, 60), start.AddDate(0, 0, 115))

	free := cfg
	repFree, err := Run(barsOf(bars), free)
	if err != nil {
		t.Fatalf("free run: %v", err)
	}
	if repFree.Overall.Trades == 0 {
		t.Fatal("test premise: the rising series should produce trades")
	}

	charged := cfg
	charged.Costs = CostModel{SpreadBps: 20, SlippageBps: 10, CommissionPerSide: 1}
	repCharged, err := Run(barsOf(bars), charged)
	if err != nil {
		t.Fatalf("charged run: %v", err)
	}
	if repCharged.Overall.Costs <= 0 {
		t.Fatal("no costs were charged")
	}
	if repCharged.Overall.NetPL >= repFree.Overall.NetPL {
		t.Fatalf("costs did not reduce the result: free %.2f vs charged %.2f",
			repFree.Overall.NetPL, repCharged.Overall.NetPL)
	}
	// The difference is exactly costed: gross is identical across the two.
	if diff := repFree.Overall.GrossPL - repCharged.Overall.GrossPL; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("gross changed with costs: %.6f", diff)
	}
	if diff := repFree.Overall.NetPL - repCharged.Overall.NetPL - repCharged.Overall.Costs; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("net difference (%.2f) does not equal charged costs (%.2f)",
			repFree.Overall.NetPL-repCharged.Overall.NetPL, repCharged.Overall.Costs)
	}
}

// A bar whose range covers both the stop and the target does not say which
// came first. Assuming the target is how a losing system becomes a winning
// backtest.
func TestAmbiguousBarBooksTheStop(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := rising(start, 80, 100)
	// Floors at zero: the entry happens on the first eligible bar, which is
	// index MinBarsForScore-1; execution is at the next bar's open.
	entryIdx := factors.MinBarsForScore
	at := start.AddDate(0, 0, entryIdx)
	entry := 100.0
	// One bar spanning well past both +8% and -4%.
	bars[entryIdx] = mkBar(at, entry, entry*1.30, entry*0.90, entry)

	// The fold starts at the first bar so the earliest eligible entry is the
	// one the crafted bar sits on.
	cfg := baseConfig(start, start.AddDate(0, 0, 79))
	rep, err := Run(barsOf(bars), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Overall.Trades == 0 {
		t.Fatal("expected at least one trade")
	}
	first := rep.Folds[0].Trades[0]
	if first.Reason != "stop" {
		t.Fatalf("an ambiguous bar was booked as %q; the stop must win ties", first.Reason)
	}
	if first.NetPL >= 0 {
		t.Fatalf("the stop hit should lose money, got %.2f", first.NetPL)
	}
}

// Folds must tile the range: no gaps, no overlaps, endpoints exact.
func TestFoldsTileTheRange(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 4, 10, 0, 0, 0, 0, time.UTC)
	folds := splitFolds(from, to, 4)
	if len(folds) != 4 {
		t.Fatalf("want 4 folds, got %d", len(folds))
	}
	if !folds[0].Start.Equal(from) {
		t.Fatalf("first fold starts at %v, want %v", folds[0].Start, from)
	}
	if !folds[len(folds)-1].End.Equal(to) {
		t.Fatalf("last fold ends at %v, want %v", folds[len(folds)-1].End, to)
	}
	for i := 1; i < len(folds); i++ {
		if !folds[i].Start.Equal(folds[i-1].End) {
			t.Fatalf("fold %d starts at %v but fold %d ended at %v", i, folds[i].Start, i-1, folds[i-1].End)
		}
	}
	// A single fold covers everything.
	one := splitFolds(from, to, 1)
	if len(one) != 1 || !one[0].Start.Equal(from) || !one[0].End.Equal(to) {
		t.Fatalf("single fold does not cover the range: %+v", one)
	}
}

// The per-fold report must be separate from the aggregate: averaging is what
// hides a rule that only worked in one regime.
func TestFoldsAreReportedSeparately(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := rising(start, 140, 100)
	cfg := baseConfig(start.AddDate(0, 0, 60), start.AddDate(0, 0, 135))
	cfg.Folds = 3
	rep, err := Run(barsOf(bars), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Folds) != 3 {
		t.Fatalf("want 3 folds, got %d", len(rep.Folds))
	}
	var sum int
	for _, f := range rep.Folds {
		sum += f.Metrics.Trades
	}
	if sum != rep.Overall.Trades {
		t.Fatalf("fold trades (%d) do not add up to the total (%d)", sum, rep.Overall.Trades)
	}
}

// Every run is archived, and repeating the same parameters is called out:
// the tenth tweak of one parameter set is not ten independent experiments.
func TestRegistryRecordsRunsAndFlagsRepetition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := rising(start, 100, 100)
	cfg := baseConfig(start.AddDate(0, 0, 60), start.AddDate(0, 0, 95))

	rep, err := Run(barsOf(bars), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	first, err := AppendRun(path, cfg, rep)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if first.PriorRunsWithSameConfig != 0 {
		t.Fatalf("first run claims %d priors", first.PriorRunsWithSameConfig)
	}
	second, err := AppendRun(path, cfg, rep)
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if second.PriorRunsWithSameConfig != 1 {
		t.Fatalf("repeated parameters were not counted: %d", second.PriorRunsWithSameConfig)
	}
	var flagged bool
	for _, n := range second.Notes {
		if len(n) > 0 && contains(n, "evaluated") {
			flagged = true
		}
	}
	if !flagged {
		t.Fatalf("repetition was not stated: %v", second.Notes)
	}
	all, err := Runs(path)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("registry holds %d runs, want 2", len(all))
	}
	// Different parameters must not be counted as repetitions.
	cfg.Strategy.TPPct = 0.12
	third, err := AppendRun(path, cfg, rep)
	if err != nil {
		t.Fatalf("append 3: %v", err)
	}
	if third.PriorRunsWithSameConfig != 0 {
		t.Fatalf("different parameters were counted as a repeat: %d", third.PriorRunsWithSameConfig)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Invalid configurations must be refused rather than silently producing a
// meaningless report.
func TestConfigValidation(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := barsOf(rising(start, 80, 100))
	base := baseConfig(start, start.AddDate(0, 0, 70))

	bad := []struct {
		name string
		mut  func(*Config)
	}{
		{"no folds", func(c *Config) { c.Folds = 0 }},
		{"no notional", func(c *Config) { c.NotionalPerTrade = 0 }},
		{"tp below sl", func(c *Config) { c.Strategy.TPPct = 0.01 }},
		{"no weights", func(c *Config) { c.Weights = nil }},
		{"backwards range", func(c *Config) { c.From, c.To = c.To, c.From }},
	}
	for _, tc := range bad {
		cfg := base
		tc.mut(&cfg)
		if _, err := Run(bars, cfg); err == nil {
			t.Errorf("%s: Run accepted an invalid configuration", tc.name)
		}
	}
}

// The execution price is the whole argument. A replay that fills at the close
// that produced the signal is running on information it could not have had at
// that price; the only honest fill is the NEXT bar's open.
func TestEntryFillsAtTheNextOpenNotTheSignalClose(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := rising(start, 80, 100)
	// The entry executes on the first in-range eligible bar's successor.
	execIdx := factors.MinBarsForScore
	signalClose := bars[execIdx-1].Close
	gapOpen := signalClose * 1.20 // an opening gap the close did not show
	bars[execIdx] = mkBar(start.AddDate(0, 0, execIdx), gapOpen, gapOpen*1.01, gapOpen*0.99, gapOpen)

	cfg := baseConfig(start, start.AddDate(0, 0, 79))
	rep, err := Run(barsOf(bars), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Overall.Trades == 0 {
		t.Fatal("expected a trade")
	}
	first := rep.Folds[0].Trades[0]
	if diff := first.Entry - gapOpen; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("filled at %.4f; the next bar opened at %.4f (signal close was %.4f) — the replay used the signal bar's price",
			first.Entry, gapOpen, signalClose)
	}
}
