package learning

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	c := DefaultConfig()
	c.Enabled = true
	c.MinTrades = 4
	c.EveryTrades = 2
	c.MinSamplesPerClass = 4
	c.MinSamplesPerBucket = 2
	c.ScoreBuckets = 5
	return c
}

func mustEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func sample(score, pct float64) Sample {
	return Sample{
		Symbol: "AAPL", Class: "equity", Score: score,
		PnL: pct * 100, Pct: pct, Hold: time.Hour,
		Closed: time.Now().UTC(),
	}
}

func samples(n int, score, pct float64) []Sample {
	out := make([]Sample, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, sample(score, pct))
	}
	return out
}

// Below the trade bar the learner must say nothing and change nothing.
func TestCalibrateIdleUntilMinTrades(t *testing.T) {
	e := mustEngine(t)
	got := e.Calibrate(samples(3, 0.7, -0.02), 0.5, time.Now().UTC())
	if got.Active {
		t.Fatal("active with fewer samples than MinTrades")
	}
	if got.ScoreFloorAdd != 0 || len(got.Quarantined) != 0 {
		t.Fatalf("inactive snapshot still moved something: %+v", got)
	}
	if got.SuggestedFloor != 0.5 {
		t.Fatalf("inactive snapshot rewrote the operator floor: %v", got.SuggestedFloor)
	}
	if got.SizeMultiplierFor("equity") != 1.0 {
		t.Fatal("inactive snapshot changed sizing")
	}
}

// Configured off: evidence may accumulate, nothing may be applied.
func TestCalibrateDisabledAppliesNothing(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	got := e.Calibrate(samples(20, 0.7, -0.05), 0.5, time.Now().UTC())
	if got.Active {
		t.Fatal("disabled learner reported an active snapshot")
	}
	if got.ScoreFloorAdd != 0 {
		t.Fatalf("disabled learner raised the floor anyway: %v", got.ScoreFloorAdd)
	}
	if got.SamplesSeen != 20 {
		t.Fatalf("samples not recorded: %d", got.SamplesSeen)
	}
}

// A configuration that would let learning grow a position is rejected at
// construction, not silently clamped at the trade.
func TestConfigRejectsGrowthMultipliers(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{"size above one", func(c *Config) { c.MinSizeMultiplier = 1.5 }},
		{"tighten above one", func(c *Config) { c.MaxTightenFactor = 2 }},
		{"zero min trades", func(c *Config) { c.MinTrades = 0 }},
		{"one bucket", func(c *Config) { c.ScoreBuckets = 1 }},
	} {
		cfg := DefaultConfig()
		tc.mut(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate accepted an unsafe config", tc.name)
		}
		if _, err := NewEngine(cfg); err == nil {
			t.Errorf("%s: NewEngine accepted an unsafe config", tc.name)
		}
	}
}

// Unsourced scores must not become evidence: a hand-placed fill has no
// score, and filing it at 0.00 would teach the learner that the lowest
// bucket loses — then it would raise its own floor to escape a number it
// invented.
func TestUnscoredSamplesAreNotCalibrationEvidence(t *testing.T) {
	e := mustEngine(t)
	var s []Sample
	for i := 0; i < 12; i++ {
		x := sample(0, -0.05) // losing, but no score on the entry
		s = append(s, x)
	}
	got := e.Calibrate(s, 0.5, time.Now().UTC())
	if !got.Active {
		t.Fatal("expected an active snapshot (enough samples)")
	}
	for _, b := range got.Buckets {
		if b.Samples != 0 {
			t.Fatalf("unscored trades entered the calibration table: %+v", got.Buckets)
		}
	}
	if got.ScoreFloorAdd != 0 {
		t.Fatalf("floor raised from fabricated evidence: %v", got.ScoreFloorAdd)
	}
}

// The floor may rise above the operator's bar, never below it, and never
// by more than one bounded step.
func TestFloorOnlyRisesAndIsBounded(t *testing.T) {
	e := mustEngine(t)
	// Every scored trade loses, spread across the range above 0.2: the
	// learner should lift the bar, but by at most maxSingleUplift.
	var s []Sample
	for _, sc := range []float64{0.25, 0.35, 0.45, 0.55, 0.65, 0.75} {
		s = append(s, sample(sc, -0.04), sample(sc, -0.03))
	}
	got := e.Calibrate(s, 0.2, time.Now().UTC())
	if got.ScoreFloorAdd <= 0 {
		t.Fatalf("expected an uplift for a uniformly losing record, got %v", got)
	}
	if got.ScoreFloorAdd > 0.15 {
		t.Fatalf("uplift %v exceeds the single-step bound", got.ScoreFloorAdd)
	}
	if got.SuggestedFloor < 0.2 {
		t.Fatalf("suggested floor %v is below the configured 0.2", got.SuggestedFloor)
	}
	// A floor already at the top cannot be raised past the bound either.
	high := e.Calibrate(s, 0.98, time.Now().UTC())
	if high.ScoreFloorAdd > 0.15 {
		t.Fatalf("uplift unbounded at a high base floor: %v", high.ScoreFloorAdd)
	}
}

// A healthy class keeps the operator's full size.
func TestHealthyClassKeepsFullSize(t *testing.T) {
	e := mustEngine(t)
	var s []Sample
	for _, sc := range []float64{0.6, 0.7, 0.8, 0.9} {
		s = append(s, sample(sc, 0.03))
	}
	got := e.Calibrate(s, 0.5, time.Now().UTC())
	adj, ok := got.Classes["equity"]
	if !ok {
		t.Fatalf("no class adjustment recorded: %+v", got.Classes)
	}
	if adj.SizeMultiplier != 1.0 || adj.StopMultiplier != 1.0 {
		t.Fatalf("winning class was penalised: %+v", adj)
	}
}

// A losing class shrinks, and the shrink is clamped to the configured
// floor rather than to zero.
func TestLosingClassShrinksWithinBounds(t *testing.T) {
	cfg := testConfig()
	cfg.MinSizeMultiplier = 0.4
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	var s []Sample
	for i := 0; i < 10; i++ {
		s = append(s, sample(0.7, -0.06))
	}
	got := e.Calibrate(s, 0.5, time.Now().UTC())
	adj := got.Classes["equity"]
	if adj.SizeMultiplier >= 1.0 {
		t.Fatalf("a class losing 6%% a trade kept full size: %+v", adj)
	}
	if adj.SizeMultiplier < 0.4 {
		t.Fatalf("shrink breached the configured floor: %v", adj.SizeMultiplier)
	}
	if adj.StopMultiplier < 0.75 || adj.StopMultiplier > 1 {
		t.Fatalf("stop factor outside the tighten bound: %v", adj.StopMultiplier)
	}
	if got.SizeMultiplierFor("option") != 1.0 {
		t.Fatal("a class with no samples got an opinion")
	}
}

// Below the per-class sample bar the learner has no evidence, and no
// evidence must mean no change.
func TestClassBelowSampleBarIsUntouched(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamplesPerClass = 8
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// Six trades: enough for the engine to be active, not enough to hold
	// an opinion about the class.
	got := e.Calibrate(samples(6, 0.7, -0.06), 0.5, time.Now().UTC())
	adj := got.Classes["equity"]
	if adj.Samples >= cfg.MinSamplesPerClass {
		t.Fatalf("test no longer exercises the bar: %+v", adj)
	}
	if adj.SizeMultiplier != 1 || adj.StopMultiplier != 1 {
		t.Fatalf("under-evidenced class was adjusted: %+v", adj)
	}
	if !strings.Contains(adj.Reason, "below") {
		t.Fatalf("under-evidenced class did not record why it was untouched: %+v", adj)
	}
}

// Quarantine needs both the trade count and a losing record.
func TestQuarantineNeedsVolumeAndLosingRecord(t *testing.T) {
	e := mustEngine(t)
	var s []Sample
	for i := 0; i < e.Config().QuarantineTrades-1; i++ {
		x := sample(0.7, -0.05)
		x.Symbol = "NVDA"
		s = append(s, x)
	}
	// A symbol that has closed enough trades to judge, and lost nearly
	// all of them.
	for i := 0; i < e.Config().QuarantineTrades; i++ {
		x := sample(0.7, -0.04)
		x.Symbol = "TSLA"
		if i == 0 {
			x.PnL, x.Pct = 10, 0.01 // one win out of six: 17% win rate
		}
		s = append(s, x)
	}

	got := e.Calibrate(s, 0.5, time.Now().UTC())
	if _, ok := got.IsQuarantined("NVDA"); ok {
		t.Fatalf("quarantined a symbol with fewer trades than the bar %d: %+v",
			e.Config().QuarantineTrades, got.Quarantined)
	}
	if _, ok := got.IsQuarantined("TSLA"); !ok {
		t.Fatalf("a six-trade 17%% win-rate symbol was not quarantined: %+v", got.Quarantined)
	}
	// A clear winner is never quarantined, however busy its tape.
	var wins []Sample
	for i := 0; i < 12; i++ {
		x := sample(0.7, 0.02)
		x.Symbol = "MSFT"
		wins = append(wins, x)
	}
	if _, ok := e.Calibrate(wins, 0.5, time.Now().UTC()).IsQuarantined("MSFT"); ok {
		t.Fatal("a profitable symbol was quarantined")
	}
}

// Every read path re-clamps: a snapshot with a corrupted multiplier must
// not be able to scale a trade up, even if it got to disk that way.
func TestMultipliersAreClampedOnRead(t *testing.T) {
	adj := Adjustments{
		Active: true, ScoreFloorAdd: -0.3,
		Classes: map[string]ClassAdjustment{
			"equity": {Class: "equity", SizeMultiplier: 3.5, StopMultiplier: -1},
			"crypto": {Class: "crypto", SizeMultiplier: 0, StopMultiplier: math.NaN()},
		},
	}
	if got := adj.SizeMultiplierFor("equity"); got != 1.0 {
		t.Fatalf("3.5x slipped through: %v", got)
	}
	if got := adj.StopMultiplierFor("equity"); got != 1.0 {
		t.Fatalf("negative stop factor slipped through: %v", got)
	}
	if got := adj.SizeMultiplierFor("crypto"); got != 1.0 {
		t.Fatalf("zero multiplier slipped through: %v", got)
	}
	if got := adj.StopMultiplierFor("crypto"); got != 1.0 {
		t.Fatalf("NaN stop factor slipped through: %v", got)
	}
}

// Classes and symbols are matched case- and space-insensitively on the
// query side: entries arrive from the broker ("BTC/USD", " CRYPTO ") while
// the snapshot keys are normalised at write time.
func TestLookupIsCaseAndSpaceTolerant(t *testing.T) {
	adj := Adjustments{
		Active:      true,
		Classes:     map[string]ClassAdjustment{"crypto": {SizeMultiplier: 0.5}},
		Quarantined: []Quarantined{{Symbol: "AAPL", Samples: 8, WinRate: 0.1}},
	}
	if got := adj.SizeMultiplierFor(" CRYPTO "); got != 0.5 {
		t.Fatalf("class lookup failed: %v", got)
	}
	if _, ok := adj.IsQuarantined(" aapl "); !ok {
		t.Fatal("quarantine lookup failed")
	}
}

// ---- store ----

func TestStoreRoundTripAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(filepath.Join(dir, "trades.jsonl"))
	if st.Path() != filepath.Join(dir, "learning-state.json") {
		t.Fatalf("unexpected path %q", st.Path())
	}
	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load on a missing file: %v", err)
	}
	if got.Active || got.Classes == nil {
		t.Fatalf("missing file should read as inactive with a usable map: %+v", got)
	}
	want := Adjustments{
		Active: true, SamplesSeen: 20, ScoreFloorAdd: 0.05, SuggestedFloor: 0.55,
		Classes: map[string]ClassAdjustment{
			"equity": {Class: "equity", Samples: 20, SizeMultiplier: 0.6, StopMultiplier: 0.8},
		},
		Quarantined: []Quarantined{{Symbol: "TSLA", Samples: 9, WinRate: 0.11}},
	}
	if err := st.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.ScoreFloorAdd != 0.05 || back.SizeMultiplierFor("equity") != 0.6 {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if _, ok := back.IsQuarantined("TSLA"); !ok {
		t.Fatal("quarantine list did not survive the round trip")
	}
}

// A hand-edited state file must not be able to authorise more risk than
// the operator configured.
func TestStoreReclampsTamperedFile(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(filepath.Join(dir, "trades.jsonl"))
	raw := map[string]any{
		"active":          true,
		"score_floor_add": -0.5,
		"suggested_floor": 0.0,
		"classes": map[string]any{
			"equity": map[string]any{"class": "equity", "size_multiplier": 4, "stop_multiplier": 9},
		},
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(st.Path(), b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ScoreFloorAdd != 0 {
		t.Fatalf("a negative floor offset survived: %v", got.ScoreFloorAdd)
	}
	if got.SizeMultiplierFor("equity") != 1.0 {
		t.Fatalf("a 4x size multiplier survived: %v", got.SizeMultiplierFor("equity"))
	}
	if got.StopMultiplierFor("equity") != 1.0 {
		t.Fatalf("a 9x stop multiplier survived: %v", got.StopMultiplierFor("equity"))
	}
}

func TestStoreRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(filepath.Join(dir, "trades.jsonl"))
	if err := os.WriteFile(st.Path(), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := st.Load(); err == nil {
		t.Fatal("corrupt state file read back without error")
	}
}

// The calibration table is the audit trail; it must cover the whole range
// and stay ordered.
func TestBucketizeCoversUnitRange(t *testing.T) {
	e := mustEngine(t)
	got := e.Calibrate(samples(6, 0.7, -0.02), 0.5, time.Now().UTC())
	if len(got.Buckets) != e.Config().ScoreBuckets {
		t.Fatalf("bucket count %d != configured %d", len(got.Buckets), e.Config().ScoreBuckets)
	}
	if got.Buckets[0].From != 0 || got.Buckets[len(got.Buckets)-1].To != 1 {
		t.Fatalf("buckets do not span [0,1]: %+v", got.Buckets)
	}
	for i := 1; i < len(got.Buckets); i++ {
		if got.Buckets[i].From != got.Buckets[i-1].To {
			t.Fatalf("buckets are not contiguous at %d: %+v", i, got.Buckets)
		}
	}
}

// Outcomes are decided on dollars OR percentage, so a break-even fill is
// not silently counted as a win.
func TestSampleOutcome(t *testing.T) {
	if sample(0.5, 0).Outcome() != "loss" {
		t.Fatal("a zero-return trade must not read as a win")
	}
	s := Sample{Pct: -0.01, PnL: 5}
	if s.Outcome() != "win" {
		t.Fatal("positive dollars with a negative percentage is still a win")
	}
}
