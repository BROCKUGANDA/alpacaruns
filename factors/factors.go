// Package factors implements the multi-factor scoring engine behind the
// pre-trade decision gate. Instead of trusting a single LLM confidence
// number, every candidate symbol is scored across independent market
// factors computed from data the system already ingests (daily bars,
// news headlines). Each factor returns a 0..1 score plus a
// human-readable rationale; the composite is a weighted mean whose
// weights come from config (FACTOR_WEIGHTS).
//
// All data access goes through small interfaces (BarSource, NewsSource)
// so every factor is unit-testable with fakes and the engine itself
// stays decoupled from any particular API client.
package factors

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// BarSource supplies historical bars. *tools.Client satisfies it.
type BarSource interface {
	GetBars(ctx context.Context, symbols []string, timeframe, start, end string, limit int) (map[string][]tools.Bar, error)
}

// NewsSource supplies recent headline items. *tools.Client satisfies it.
type NewsSource interface {
	GetNews(ctx context.Context, symbols []string, limit int) ([]tools.NewsItem, error)
}

// Tunable factor parameters. Zero-value Options picks the defaults.
type Options struct {
	Timeframe    string  // bar timeframe for scoring: "1Day", "15Min", ... (default "1Day")
	MomentumDays int     // lookback in BARS for the N-bar return factor
	VolWindow    int     // number of bar returns used for stdev
	VolThreshold float64 // per-bar return stdev considered "normal"; above ~2x scores 0
}

const (
	DefaultTimeframe    = "1Day"
	DefaultMomentumDays = 10
	DefaultVolWindow    = 20
	DefaultVolThreshold = 0.03 // 3% per-bar stdev is already an unusually hot tape
)

// FactorResult is one factor's verdict: score plus why.
type FactorResult struct {
	Score     float64
	Rationale string
}

// Result is the engine output consumed by the risk gate.
type Result struct {
	Composite float64
	Factors   map[string]FactorResult
	Passed    bool
}

// Scorer produces a multi-factor Result for one symbol.
type Scorer interface {
	Score(ctx context.Context, symbol string) (Result, error)
}

// Engine is the default Scorer: five factors, configured weights.
type Engine struct {
	opts     Options
	weights  map[string]float64
	minScore float64
	bars     BarSource
	news     NewsSource
}

// NewEngine builds an engine from loaded config. bars may double as news;
// news may be nil (sentiment degrades to neutral, never fatal).
func NewEngine(cfg *config.Config, bars BarSource, news NewsSource, opts Options) *Engine {
	if opts.Timeframe == "" {
		opts.Timeframe = DefaultTimeframe
	}
	if opts.MomentumDays <= 0 {
		opts.MomentumDays = DefaultMomentumDays
	}
	if opts.VolWindow <= 0 {
		opts.VolWindow = DefaultVolWindow
	}
	if opts.VolThreshold <= 0 {
		opts.VolThreshold = DefaultVolThreshold
	}
	w := cfg.FactorWeights
	if len(w) == 0 {
		w = config.DefaultFactorWeights
	}
	return &Engine{opts: opts, weights: w, minScore: cfg.FactorMinScore, bars: bars, news: news}
}

// Score computes every factor for symbol and folds them into a weighted
// composite. Any failure fetching the underlying market data is an error
// (the risk gate treats scorer errors as rejections, fail closed).
func (e *Engine) Score(ctx context.Context, symbol string) (Result, error) {
	res := Result{Factors: map[string]FactorResult{}}

	allBars, err := e.bars.GetBars(ctx, []string{symbol}, e.opts.Timeframe, "", "", 100)
	if err != nil {
		return Result{}, fmt.Errorf("factor scoring %s: fetch bars: %w", symbol, err)
	}
	daily := allBars[symbol]
	sort.Slice(daily, func(i, j int) bool { return daily[i].Time < daily[j].Time })
	closes := make([]float64, len(daily))
	for i, b := range daily {
		closes[i] = b.Close
	}

	// Trend and volatility need SMA50 / a 20-return window respectively;
	// without enough history we cannot judge — report an error rather
	// than silently scoring blind.
	const minCloses = 51 // SMA50 + current bar
	if len(closes) < minCloses {
		return Result{}, fmt.Errorf("factor scoring %s: need >= %d daily bars, got %d",
			symbol, minCloses, len(closes))
	}
	res.Factors["trend"] = scoreTrend(closes)
	res.Factors["momentum"] = scoreMomentum(closes, e.opts.MomentumDays)
	res.Factors["volatility"] = scoreVolatility(closes, e.opts.VolWindow, e.opts.VolThreshold)
	res.Factors["volume"] = scoreVolume(daily)

	// Sentiment proxy is best-effort by design: a dead or empty news feed
	// yields a neutral 0.5 and never fails the whole score.
	res.Factors["sentiment"] = scoreSentiment(ctx, e.news, symbol)

	var num, den float64
	for name, fr := range res.Factors {
		w := e.weights[name]
		num += w * fr.Score
		den += w
	}
	if den <= 0 {
		return Result{}, fmt.Errorf("factor scoring %s: all factor weights are zero", symbol)
	}
	res.Composite = num / den
	res.Passed = res.Composite >= e.minScore
	return res, nil
}

// scoreTrend compares the latest close against SMA20/SMA50 of daily
// closes. Above both MAs is a strong uptrend (high score); below both is
// a downtrend (low score); straddling is neutral.
func scoreTrend(closes []float64) FactorResult {
	n := len(closes)
	price := closes[n-1]
	sma20 := mean(closes[n-20:])
	sma50 := mean(closes[n-50:])
	score := 0.5
	switch {
	case price > sma20:
		score += 0.2
	case price < sma20:
		score -= 0.2
	}
	switch {
	case price > sma50:
		score += 0.3
	case price < sma50:
		score -= 0.3
	}
	dir := func(p, ma float64) string {
		switch {
		case p > ma:
			return "above"
		case p < ma:
			return "below"
		default:
			return "at"
		}
	}
	return FactorResult{
		Score: clamp01(score),
		Rationale: fmt.Sprintf("close %.2f is %s SMA20 %.2f and %s SMA50 %.2f",
			price, dir(price, sma20), sma20, dir(price, sma50), sma50),
	}
}

// scoreMomentum maps the N-day return to a score: +10% or more -> 1.0,
// -10% or worse -> 0.0, flat -> 0.5.
func scoreMomentum(closes []float64, days int) FactorResult {
	prev := closes[len(closes)-1-days]
	last := closes[len(closes)-1]
	ret := (last - prev) / prev
	score := clamp01(0.5 + ret*5)
	return FactorResult{
		Score: score,
		Rationale: fmt.Sprintf("%d-day return %+.2f%% (%.2f -> %.2f)",
			days, ret*100, prev, last),
	}
}

// scoreVolatility scores the stdev of daily returns against the
// configured threshold: calm tapes score high, extreme vol scores low.
func scoreVolatility(closes []float64, window int, threshold float64) FactorResult {
	n := len(closes)
	rets := make([]float64, 0, window)
	for i := n - window; i < n; i++ {
		rets = append(rets, (closes[i]-closes[i-1])/closes[i-1])
	}
	sd := stdev(rets)
	score := clamp01(1 - sd/(2*threshold)) // at threshold -> 0.5, at 2x -> 0
	return FactorResult{
		Score: score,
		Rationale: fmt.Sprintf("daily-return stdev %.4f vs threshold %.4f (window %d)",
			sd, threshold, window),
	}
}

// scoreVolume compares the latest day's volume with the trailing 20-day
// average: surging participation lifts the score, thinning volume sinks it.
func scoreVolume(daily []tools.Bar) FactorResult {
	n := len(daily)
	var sum int64
	for _, b := range daily[n-21 : n-1] {
		sum += b.Volume
	}
	avg := float64(sum) / 20
	cur := float64(daily[n-1].Volume)
	ratio := cur / avg
	return FactorResult{
		Score: clamp01(0.5 * ratio),
		Rationale: fmt.Sprintf("volume %d is %.2fx the 20-day average %.0f",
			daily[n-1].Volume, ratio, avg),
	}
}

// scoreSentiment is a deliberately keyword-free news-attention proxy:
// a healthy stream of recent headlines signals active coverage (slight
// positive), silence or a broken feed stays neutral. It NEVER returns an
// error and NEVER panics — news is corroborating evidence only.
func scoreSentiment(ctx context.Context, src NewsSource, symbol string) FactorResult {
	if src == nil {
		return FactorResult{Score: 0.5, Rationale: "no news source configured; neutral"}
	}
	items, err := src.GetNews(ctx, []string{symbol}, 20)
	if err != nil {
		return FactorResult{Score: 0.5, Rationale: fmt.Sprintf("news feed unavailable (%v); neutral", err)}
	}
	if len(items) == 0 {
		return FactorResult{Score: 0.5, Rationale: "no recent headlines; neutral"}
	}
	if len(items) >= 5 {
		return FactorResult{
			Score: 0.55,
			Rationale: fmt.Sprintf("%d recent headlines indicate active coverage; attention-weighted neutral-positive",
				len(items)),
		}
	}
	return FactorResult{
		Score:     0.5,
		Rationale: fmt.Sprintf("%d recent headline(s); thin coverage, neutral", len(items)),
	}
}

func mean(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func stdev(xs []float64) float64 {
	m := mean(xs)
	var ss float64
	for _, x := range xs {
		d := x - m
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(xs)))
}

func clamp01(x float64) float64 {
	return math.Max(0, math.Min(1, x))
}

// ScoreFactors adapts Engine to the risk.FactorScorer interface so the
// risk gate can consume factor results without importing engine details.
func (e *Engine) ScoreFactors(ctx context.Context, symbol string) (risk.FactorResult, error) {
	r, err := e.Score(ctx, symbol)
	if err != nil {
		return risk.FactorResult{}, err
	}
	scores := make(map[string]float64, len(r.Factors))
	rationales := make(map[string]string, len(r.Factors))
	for name, fr := range r.Factors {
		scores[name] = fr.Score
		rationales[name] = fr.Rationale
	}
	return risk.FactorResult{
		Composite:  r.Composite,
		MinScore:   e.minScore,
		Passed:     r.Passed,
		Factors:    scores,
		Rationales: rationales,
	}, nil
}
