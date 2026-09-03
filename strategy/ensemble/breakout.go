package ensemble

import (
	"context"
	"fmt"
	"math"
)

// breakout.go: 20-day Donchian channel breakout with volume
// confirmation.
//
//	BUY  when the close makes a NEW high above the prior 20-day high AND
//	     today's volume > 1.5x its trailing average.
//	SELL a HELD position on the mirrored downside breakdown.
//
// Confidence scales with breakout distance measured in ATRs. The expert
// tags VolExpansion when realized vol is rising vs its own history,
// else Trending.

const (
	breakoutWindow   = 20 // Donchian lookback (prior bars, excluding today)
	breakoutVolMult  = 1.5
	volAvgWindow     = 20 // trailing volume-average window
	breakoutMinBars  = breakoutWindow + volAvgWindow + 1
	breakoutATRsFull = 1.0 // distance >= 1 ATR maps to full confidence
)

// BreakoutExpert is the momentum-continuation voice.
type BreakoutExpert struct{}

// Name implements Expert.
func (BreakoutExpert) Name() string { return "breakout" }

// Evaluate checks Donchian breaks plus volume surge on shared bars.
func (BreakoutExpert) Evaluate(_ context.Context, symbol string, data MarketData) ([]Signal, error) {
	sd := data.SD(symbol)
	if sd == nil || len(sd.Bars) < breakoutMinBars {
		return []Signal{holdSig(symbol, "breakout: insufficient history")}, nil
	}
	bars := sd.Bars
	last := len(bars) - 1

	// Prior N-day extremes EXCLUDING today's bar.
	var hi, lo float64
	for i := last - breakoutWindow; i < last; i++ {
		hi = math.Max(hi, bars[i].High)
		if lo == 0 || bars[i].Low < lo {
			lo = bars[i].Low
		}
	}
	close := bars[last].Close

	// Volume confirmation: today vs trailing average.
	var vsum int64
	for i := last - volAvgWindow; i < last; i++ {
		vsum += bars[i].Volume
	}
	avgVol := float64(vsum) / volAvgWindow
	volOK := avgVol > 0 && float64(bars[last].Volume) > breakoutVolMult*avgVol

	atr := sd.ATR14
	dist := math.Abs(close - hi)
	if math.Abs(close-lo) > dist {
		dist = math.Abs(close - lo)
	}

	regime := RegimeTrending
	if sd.RealizedVol > 0 && atr > 0 && sd.RealizedVol/atr*math.Sqrt(252) > 0.30 {
		// Annualized realized vol above ~30% counts as expansion.
		regime = RegimeVolExpansion
	}

	switch {
	case close > hi && volOK:
		conf := Clamp01(0.5 + dist/(breakoutATRsFull*math.Max(atr, 1e-9))/2)
		return []Signal{{Symbol: symbol, Action: ActionBuy, Confidence: conf, Regime: regime,
			Reason: fmt.Sprintf("breakout BUY close=%.2f > %dd-high=%.2f vol=%.1fx", close, breakoutWindow, hi, float64(bars[last].Volume)/avgVol)}}, nil

	case data.Held[symbol] && close < lo && volOK:
		conf := Clamp01(0.5 + dist/(breakoutATRsFull*math.Max(atr, 1e-9))/2)
		return []Signal{{Symbol: symbol, Action: ActionSell, Confidence: conf, Regime: regime,
			Reason: fmt.Sprintf("breakdown SELL held close=%.2f < %dd-low=%.2f vol=%.1fx", close, breakoutWindow, lo, float64(bars[last].Volume)/avgVol)}}, nil

	default:
		return []Signal{holdSig(symbol, fmt.Sprintf("breakout hold close=%.2f inside [%.2f,%.2f]", close, lo, hi))}, nil
	}
}

var _ Expert = BreakoutExpert{}
