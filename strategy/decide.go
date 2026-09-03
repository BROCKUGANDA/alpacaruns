package strategy

import (
	"fmt"
	"math"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// Signal is the deterministic verdict for one symbol on one tick.
type Signal string

const (
	SignalBuy  Signal = "buy"
	SignalSell Signal = "sell" // exit a held position
	SignalHold Signal = "hold"
)

// Decision is the full deterministic decision record for one symbol.
type Decision struct {
	Symbol    string
	Signal    Signal
	Composite float64
	Trend     float64
	Momentum  float64
	Reason    string
	// Sizing is an optional per-decision override. When set, PlanEntry
	// uses it instead of the engine's default sizing. The auto loop
	// populates this from Settings.SizingFor(symbol) so crypto entries
	// get the larger CRYPTO_* caps and equity entries stay small.
	Sizing Sizing
	// Notional, when true and the symbol is crypto, makes PlanEntry
	// return a USD-notional EntryPlan (fractional fill) instead of a
	// whole-coin quantity. Wired by the auto loop when NOTIONAL_CRYPTO
	// is set and the budget can't buy 1 full coin.
	Notional bool
	// NotionalUSD, when >0, overrides the per-position budget for
	// notional crypto orders. The user sets CRYPTO_NOTIONAL_USD in env
	// to cap each crypto entry to a fixed dollar amount (independent of
	// the equity MAX_POSITION_USD), so multiple crypto positions can
	// coexist with equity positions from a $100k account.
	NotionalUSD float64
}
// Thresholds is the exact, documented decision rule (all values come from
// Settings; defaults in settings.go):
//
//	BUY  fires when: composite >= FactorMinScore AND trend >= TrendBuy
//	                 AND momentum >= MomentumBuy
//	SELL fires for held symbols when: composite <= ExitComposite OR
//	                 momentum <= ExitMomentum
//	else HOLD.
//
// The rule is total and deterministic: identical factor inputs always
// yield an identical signal. Boundary semantics are inclusive on both
// sides (>= / <=) exactly as written here.
type Thresholds struct {
	FactorMinScore float64 // cfg.FactorMinScore (FACTOR_MIN_SCORE)
	TrendBuy       float64 // TREND_BUY
	MomentumBuy    float64 // MOMENTUM_BUY
	ExitComposite  float64 // EXIT_COMPOSITE
	ExitMomentum   float64 // EXIT_MOMENTUM
}

// Decide applies the rule. held marks whether the portfolio already owns
// the symbol (exit signals only apply to held positions).
func Decide(symbol string, held bool, composite, trend, momentum float64, t Thresholds) Decision {
	d := Decision{Symbol: symbol, Composite: composite, Trend: trend, Momentum: momentum}
	switch {
	case !held && composite >= t.FactorMinScore && trend >= t.TrendBuy && momentum >= t.MomentumBuy:
		d.Signal = SignalBuy
		d.Reason = fmt.Sprintf("composite %.3f>=%.2f, trend %.3f>=%.2f, momentum %.3f>=%.2f",
			composite, t.FactorMinScore, trend, t.TrendBuy, momentum, t.MomentumBuy)
	case held && (composite <= t.ExitComposite || momentum <= t.ExitMomentum):
		d.Signal = SignalSell
		d.Reason = fmt.Sprintf("held exit: composite %.3f<=%.2f or momentum %.3f<=%.2f",
			composite, t.ExitComposite, momentum, t.ExitMomentum)
	default:
		d.Signal = SignalHold
		if !held && composite < t.FactorMinScore {
			d.Reason = fmt.Sprintf("composite %.3f below minimum %.2f", composite, t.FactorMinScore)
		} else if !held && trend < t.TrendBuy {
			d.Reason = fmt.Sprintf("trend %.3f below buy threshold %.2f", trend, t.TrendBuy)
		} else if !held && momentum < t.MomentumBuy {
			d.Reason = fmt.Sprintf("momentum %.3f below buy threshold %.2f", momentum, t.MomentumBuy)
		} else if held {
			d.Reason = "held with no exit trigger"
		}
	}
	return d
}

// BracketLevels computes server-side bracket children from an expected
// entry price. Pure math shared by equities (real bracket orders) and
// tests. TP above entry by TPPct, SL below entry by SLPct.
//
// Equities ONLY: per Alpaca's order model, order_class=bracket is not
// supported for crypto orders — crypto positions carry their levels
// locally instead (see state.go and monitor.go).
type BracketLevels struct {
	Entry      float64
	TakeProfit float64
	StopLoss   float64
}

// ATR14 computes the 14-bar average true range over OHLC bars.
// Returns ok=false when fewer than 15 bars are available.
func ATR14(bars []tools.Bar) (float64, bool) {
	if len(bars) < 15 {
		return 0, false
	}
	var sum float64
	for i := len(bars) - 14; i < len(bars); i++ {
		prevClose := bars[i-1].Close
		tr := math.Max(bars[i].High-bars[i].Low,
			math.Max(math.Abs(bars[i].High-prevClose), math.Abs(bars[i].Low-prevClose)))
		sum += tr
	}
	return sum / 14, true
}

// ComputeATRBrackets builds volatility-scaled brackets: TP = entry +
// tpMult x atr, SL = entry - slMult x atr, penny-rounded. Classic swing
// defaults are 3x/1.5x.
func ComputeATRBrackets(entry, atr, tpMult, slMult float64) (BracketLevels, error) {
	if entry <= 0 || atr <= 0 {
		return BracketLevels{}, fmt.Errorf("ATR brackets need positive entry and ATR, got %g/%g", entry, atr)
	}
	if slMult <= 0 || tpMult <= slMult {
		return BracketLevels{}, fmt.Errorf("ATR multiples invalid: need TP > SL > 0, got %g/%g", tpMult, slMult)
	}
	lv := BracketLevels{
		Entry:      entry,
		TakeProfit: round2(entry + tpMult*atr),
		StopLoss:   round2(entry - slMult*atr),
	}
	if lv.StopLoss <= 0 {
		return BracketLevels{}, fmt.Errorf("ATR stop below zero (entry %.4f, atr %.4f, mult %.2f)", entry, atr, slMult)
	}
	return lv, nil
}

// ComputeBrackets returns the OCO levels for an entry price. Returns an
// error when tpPct <= slPct <= 0 would produce inverted/zero levels.
func ComputeBrackets(entry, tpPct, slPct float64) (BracketLevels, error) {
	if entry <= 0 {
		return BracketLevels{}, fmt.Errorf("entry price %g must be positive", entry)
	}
	if slPct <= 0 || tpPct <= slPct {
		return BracketLevels{}, fmt.Errorf("bracket pcts invalid: need TP_PCT > SL_PCT > 0, got %g/%g", tpPct, slPct)
	}
	lv := BracketLevels{
		Entry:      entry,
		TakeProfit: round2(entry * (1 + tpPct)),
		StopLoss:   round2(entry * (1 - slPct)),
	}
	return lv, nil
}

// round2 (defined in monitor.go) pins prices to penny increments —
// Alpaca rejects equity orders priced beyond two decimal places
// ("sub-penny increment" HTTP 422).
func round4(x float64) float64 {
	return math.Round(x*10000) / 10000
}
