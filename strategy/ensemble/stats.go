package ensemble

import (
	"math"
	"sort"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// stats.go: hand-rolled descriptive statistics over daily bars. Gonum is
// deliberately not a dependency; every function here is textbook math.

// SortBars orders bars ascending by timestamp (Alpaca feeds arrive in
// order, but defensive sorting keeps every downstream window correct).
func SortBars(bars []tools.Bar) {
	sort.SliceStable(bars, func(i, j int) bool { return bars[i].Time < bars[j].Time })
}

// Closes extracts the close series from bars.
func Closes(bars []tools.Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}

// Mean returns the arithmetic mean; 0 for an empty slice.
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// Stdev is the SAMPLE standard deviation (n-1 denominator); 0 when
// fewer than two points.
func Stdev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := Mean(xs)
	var ss float64
	for _, x := range xs {
		d := x - m
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

// Returns computes consecutive fractional returns of closes
// (len(out) == len(in)-1); empty input yields nil.
func Returns(closes []float64) []float64 {
	if len(closes) < 2 {
		return nil
	}
	out := make([]float64, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		if closes[i-1] != 0 {
			out[i-1] = closes[i]/closes[i-1] - 1
		}
	}
	return out
}

// SMA is the simple moving average of the LAST n values; ok=false when
// fewer than n are available.
func SMA(xs []float64, n int) (float64, bool) {
	if n <= 0 || len(xs) < n {
		return 0, false
	}
	return Mean(xs[len(xs)-n:]), true
}

// ATR computes the average true range over the last window sessions.
// TR for bar i = max(high-low, |high-prevClose|, |low-prevClose|). The
// first bar has no previous close so its range is high-low. ok=false
// without at least one usable bar.
func ATR(bars []tools.Bar, window int) (float64, bool) {
	if window <= 0 || len(bars) == 0 {
		return 0, false
	}
	start := max(1, len(bars)-window)
	trs := make([]float64, 0, window)
	for i := start; i < len(bars); i++ {
		hl := bars[i].High - bars[i].Low
		tr := hl
		if i > 0 {
			pc := bars[i-1].Close
			tr = math.Max(hl, math.Max(math.Abs(bars[i].High-pc), math.Abs(bars[i].Low-pc)))
		}
		trs = append(trs, tr)
	}
	if len(trs) == 0 {
		return 0, false
	}
	return Mean(trs), true
}

// PercentileRank reports the fraction of xs strictly below x (0..1);
// 0.5 for an empty slice.
func PercentileRank(xs []float64, x float64) float64 {
	if len(xs) == 0 {
		return 0.5
	}
	var below int
	for _, v := range xs {
		if v < x {
			below++
		}
	}
	return float64(below) / float64(len(xs))
}

// Corr is the Pearson correlation of two equal-length return series;
// 0 when either side has no variance or the lengths mismatch.
func Corr(a, b []float64) float64 {
	n := min(len(a), len(b))
	if n < 2 {
		return 0
	}
	a, b = a[:n], b[:n]
	ma, mb := Mean(a), Mean(b)
	var sab, sa2, sb2 float64
	for i := range a {
		da, db := a[i]-ma, b[i]-mb
		sab += da * db
		sa2 += da * da
		sb2 += db * db
	}
	if sa2 == 0 || sb2 == 0 {
		return 0
	}
	return sab / math.Sqrt(sa2*sb2)
}

// Clamp01 clamps x into [0,1].
func Clamp01(x float64) float64 { return math.Max(0, math.Min(1, x)) }
