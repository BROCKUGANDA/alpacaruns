package ensemble

import (
	"context"
	"fmt"
	"math"
)

// meanrev.go: z-score of close vs the 20-day SMA.
//
//	BUY  when z < -2 (oversold) — long-only entry
//	SELL when z > +2 on a HELD position (overbought exit)
//
// Confidence = min(1, |z|/3). Fires ONLY in a Ranging regime: a strong
// trend makes "oversold" just "falling", so the expert abstains
// (Hold) unless its own regime read says ranging.

const (
	meanRevWindow   = 20 // SMA lookback in sessions
	meanRevBuyZ     = -2.0
	meanRevExitZ    = 2.0
	meanRevMinBars  = 25 // window + slack for a meaningful stdev
	meanRevTrendCap = 0.02
)

// MeanRevExpert is the contrarian voice. Regime is self-assessed: the
// drift of closes over the window must stay inside ±2% per session,
// else the tape is trending and the expert holds fire.
type MeanRevExpert struct{}

// Name implements Expert.
func (MeanRevExpert) Name() string { return "meanrev" }

// Evaluate computes the SMA-20 z-score from shared bars.
func (MeanRevExpert) Evaluate(_ context.Context, symbol string, data MarketData) ([]Signal, error) {
	sd := data.SD(symbol)
	if sd == nil || len(sd.Closes) < meanRevMinBars {
		return []Signal{holdSig(symbol, "meanrev: insufficient history")}, nil
	}
	closes := sd.Closes
	window := closes[len(closes)-meanRevWindow:]
	sma := Mean(window)
	sd20 := Stdev(window)
	if sma <= 0 || sd20 == 0 {
		return []Signal{holdSig(symbol, "meanrev: degenerate distribution")}, nil
	}
	z := (closes[len(closes)-1] - sma) / sd20

	// Self-assessed regime: |drift| over the window beyond ~2%/session
	// means we are trending, not ranging — never fade that.
	drift := math.Abs(window[len(window)-1]/window[0] - 1) / float64(len(window)-1)
	if drift > meanRevTrendCap {
		return []Signal{{Symbol: symbol, Action: ActionHold, Confidence: 0.5,
			Regime: RegimeTrending,
			Reason: fmt.Sprintf("meanrev: drifting %.2f%%/session; not ranging (z=%.2f)", drift*100, z)}}, nil
	}

	switch {
	case !data.Held[symbol] && z < meanRevBuyZ:
		return []Signal{{Symbol: symbol, Action: ActionBuy,
			Confidence: math.Min(1, math.Abs(z)/3), Regime: RegimeRanging,
			Reason: fmt.Sprintf("meanrev BUY z=%.2f < -2 vs SMA%d=%.2f", z, meanRevWindow, sma)}}, nil

	case data.Held[symbol] && z > meanRevExitZ:
		return []Signal{{Symbol: symbol, Action: ActionSell,
			Confidence: math.Min(1, math.Abs(z)/3), Regime: RegimeRanging,
			Reason: fmt.Sprintf("meanrev SELL held z=%.2f > +2 vs SMA%d=%.2f", z, meanRevWindow, sma)}}, nil

	default:
		return []Signal{holdSig(symbol, fmt.Sprintf("meanrev hold z=%.2f", z))}, nil
	}
}

func holdSig(symbol, reason string) Signal {
	return Signal{Symbol: symbol, Action: ActionHold, Confidence: 0.5, Regime: RegimeRanging, Reason: reason}
}

var _ Expert = MeanRevExpert{}
