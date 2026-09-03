package ensemble

import (
	"context"
	"fmt"

	"github.com/BROCKUGANDA/alpacaruns/factors"
)

// trend.go: the Layer-1 deterministic factor rule, wrapped as an Expert.
// It REUSES factors.Engine through a thin scorer interface so the
// ensemble's trend voice is exactly the existing composite/trend/
// momentum thresholds — one code path, two consumers.

// trendScored is the flat factor triple the trend expert needs. The
// production adapter wraps strategy.FactorScorer-shaped engines; tests
// inject fakes.
type trendScored struct {
	Composite float64
	Trend     float64
	Momentum  float64
}

// TrendScorer produces the flat score for one symbol from SHARED data.
// Implementations may use data.Symbols[symbol] instead of re-fetching.
type TrendScorer interface {
	ScoreFlat(ctx context.Context, symbol string, data MarketData) (trendScored, error)
}

// Thresholds for the trend expert, mirroring strategy.Thresholds
// semantics (inclusive boundaries).
type TrendThresholds struct {
	MinComposite float64 // BUY requires composite >= this
	TrendBuy     float64 // AND trend >= this
	MomentumBuy  float64 // AND momentum >= this
	ExitCompo    float64 // held: composite <= this exits
	ExitMomentum float64 // held: momentum <= this exits
}

// DefaultTrendThresholds matches the paper-trading defaults of
// strategy.Settings so the ensemble's trend voice behaves like the
// Layer-1 engine unless tuned separately via env.
func DefaultTrendThresholds() TrendThresholds {
	return TrendThresholds{MinComposite: 0.6, TrendBuy: 0.7, MomentumBuy: 0.6,
		ExitCompo: 0.4, ExitMomentum: 0.35}
}

// TrendExpert is the wrapped factor-rule voice.
type TrendExpert struct {
	Scorer TrendScorer
	Thresh TrendThresholds
}

// NewTrendExpert validates wiring.
func NewTrendExpert(s TrendScorer, th TrendThresholds) (*TrendExpert, error) {
	if s == nil {
		return nil, fmt.Errorf("trend expert needs a scorer")
	}
	return &TrendExpert{Scorer: s, Thresh: th}, nil
}

// Name implements Expert.
func (e *TrendExpert) Name() string { return "trend" }

// Evaluate applies the exact Layer-1 threshold rule. BUY fires only for
// non-held symbols meeting all three gates; SELL fires only for held
// symbols breaching either exit gate; otherwise HOLD. Confidence is the
// normalized distance above the buy gates (or below the exit gates),
// clamped to [0,1].
func (e *TrendExpert) Evaluate(ctx context.Context, symbol string, data MarketData) ([]Signal, error) {
	fs, err := e.Scorer.ScoreFlat(ctx, symbol, data)
	if err != nil {
		return nil, fmt.Errorf("trend score %s: %w", symbol, err)
	}
	t := e.Thresh
	held := data.Held[symbol]

	switch {
	case !held && fs.Composite >= t.MinComposite && fs.Trend >= t.TrendBuy && fs.Momentum >= t.MomentumBuy:
		depth := (fs.Composite - t.MinComposite + fs.Trend - t.TrendBuy + fs.Momentum - t.MomentumBuy) / 3
		return []Signal{{Symbol: symbol, Action: ActionBuy,
			Confidence: Clamp01(0.5 + depth), Regime: RegimeTrending,
			Reason: fmt.Sprintf("composite=%.2f trend=%.2f momentum=%.2f pass all gates", fs.Composite, fs.Trend, fs.Momentum)}}, nil

	case held && (fs.Composite <= t.ExitCompo || fs.Momentum <= t.ExitMomentum):
		conf := Clamp01(0.5 + (t.ExitCompo-fs.Composite+t.ExitMomentum-fs.Momentum)/2)
		return []Signal{{Symbol: symbol, Action: ActionSell,
			Confidence: conf, Regime: RegimeTrending,
			Reason: fmt.Sprintf("exit: composite=%.2f<=%.2f or momentum=%.2f<=%.2f", fs.Composite, t.ExitCompo, fs.Momentum, t.ExitMomentum)}}, nil

	default:
		return []Signal{{Symbol: symbol, Action: ActionHold, Confidence: 0.5,
			Regime: RegimeTrending,
			Reason: fmt.Sprintf("composite=%.2f trend=%.2f momentum=%.2f no gate", fs.Composite, fs.Trend, fs.Momentum)}}, nil
	}
}

// FactorsScorer adapts *factors.Engine to TrendScorer. The shared bars in
// data are used as a cache seed; Score itself re-fetches per symbol today
// (the engine owns its own bar access), which stays correct because the
// caller still batch-fetches once for every OTHER expert — trend is the
// compatibility voice, not the hot path.
type FactorsScorer struct{ Inner *factors.Engine }

// ScoreFlat flattens Engine.Score to (composite, trend, momentum); a
// missing per-factor key scores 0 so the rule can never BUY on absent
// data.
func (f FactorsScorer) ScoreFlat(ctx context.Context, symbol string, _ MarketData) (trendScored, error) {
	res, err := f.Inner.Score(ctx, symbol)
	if err != nil {
		return trendScored{}, err
	}
	out := trendScored{Composite: res.Composite}
	if tr, ok := res.Factors["trend"]; ok {
		out.Trend = tr.Score
	}
	if mo, ok := res.Factors["momentum"]; ok {
		out.Momentum = mo.Score
	}
	return out, nil
}

// Compile-time interface checks.
var (
	_ Expert      = (*TrendExpert)(nil)
	_ TrendScorer = FactorsScorer{}
)
