package ensemble

import (
	"context"
	"fmt"
	"math"
)

// pairs.go: cointegration-LITE stat-arb for configured pairs, LONG-ONLY
// v1 (Alpaca paper supports shorting equities but the ensemble stays on
// the safe side until the risk module earns shorts).
//
// For pair (A,B): ratio = closeA / closeB; spread z-score over a
// rolling 60-session window of the ratio.
//
//	z < -2  -> A is CHEAP relative to B -> BUY leg A
//	z > +2  -> B is CHEAP relative to A -> BUY leg B
//	|z| < 0.5 with an open pair position -> exit it (convergence)
//
// Confidence scales with |z|. At most ONE concurrent position per pair.

const (
	pairWindow    = 60
	pairEnterZ    = 2.0
	pairExitZ     = 0.5
	pairMinBars   = pairWindow + 1
	pairMaxZConf  = 3.0 // |z| >= 3 maps to confidence 1.0
	pairATRBuffer = 2.0 // entry requires |z|*ratio-stdev > buffer*ATR to matter
)

// Pair is one configured ratio pair.
type Pair struct {
	A string // numerator leg
	B string // denominator leg
}

// PairsExpert trades mean-reversion of pairwise ratios.
type PairsExpert struct {
	Pairs []Pair
}

// Name implements Expert.
func (*PairsExpert) Name() string { return "pairs" }

// Evaluate emits signals for BOTH legs of each configured pair plus a
// convergence-exit Sell for whichever leg is currently held when the
// spread collapses. The gater dedupes per symbol by taking the max-
// weighted vote, and RiskBudget caps concurrent entries per pair via
// the caller's state.
func (e *PairsExpert) Evaluate(_ context.Context, _ string, data MarketData) ([]Signal, error) {
	var out []Signal
	for _, p := range e.Pairs {
		sdA, sdB := data.SD(p.A), data.SD(p.B)
		if sdA == nil || sdB == nil {
			out = append(out, holdSig(p.A, "pairs: missing leg data"))
			continue
		}
		n := min(len(sdA.Closes), len(sdB.Closes))
		if n < pairMinBars {
			out = append(out, holdSig(p.A, fmt.Sprintf("pairs %s/%s: insufficient history", p.A, p.B)))
			continue
		}
		ratio := make([]float64, n)
		for i := 0; i < n; i++ {
			if sdB.Closes[i] == 0 {
				return out, nil // degenerate leg; no signals this tick
			}
			ratio[i] = sdA.Closes[i] / sdB.Closes[i]
		}
		window := ratio[len(ratio)-pairWindow:]
		mean, sd := Mean(window), Stdev(window)
		// Require meaningful dispersion: a flat ratio makes the z-score
		// explode on noise. Below 0.5% coefficient of variation there is
		// no tradeable spread — treat as neutral/converged.
		if mean <= 0 || sd == 0 || sd/mean < 0.005 {
			for _, leg := range []string{p.A, p.B} {
				if data.Held[leg] {
					out = append(out, Signal{Symbol: leg, Action: ActionSell, Confidence: 0.5,
						Regime: RegimeRanging, Reason: fmt.Sprintf("pairs %s/%s: no dispersion, unwinding", p.A, p.B)})
				}
			}
			continue
		}
		z := (ratio[len(ratio)-1] - mean) / sd
		conf := math.Min(1, math.Abs(z)/pairMaxZConf)
		reason := fmt.Sprintf("pairs %s/%s z=%.2f (mean=%.4f sd=%.4f)", p.A, p.B, z, mean, sd)

		switch {
		case z < -pairEnterZ:
			// A cheap vs B: long-only BUY of the cheap leg.
			out = append(out, Signal{Symbol: p.A, Action: ActionBuy, Confidence: conf,
				Regime: RegimeRanging, Reason: reason + " -> buy cheap leg " + p.A})
		case z > pairEnterZ:
			out = append(out, Signal{Symbol: p.B, Action: ActionBuy, Confidence: conf,
				Regime: RegimeRanging, Reason: reason + " -> buy cheap leg " + p.B})
		default:
			// Convergence: exit any open pair-side position.
			for _, leg := range []string{p.A, p.B} {
				if data.Held[leg] && math.Abs(z) < pairExitZ {
					out = append(out, Signal{Symbol: leg, Action: ActionSell, Confidence: conf,
						Regime: RegimeRanging, Reason: reason + " -> converged, exit"})
				}
			}
		}
	}
	return out, nil
}


// WholeUniverse marks this expert cross-sectional: invoked once per tick.
func (*PairsExpert) WholeUniverse() bool { return true }

var _ Expert = (*PairsExpert)(nil)
