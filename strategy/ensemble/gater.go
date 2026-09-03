package ensemble

import (
	"fmt"
	"log"
	"math"
	"sort"
	"time"
)

// gater.go: performance-weighted stacking vote per symbol.
//
// Each expert voice carries weight = BaseWeight x (0.5 + hitRate), then a
// vol-regime modifier scales contrarian voices (meanrev, pairs) DOWN in
// RisingVol/Crisis and trend voices (trend, breakout) UP — the inverse in
// LowVol. Buy mass vs Hold floor decides; the winner must clear
// MinConfidence or the gater forces Hold. Vote variance beyond
// VarianceBreaker means experts disagree so fundamentally that no trade
// is safe: force Hold and log loudly.

const (
	holdFloor        = 0.5  // hold votes carry half-mass against directional conviction
	minConfDefault   = 0.55 // matches MIN_ENSEMBLE_CONFIDENCE default
	varianceBreaker  = 0.15 // vote-variance threshold forcing Hold
	baseWeightDef    = 1.0
	hitRateMinSample = 5 // below this many resolved samples -> neutral weight
)

// VoiceWeight is one expert's base weight, keyed by expert Name().
type VoiceWeights map[string]float64

// DefaultVoiceWeights returns equal base weights for the standard voices.
func DefaultVoiceWeights() VoiceWeights {
	return VoiceWeights{"trend": 1.0, "meanrev": 1.0, "breakout": 1.0,
		"pairs": 0.75, "xsmom": 1.0, "seasonality": 0.35}
}

// GaterConfig wires the stacking vote.
type GaterConfig struct {
	Base           VoiceWeights // per-expert base weights (defaults applied for missing keys)
	MinConfidence  float64      // winning normalized mass floor (default 0.55)
	VarianceLimit  float64      // vote-variance breaker (default 0.15)
	HoldFloorMass  float64      // hold-side mass multiplier (default 0.5)
	MinHitSamples  int          // resolved samples before hit-rate counts (default 5)
	Log            *log.Logger
}

// Gater aggregates expert signals into one verdict per symbol.
type Gater struct {
	cfg     GaterConfig
	tracker *Tracker
}

// NewGater validates config and wires the tracker.
func NewGater(cfg GaterConfig, tr *Tracker) *Gater {
	if cfg.MinConfidence <= 0 {
		cfg.MinConfidence = minConfDefault
	}
	if cfg.VarianceLimit <= 0 {
		cfg.VarianceLimit = varianceBreaker
	}
	if cfg.HoldFloorMass <= 0 {
		cfg.HoldFloorMass = holdFloor
	}
	if cfg.MinHitSamples <= 0 {
		cfg.MinHitSamples = hitRateMinSample
	}
	if len(cfg.Base) == 0 {
		cfg.Base = DefaultVoiceWeights()
	}
	return &Gater{cfg: cfg, tracker: tr}
}

// Verdict is the gater's final call for one symbol.
type Verdict struct {
	Symbol     string
	Action     Action
	Confidence float64
	Reason     string
	Votes      []VoiceVote // per-expert audit trail
	BuyScore   float64
	HoldScore  float64
	SellScore  float64
	Variance   float64
	Circuit    bool // true when the disagreement breaker forced Hold
}

// VoiceVote records one expert's contribution to a symbol's verdict.
type VoiceVote struct {
	Expert string
	Action Action
	Conf   float64
	Weight float64 // effective weight AFTER hit-rate and regime modifiers
}

// Gate stacks all signals for one tick into per-symbol verdicts,
// applying vol-regime reweighting. Signals are attributed to experts by
// ExpertName. Returns verdicts sorted by symbol.
func (g *Gater) Gate(signalsByExpert map[string][]Signal, regime RegimeAssessment, now time.Time) []Verdict {
	// Collect every distinct symbol with any non-Hold vote plus holds
	// (holds still matter: they contribute to the Hold side).
	perSymbol := map[string][]VoiceVote{}
	for expert, sigs := range signalsByExpert {
		w := g.effectiveWeight(expert, regime)
		for _, s := range sigs {
			perSymbol[s.Symbol] = append(perSymbol[s.Symbol],
				VoiceVote{Expert: expert, Action: s.Action, Conf: Clamp01(s.Confidence), Weight: w})
		}
	}

	syms := make([]string, 0, len(perSymbol))
	for sym := range perSymbol {
		syms = append(syms, sym)
	}
	sort.Strings(syms)

	out := make([]Verdict, 0, len(syms))
	for _, sym := range syms {
		out = append(out, g.gateOne(sym, perSymbol[sym]))
	}
	return out
}

// effectiveWeight = base x (0.5 + hitRate) x regimeModifier(expert).
func (g *Gater) effectiveWeight(expert string, regime RegimeAssessment) float64 {
	base := g.cfg.Base[expert]
	if base == 0 && !g.cfg.Base.Has(expert) {
		base = baseWeightDef
	}
	hr := neutralHitRate
	if g.tracker != nil {
		hr = g.tracker.HitRate(expert, g.cfg.MinHitSamples)
	}
	return base * (holdFloor + hr) * regimeModifier(expert, regime.Level)
}

// regimeModifier implements the vol-regime scaling table:
//
//	RisingVol/Crisis: meanrev,pairs x0.5/x0; trend,breakout x1.25/x1.5
//	LowVol:           meanrev,pairs boosted x1.25; trend,breakout x0.75
func regimeModifier(expert string, level VolLevel) float64 {
	contrarian := expert == "meanrev" || expert == "pairs"
	trendy := expert == "trend" || expert == "breakout"
	switch level {
	case VolRising:
		if contrarian {
			return 0.5
		}
		if trendy {
			return 1.25
		}
	case VolCrisis:
		if contrarian {
			return 0
		}
		if trendy {
			return 1.5
		}
	default: // VolLow
		if contrarian {
			return 1.25
		}
		if trendy {
			return 0.75
		}
	}
	return 1.0
}

func (g *Gater) gateOne(sym string, votes []VoiceVote) Verdict {
	v := Verdict{Symbol: sym, Votes: votes}

	var buyMass, sellMass, holdMass float64
	var confs []float64
	for _, vt := range votes {
		mass := vt.Weight * vt.Conf
		switch vt.Action {
		case ActionBuy:
			buyMass += mass
		case ActionSell:
			sellMass += mass
		default:
			holdMass += vt.Weight * g.cfg.HoldFloorMass
		}
		confs = append(confs, vt.Conf)
	}
	v.BuyScore, v.SellScore, v.HoldScore = buyMass, sellMass, holdMass
	v.Variance = variance(confs)

	// Disagreement circuit breaker, two triggers:
	//   1. confidence variance across voices beyond the limit, OR
	//   2. genuine directional split: both Buy and Sell carry mass and
	//      the larger side is under 2x the smaller (no dominance).
	split := buyMass > 0 && sellMass > 0 &&
		math.Max(buyMass, sellMass) < 2*math.Min(buyMass, sellMass)
	if v.Variance > g.cfg.VarianceLimit || split {
		v.Action = ActionHold
		v.Circuit = true
		if split {
			v.Reason = fmt.Sprintf("ENSEMBLE BREAKER: directional split buy=%.2f sell=%.2f lacks 2:1 dominance; forcing HOLD", buyMass, sellMass)
		} else {
			v.Reason = fmt.Sprintf("ENSEMBLE BREAKER: vote variance %.3f > %.3f; forcing HOLD", v.Variance, g.cfg.VarianceLimit)
		}
		g.logf("[ensemble] %s %s", sym, v.Reason)
		return v
	}

	best, bestMass := ActionHold, holdMass
	if buyMass > bestMass {
		best, bestMass = ActionBuy, buyMass
	}
	if sellMass > bestMass {
		best, bestMass = ActionSell, sellMass
	}

	total := buyMass + sellMass + holdMass
	conf := 0.0
	if total > 0 && best != ActionHold {
		conf = bestMass / total
	}
	if best == ActionHold || conf < g.cfg.MinConfidence {
		v.Action = ActionHold
		v.Confidence = conf
		v.Reason = fmt.Sprintf("buy=%.2f sell=%.2f hold=%.2f conf=%.2f < floor %.2f",
			buyMass, sellMass, holdMass, conf, g.cfg.MinConfidence)
		return v
	}

	v.Action = best
	v.Confidence = conf
	v.Reason = fmt.Sprintf("won mass=%.2f of %.2f (conf=%.2f)", bestMass, total, conf)
	return v
}

func (g *Gater) logf(format string, a ...any) {
	if g.cfg.Log != nil {
		g.cfg.Log.Printf(format, a...)
	}
}

// variance is the population variance of xs.
func variance(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := Mean(xs)
	var ss float64
	for _, x := range xs {
		ss += (x - m) * (x - m)
	}
	return ss / float64(len(xs))
}

// Has reports whether the key exists (distinguishing explicit 0 weight).
func (w VoiceWeights) Has(k string) bool {
	_, ok := w[k]
	return ok
}
