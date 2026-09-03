package ensemble

import "math"

// volregime.go: NOT directional. Produces a RegimeAssessment consumed by
// the gater to reweight expert voices:
//
//	LowVol    — SPY realized vol in the calm part of its history
//	RisingVol — vol elevated vs its trailing percentile
//	Crisis    — vol at extreme percentile
//
// Inputs: ATR-14 percentile of the benchmark over its own bar history
// plus a VIX proxy = annualized realized vol of the benchmark's last 20
// sessions.

const (
	volRegimeWindow   = 20   // sessions for the VIX-proxy window
	volRegimeMinBars  = 120  // history needed for a stable percentile
	volRisingPercentile = 0.70 // >= this ATR percentile -> RisingVol
	volCrisisPercentile = 0.90 // >= this ATR percentile -> Crisis
	annualizeFactor     = 252.0
)

// VolLevel classifies the regime.
type VolLevel string

const (
	VolLow    VolLevel = "low-vol"
	VolRising VolLevel = "rising-vol"
	VolCrisis VolLevel = "crisis"
)

// RegimeAssessment is the volatility-regime verdict for one tick.
type RegimeAssessment struct {
	Level          VolLevel
	VIXProxy       float64 // annualized realized vol of the benchmark (e.g. 0.18 = 18%)
	ATRPercentile  float64 // benchmark's latest ATR vs its own history, 0..1
	Benchmark      string
}

// AssessVolRegime computes the assessment from shared bars for the
// benchmark symbol (default SPY). Degenerate data degrades to LowVol —
// never blocks trading on a broken feed.
func AssessVolRegime(data MarketData) RegimeAssessment {
	bench := data.Benchmark()
	out := RegimeAssessment{Level: VolLow, VIXProxy: 0, ATRPercentile: 0.5, Benchmark: bench}

	sd := data.SD(bench)
	if sd == nil || len(sd.Bars) < volRegimeMinBars {
		return out
	}
	bars := sd.Bars

	// ATR series: rolling 14-session ATR across history for percentile.
	const atrW = 14
	var atrs []float64
	for end := atrW; end <= len(bars); end++ {
		if a, ok := ATR(bars[:end], atrW); ok {
			atrs = append(atrs, a)
		}
	}
	if len(atrs) == 0 {
		return out
	}
	lastATR := atrs[len(atrs)-1]
	out.ATRPercentile = PercentileRank(atrs, lastATR)

	// VIX proxy: stdev of last N daily returns, annualized.
	rets := Returns(sd.Closes)
	tail := rets
	if len(tail) > volRegimeWindow {
		tail = tail[len(tail)-volRegimeWindow:]
	}
	out.VIXProxy = Stdev(tail) * math.Sqrt(annualizeFactor)

	switch {
	case out.ATRPercentile >= volCrisisPercentile || out.VIXProxy >= crisisVolFloor():
		out.Level = VolCrisis
	case out.ATRPercentile >= volRisingPercentile:
		out.Level = VolRising
	default:
		out.Level = VolLow
	}
	return out
}

// crisisVolFloor is the absolute VIX-proxy level (~35% annualized) that
// alone qualifies as Crisis regardless of percentile.
func crisisVolFloor() float64 { return 0.35 }
