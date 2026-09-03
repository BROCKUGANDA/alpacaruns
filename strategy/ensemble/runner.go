package ensemble

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/factors"
)

// runner.go: the ensemble orchestrator. One Run call = one tick:
//
//	bars fetched ONCE -> MarketData built for every expert ->
//	vol-regime assessment -> all experts Evaluate -> gater stacks
//	votes -> tracker records/resolves performance.
//
// The runner is pure plumbing; every decision lives in the experts,
// gater and RiskBudget, each unit-testable in isolation.

// RunnerConfig wires the full Layer-2 stack.
type RunnerConfig struct {
	Cfg            Config      // parsed ENSEMBLE_* knobs
	Scorer         TrendScorer // trend voice (factors.Engine adapter or fake)
	PositionPct    float64     // legacy fixed-fractional cap (strategy.Settings)
	MaxPositionUSD float64     // legacy notional cap (config.Config)
	Thresholds     TrendThresholds // trend expert gates; if zero, DefaultTrendThresholds applies
	Log            *log.Logger
}

// Runner executes the ensemble per tick.
type Runner struct {
	cfg     Config
	experts []Expert
	gater   *Gater
	risk    *RiskBudget
	tracker *Tracker
	log     *log.Logger
}

// NewRunner assembles experts in fixed order (trend, meanrev, breakout,
// pairs, xsmom, seasonality) so journal output is deterministic.
func NewRunner(rc RunnerConfig) (*Runner, error) {
	if rc.Scorer == nil {
		return nil, fmt.Errorf("ensemble runner needs a trend scorer")
	}
	tth := rc.Thresholds
	if tth.MinComposite == 0 && tth.TrendBuy == 0 && tth.MomentumBuy == 0 {
		tth = DefaultTrendThresholds()
	}
	trend, err := NewTrendExpert(rc.Scorer, tth)
	if err != nil {
		return nil, err
	}
	pairsExp := &PairsExpert{Pairs: rc.Cfg.Pairs}
	xs := NewXSMomExpert()

	gcfg := GaterConfig{MinConfidence: rc.Cfg.MinConfidence, Log: rc.Log}
	rbCfg := DefaultRiskConfig()
	rbCfg.RiskPctPerTrade = rc.Cfg.RiskPct
	rbCfg.MaxCorrelation = rc.Cfg.MaxCorr
	rbCfg.LiquidityPct = rc.Cfg.LiquidityPct
	rbCfg.PositionPct = rc.PositionPct
	rbCfg.MaxPositionUSD = rc.MaxPositionUSD

	return &Runner{
		cfg: rc.Cfg,
		experts: []Expert{trend, &MeanRevExpert{}, BreakoutExpert{}, pairsExp, xs, &SeasonalityExpert{}},
		gater:   NewGater(gcfg, nil),
		risk:    NewRiskBudget(rbCfg),
		tracker: NewTracker(rc.Cfg.PerfWindow, ""),
		log:     rc.Log,
	}, nil
}

// AttachTracker wires persistence into the runner's tracker (call after
// NewRunner with the state path derived from TRADE_LOG).
func (r *Runner) AttachTracker(path string) error {
	r.tracker = NewTracker(r.cfg.PerfWindow, path)
	if err := r.tracker.Load(); err != nil {
		return fmt.Errorf("ensemble state load: %w", err)
	}
	r.gater = NewGater(GaterConfig{MinConfidence: r.cfg.MinConfidence, Log: r.log}, r.tracker)
	return nil
}

// BuildMarketData batch-fetches bars ONCE and derives per-symbol data.
// benchmark (e.g. SPY) joins the fetch list for the vol-regime module;
// its dataset stays available to experts too. held marks open positions.
func (r *Runner) BuildMarketData(ctx context.Context, src factors.BarSource, universe []string, held map[string]bool, now time.Time) (MarketData, error) {
	syms := append([]string(nil), universe...)
	if b := strings.ToUpper(strings.TrimSpace(r.cfg.Benchmark)); b != "" {
		var have bool
		for _, s := range syms {
			if strings.EqualFold(s, b) {
				have = true
			}
		}
		if !have {
			syms = append(syms, b)
		}
	}
	allBars, err := src.GetBars(ctx, syms, "1Day", "", "", 200)
	if err != nil {
		return MarketData{}, fmt.Errorf("ensemble bars: %w", err)
	}
	md := MarketData{Symbols: map[string]*SymbolData{}, Held: held, Now: now}
	for sym, bars := range allBars {
		SortBars(bars)
		closes := Closes(bars)
		sd := &SymbolData{Bars: bars, Closes: closes}
		if a, ok := ATR(bars, 14); ok {
			sd.ATR14 = a
		}
		rets := Returns(closes)
		if len(rets) > corrWindow {
			rets = rets[len(rets)-corrWindow:]
		}
		sd.RealizedVol = Stdev(rets)
		md.Symbols[sym] = sd
	}
	md = md.WithBenchmark(r.cfg.Benchmark)
	return md, nil
}

// Decision is one executable ensemble outcome for auto.go.
type Decision struct {
	Symbol     string
	Action     Action
	Qty        int    // 0 for SELL (executor resolves) or blocked BUYs
	Confidence float64
	Reason     string
	Votes      []VoiceVote
	Blocked    bool
	BlockWhy   string
}

// Run executes one full ensemble pass over the universe. Verdicts are
// risk-checked and returned sorted by symbol; Hold verdicts are dropped.
func (r *Runner) Run(ctx context.Context, md MarketData, pfv float64) []Decision {
	// Vol-regime first: it reweights every voice below.
	regime := AssessVolRegime(md)

	signalsByExpert := map[string][]Signal{}
	for _, e := range r.experts {
		var sigs []Signal
		var err error
		if ue, ok := e.(UniverseExpert); ok && ue.WholeUniverse() {
			// Cross-sectional voices rank the entire dataset in one call.
			sigs, err = e.Evaluate(ctx, "", md)
		} else {
			// Symbol-scoped voices run once per dataset symbol.
			for _, sym := range md.Universe() {
				ss, serr := e.Evaluate(ctx, sym, md)
				if serr != nil {
					err = serr
					break
				}
				sigs = append(sigs, ss...)
			}
		}
		if err != nil {
			r.logf("[ensemble] expert %s failed this tick: %v (skipped)", e.Name(), err)
			continue
		}
		for i := range sigs {
			sigs[i].ExpertName = e.Name()
		}
		signalsByExpert[e.Name()] = sigs
	}

	verdicts := r.gater.Gate(signalsByExpert, regime, time.Now())

	out := make([]Decision, 0, len(verdicts))
	for _, v := range verdicts {
		if v.Action != ActionBuy && v.Action != ActionSell {
			continue
		}
		sv := r.risk.Apply(v, pfv, md)
		d := Decision{Symbol: sv.Symbol, Action: sv.Action, Qty: sv.Qty,
			Confidence: sv.Confidence, Reason: sv.Reason, Votes: sv.Votes,
			Blocked: sv.Blocked, BlockWhy: sv.BlockWhy}
		out = append(out, d)

		// Track pending performance only for actionable signals.
		if !sv.Blocked && sv.Action == ActionBuy {
			if sd := md.SD(sv.Symbol); sd != nil && len(sd.Bars) > 0 {
				r.tracker.Record(Signal{Symbol: sv.Symbol, Action: ActionBuy,
					ExpertName: "ensemble:" + v.majority()}, sd.Closes[len(sd.Closes)-1], sd.ATR14, time.Now())
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// ResolvePerformance settles expired pending signals against fresh
// closes and persists the tracker.
func (r *Runner) ResolvePerformance(closes map[string][]float64) {
	if n := r.tracker.Resolve(closes); n > 0 {
		r.logf("[ensemble] resolved %d pending signal(s)", n)
	}
	_ = r.tracker.Save()
}

func (r *Runner) logf(format string, a ...any) {
	if r.log != nil {
		r.log.Printf(format, a...)
		return
	}
	fmt.Printf(format+"\n", a...)
}

// majority names the highest-weight Buy voice for attribution.
func (v Verdict) majority() string {
	best, bestW := "ensemble", -1.0
	for _, vt := range v.Votes {
		if vt.Action == ActionBuy && vt.Weight > bestW {
			best, bestW = vt.Expert, vt.Weight
		}
	}
	return best
}
