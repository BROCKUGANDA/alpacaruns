// Package research answers one question the rest of this repository cannot:
// does the decision rule make money out of sample?
//
// Nothing here places an order, and nothing here is used by the trading loop.
// It replays the SAME scoring code the bot runs (factors.ScoreBars) over
// historical bars and reports what the rule would have produced, with costs
// and without look-ahead. Three properties are enforced by construction and
// pinned by tests:
//
//   - a signal computed from bar N is executed at bar N+1's OPEN. Filling at
//     the close that produced the signal is the classic way to manufacture a
//     backtest that cannot be traded.
//   - if a bar's range covers both the stop and the target, the STOP is
//     assumed to have triggered first. OHLC does not say which came first,
//     and assuming the favourable one turns a losing system into a winning
//     one.
//   - every trade pays the configured spread, slippage and commission. A
//     strategy that is only profitable before costs is not profitable.
//
// Evaluation is chronological walk-forward: the range is cut into folds and
// each fold is reported separately, so a rule that only worked in one regime
// is visible rather than averaged away. There is no parameter search here —
// the parameters are the operator's, and searching them on the same data is
// what the registry exists to expose.
package research

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/factors"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// Strategy is the decision rule under test. It mirrors the live thresholds so
// a result here means something about the bot, not about a research toy.
type Strategy struct {
	MinScore     float64 // composite floor for an entry
	TrendBuy     float64 // trend factor floor
	MomentumBuy  float64 // momentum factor floor
	ExitCompos   float64 // composite at or below which an open position exits
	ExitMomentum float64 // momentum at or below which an open position exits
	TPPct        float64 // take-profit distance
	SLPct        float64 // stop distance
	MaxHoldDays  int     // force an exit after this many bars (0 = no cap)
}

// CostModel is what a round trip pays, in the units a broker charges.
type CostModel struct {
	SpreadBps         float64 // quoted spread, charged half on each side
	SlippageBps       float64 // adverse fill on each side
	CommissionPerSide float64 // flat currency per order
}

// CostFraction returns the fraction of notional one SIDE costs.
func (c CostModel) CostFraction() float64 {
	return (c.SpreadBps/2 + c.SlippageBps) / 10000
}

// Config is one experiment.
type Config struct {
	From, To time.Time
	Folds    int
	// NotionalPerTrade is the fixed stake used to turn percentages into
	// currency. Equal stakes per trade keep the metrics interpretable;
	// compounding would make a single early win dominate the report.
	NotionalPerTrade float64
	Strategy         Strategy
	Weights          map[string]float64
	FactorOptions    factors.Options
	Costs            CostModel
}

// Trade is one completed round trip.
type Trade struct {
	Symbol     string    `json:"symbol"`
	EntryAt    time.Time `json:"entry_at"`
	ExitAt     time.Time `json:"exit_at"`
	Entry      float64   `json:"entry"`
	Exit       float64   `json:"exit"`
	Notional   float64   `json:"notional"`
	GrossPL    float64   `json:"gross_pl"`
	Costs      float64   `json:"costs"`
	NetPL      float64   `json:"net_pl"`
	NetPct     float64   `json:"net_pct"`
	EntryScore float64   `json:"entry_score"`
	Reason     string    `json:"exit_reason"`
	BarsHeld   int       `json:"bars_held"`
}

// Metrics summarises a set of trades.
type Metrics struct {
	Trades  int     `json:"trades"`
	Wins    int     `json:"wins"`
	Losses  int     `json:"losses"`
	WinRate float64 `json:"win_rate"`
	GrossPL float64 `json:"gross_pl"`
	Costs   float64 `json:"costs"`
	NetPL   float64 `json:"net_pl"`
	// NetReturnPct is net P/L over the total staked notional.
	NetReturnPct float64 `json:"net_return_pct"`
	// ExpectancyPct is the mean net percentage per trade.
	ExpectancyPct  float64 `json:"expectancy_pct"`
	MaxDrawdownPct float64 `json:"max_drawdown_pct"`
}

// Fold is one chronological slice of the evaluation.
type Fold struct {
	Index   int       `json:"index"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Trades  []Trade   `json:"trades"`
	Metrics Metrics   `json:"metrics"`
}

// Report is the whole experiment.
type Report struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Folds   []Fold    `json:"folds"`
	Overall Metrics   `json:"overall"`
	// Notes state what the numbers do NOT prove, so a reader is not left to
	// infer that a backtest is evidence of future returns.
	Notes []string `json:"notes"`
}

// ConfigHash identifies the experiment's parameters, for the registry.
func (c Config) Hash() string {
	payload := fmt.Sprintf("%s|%s|%d|%.2f|%+v|%+v|%+v",
		c.From.UTC().Format("2006-01-02"), c.To.UTC().Format("2006-01-02"), c.Folds,
		c.NotionalPerTrade, c.Strategy, c.Weights, c.Costs)
	return shortHash(payload)
}

// Run replays the rule over bars, fold by fold.
//
// bars must be keyed by symbol and contain full history; each fold evaluates
// only its own date range but may read earlier bars as warm-up.
func Run(bars map[string][]tools.Bar, cfg Config) (Report, error) {
	if err := validate(bars, cfg); err != nil {
		return Report{}, err
	}
	folds := splitFolds(cfg.From, cfg.To, cfg.Folds)
	rep := Report{From: cfg.From, To: cfg.To}
	var all []Trade
	for i, f := range folds {
		var trades []Trade
		for sym, series := range bars {
			trades = append(trades, replaySymbol(sym, series, f.Start, f.End, cfg)...)
		}
		sort.Slice(trades, func(a, b int) bool { return trades[a].EntryAt.Before(trades[b].EntryAt) })
		all = append(all, trades...)
		rep.Folds = append(rep.Folds, Fold{Index: i, Start: f.Start, End: f.End, Trades: trades, Metrics: summarise(trades)})
	}
	rep.Overall = summarise(all)
	rep.Notes = []string{
		"Walk-forward folds are reported separately; a rule that only worked in one fold is not a rule.",
		"Signals execute at the NEXT bar's open; a bar that covers both stop and target is booked as a stop.",
		"Costs are the configured spread, slippage and commission per side. No market impact or queue model.",
		"Daily bars cannot show intraday path, borrow, or partial fills. Treat this as a screen, not a validation.",
	}
	return rep, nil
}

func validate(bars map[string][]tools.Bar, cfg Config) error {
	if len(bars) == 0 {
		return fmt.Errorf("research: no bars supplied")
	}
	if cfg.From.IsZero() || cfg.To.IsZero() || !cfg.From.Before(cfg.To) {
		return fmt.Errorf("research: need From before To")
	}
	if cfg.Folds < 1 {
		return fmt.Errorf("research: need at least one fold, got %d", cfg.Folds)
	}
	if cfg.NotionalPerTrade <= 0 {
		return fmt.Errorf("research: NotionalPerTrade must be > 0")
	}
	if cfg.Strategy.SLPct <= 0 || cfg.Strategy.TPPct <= cfg.Strategy.SLPct {
		return fmt.Errorf("research: need TP_PCT > SL_PCT > 0, got %g/%g", cfg.Strategy.TPPct, cfg.Strategy.SLPct)
	}
	if len(cfg.Weights) == 0 {
		return fmt.Errorf("research: no factor weights")
	}
	return nil
}

type fold struct{ Start, End time.Time }

// splitFolds cuts [from, to) into n contiguous, non-overlapping windows.
func splitFolds(from, to time.Time, n int) []fold {
	total := to.Sub(from)
	step := total / time.Duration(n)
	if step <= 0 {
		return []fold{{from, to}}
	}
	out := make([]fold, 0, n)
	for i := 0; i < n; i++ {
		start := from.Add(time.Duration(i) * step)
		end := from.Add(time.Duration(i+1) * step)
		if i == n-1 {
			end = to
		}
		out = append(out, fold{Start: start, End: end})
	}
	return out
}

// replaySymbol walks one series. Signals are computed on bars[:i+1] and acted
// on at bars[i+1].Open.
func replaySymbol(sym string, series []tools.Bar, from, to time.Time, cfg Config) []Trade {
	if len(series) < factors.MinBarsForScore+1 {
		return nil
	}
	sorted := make([]tools.Bar, len(series))
	copy(sorted, series)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time < sorted[j].Time })

	type openPos struct {
		entry, tp, sl float64
		at            time.Time
		barIndex      int
		score         float64
	}
	var pos *openPos
	var out []Trade

	for i := factors.MinBarsForScore - 1; i < len(sorted)-1; i++ {
		bar := sorted[i]
		at := time.Unix(bar.Time, 0).UTC()
		if at.Before(from) {
			continue
		}
		if !at.Before(to) {
			break
		}

		// --- manage an open position on THIS bar's range ---
		if pos != nil {
			next := sorted[i+1]
			exitPrice, reason := 0.0, ""
			held := i - pos.barIndex
			switch {
			case bar.Low <= pos.sl:
				// Conservative: when the range covers both levels, the stop
				// is the one assumed to have been hit first.
				exitPrice, reason = pos.sl, "stop"
			case bar.High >= pos.tp:
				exitPrice, reason = pos.tp, "target"
			case cfg.Strategy.MaxHoldDays > 0 && held >= cfg.Strategy.MaxHoldDays:
				exitPrice, reason = next.Open, "max-hold"
			}
			if reason != "" {
				out = append(out, closeTrade(sym, pos.entry, exitPrice, pos.at, at, pos.score, reason, held, cfg))
				pos = nil
			} else {
				// Signal-based exit, evaluated on the same close and acted on
				// at the next open — identical to the live rule.
				res, err := factors.ScoreBars(sym, sorted[:i+1], cfg.Weights, cfg.FactorOptions, nil)
				if err == nil && (res.Composite <= cfg.Strategy.ExitCompos || factorScore(res, "momentum") <= cfg.Strategy.ExitMomentum) {
					out = append(out, closeTrade(sym, pos.entry, next.Open, pos.at, time.Unix(next.Time, 0).UTC(), pos.score, "signal", held, cfg))
					pos = nil
				}
			}
		}

		// --- entry, only when flat ---
		if pos != nil {
			continue
		}
		res, err := factors.ScoreBars(sym, sorted[:i+1], cfg.Weights, cfg.FactorOptions, nil)
		if err != nil || res.Composite < cfg.Strategy.MinScore {
			continue
		}
		if factorScore(res, "trend") < cfg.Strategy.TrendBuy || factorScore(res, "momentum") < cfg.Strategy.MomentumBuy {
			continue
		}
		next := sorted[i+1]
		entry := next.Open
		if entry <= 0 {
			continue
		}
		pos = &openPos{
			entry: entry,
			tp:    entry * (1 + cfg.Strategy.TPPct),
			sl:    entry * (1 - cfg.Strategy.SLPct),
			at:    time.Unix(next.Time, 0).UTC(),
			// The position is held from the execution bar onward.
			barIndex: i + 1,
			score:    res.Composite,
		}
		// No i++ here: the next iteration is bar i+1, whose range the new
		// position genuinely traded through. Skipping it would ignore a stop
		// hit on the entry bar itself.
	}
	return out
}

func factorScore(r factors.Result, name string) float64 {
	if f, ok := r.Factors[name]; ok {
		return f.Score
	}
	return 0
}

func closeTrade(sym string, entry, exit float64, entryAt, exitAt time.Time, score float64, reason string, held int, cfg Config) Trade {
	notional := cfg.NotionalPerTrade
	qty := notional / entry
	gross := (exit - entry) * qty
	costFrac := cfg.Costs.CostFraction()
	costs := notional*costFrac + exit*qty*costFrac + 2*cfg.Costs.CommissionPerSide
	net := gross - costs
	return Trade{
		Symbol: sym, EntryAt: entryAt, ExitAt: exitAt,
		Entry: entry, Exit: exit, Notional: notional,
		GrossPL: gross, Costs: costs, NetPL: net,
		NetPct: net / notional, EntryScore: score,
		Reason: reason, BarsHeld: held,
	}
}

// summarise computes the fold or overall metrics.
func summarise(trades []Trade) Metrics {
	m := Metrics{Trades: len(trades)}
	if len(trades) == 0 {
		return m
	}
	var staked float64
	var equity, peak, maxDD float64
	// Drawdown on cumulative net P/L, in currency relative to staked capital
	// per trade. Reported as a percentage of the average stake so folds of
	// different sizes are comparable.
	for _, t := range trades {
		m.GrossPL += t.GrossPL
		m.Costs += t.Costs
		m.NetPL += t.NetPL
		staked += t.Notional
		if t.NetPL > 0 {
			m.Wins++
		} else if t.NetPL < 0 {
			m.Losses++
		}
		equity += t.NetPL
		if equity > peak {
			peak = equity
		}
		if dd := peak - equity; dd > maxDD {
			maxDD = dd
		}
	}
	if m.Trades > 0 {
		m.WinRate = float64(m.Wins) / float64(m.Trades)
	}
	if staked > 0 {
		m.NetReturnPct = m.NetPL / staked
		m.MaxDrawdownPct = maxDD / (staked / float64(m.Trades))
	}
	m.ExpectancyPct = m.NetPL / float64(m.Trades) / (staked / float64(m.Trades))
	return m
}

// ---- experiment registry ----

// RunRecord is one archived experiment. Every run is written, including the
// ones that looked bad: a registry that only keeps winners is a marketing
// document.
type RunRecord struct {
	At         time.Time `json:"at"`
	ConfigHash string    `json:"config_hash"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Folds      int       `json:"folds"`
	Overall    Metrics   `json:"overall"`
	PerFold    []Metrics `json:"per_fold"`
	// PriorRunsWithSameConfig counts how many earlier runs share this hash,
	// which is the multiple-testing signal: the tenth tweak of the same
	// parameters is not one experiment.
	PriorRunsWithSameConfig int      `json:"prior_runs_same_config"`
	Notes                   []string `json:"notes,omitempty"`
}

// AppendRun records a run and returns the record (with the prior-run count).
func AppendRun(path string, cfg Config, rep Report) (RunRecord, error) {
	prior, err := Runs(path)
	if err != nil {
		return RunRecord{}, err
	}
	rec := RunRecord{
		At:         time.Now().UTC(),
		ConfigHash: cfg.Hash(),
		From:       cfg.From.UTC().Format("2006-01-02"),
		To:         cfg.To.UTC().Format("2006-01-02"),
		Folds:      len(rep.Folds),
		Overall:    rep.Overall,
		Notes:      rep.Notes,
	}
	for _, f := range rep.Folds {
		rec.PerFold = append(rec.PerFold, f.Metrics)
	}
	for _, p := range prior {
		if p.ConfigHash == rec.ConfigHash {
			rec.PriorRunsWithSameConfig++
		}
	}
	if rec.PriorRunsWithSameConfig > 0 {
		rec.Notes = append(rec.Notes, fmt.Sprintf(
			"this exact parameter set has been evaluated %d time(s) before; repeated evaluation on the same data inflates the best result",
			rec.PriorRunsWithSameConfig))
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return rec, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return rec, err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return rec, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return rec, err
	}
	return rec, nil
}

// Runs reads the registry. A missing file is an empty registry.
func Runs(path string) ([]RunRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []RunRecord
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r RunRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			// A torn tail from a crashed write must not hide the valid rows
			// before it.
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func shortHash(s string) string {
	const prime = 16777619
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime
	}
	return fmt.Sprintf("%08x", h)
}
