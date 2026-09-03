package ensemble

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

func timeDate(y, m, d int) time.Time {
	return time.Date(y, time.Month(m), d, 12, 0, 0, 0, time.UTC)
}

// synth.go: synthetic bar builders shared by every expert's table tests.
// Bars are daily, ascending, deterministic — no randomness so failures
// reproduce exactly.

const day = int64(86400) // seconds per session (timestamps are arbitrary)

// flatBars builds n bars closing at px with tiny volume and no range.
func flatBars(n int, px float64) []tools.Bar {
	bars := make([]tools.Bar, n)
	for i := range bars {
		bars[i] = tools.Bar{Time: int64(i) * day, Open: px, High: px, Low: px, Close: px}
	}
	return bars
}

// rampBars builds n bars whose close rises (step > 0) or falls
// (step < 0) linearly from startPx. Volume constant at vol.
func rampBars(n int, startPx, step float64, vol int64) []tools.Bar {
	bars := make([]tools.Bar, n)
	px := startPx
	for i := range bars {
		next := px + step
		hi := math.Max(px, next) * 1.001
		lo := math.Min(px, next) * 0.999
		bars[i] = tools.Bar{Time: int64(i) * day, Open: px, High: hi, Low: lo,
			Close: next, Volume: vol}
		px = next
	}
	return bars
}

// lastBar overrides the final bar of a series.
func lastBar(bars []tools.Bar, b tools.Bar) []tools.Bar {
	out := append([]tools.Bar(nil), bars...)
	b.Time = out[len(out)-1].Time
	out[len(out)-1] = b
	return out
}

func mdOf(sym string, sd *SymbolData, held ...string) MarketData {
	h := map[string]bool{}
	for _, s := range held {
		h[s] = true
	}
	return MarketData{Symbol: sym, Symbols: map[string]*SymbolData{sym: sd}, Held: h}
}

// fakeTrendScorer scripts TrendExpert scores per symbol.
type fakeTrendScorer struct {
	res map[string]trendScored
	err error
}

func (f fakeTrendScorer) ScoreFlat(_ context.Context, symbol string, _ MarketData) (trendScored, error) {
	if f.err != nil {
		return trendScored{}, f.err
	}
	return f.res[symbol], nil
}

func TestTrendExpertBuySellHold(t *testing.T) {
	tests := []struct {
		name   string
		score  trendScored
		held   bool
		action Action
	}{
		{"all gates pass -> buy", trendScored{Composite: 0.9, Trend: 0.9, Momentum: 0.9}, false, ActionBuy},
		{"composite below min -> hold", trendScored{Composite: 0.5, Trend: 0.9, Momentum: 0.9}, false, ActionHold},
		{"trend below buy -> hold", trendScored{Composite: 0.9, Trend: 0.6, Momentum: 0.9}, false, ActionHold},
		{"momentum below buy -> hold", trendScored{Composite: 0.9, Trend: 0.9, Momentum: 0.5}, false, ActionHold},
		{"held exit composite", trendScored{Composite: 0.3, Trend: 0.1, Momentum: 0.9}, true, ActionSell},
		{"held exit momentum", trendScored{Composite: 0.9, Trend: 0.9, Momentum: 0.2}, true, ActionSell},
		{"held mid scores -> hold", trendScored{Composite: 0.6, Trend: 0.7, Momentum: 0.6}, true, ActionHold},
	}
	th := DefaultTrendThresholds()
	exp, err := NewTrendExpert(fakeTrendScorer{res: map[string]trendScored{
		"SYM": th.toScore(), // replaced below per-case
	}}, th)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exp.Scorer = fakeTrendScorer{res: map[string]trendScored{"SYM": tc.score}}
			data := mdOf("SYM", &SymbolData{}, "OTHER")
			if tc.held {
				data = mdOf("SYM", &SymbolData{}, "SYM")
			}
			sigs, err := exp.Evaluate(context.Background(), "SYM", data)
			if err != nil {
				t.Fatal(err)
			}
			if len(sigs) != 1 || sigs[0].Action != tc.action {
				t.Fatalf("got %+v, want action %s", sigs, tc.action)
			}
			if sigs[0].Confidence < 0 || sigs[0].Confidence > 1 {
				t.Fatalf("confidence out of range: %v", sigs[0].Confidence)
			}
		})
	}
}

// toScore is a helper keeping the table literal short; the real scorer
// replaces it per case anyway.
func (t TrendThresholds) toScore() trendScored { return trendScored{} }

func TestMeanRevZScore(t *testing.T) {
	// 30 flat closes at 100 then one crash to 91 => z strongly < -2.
	closes := make([]float64, 31)
	for i := range closes[:30] {
		closes[i] = 100
	}
	closes[30] = 91 // ~9 sigma below SMA20=100 given zero stdev? use jitter instead
	// Add tiny alternating jitter so stdev is nonzero but small.
	for i := range closes[:30] {
		if i%2 == 0 {
			closes[i] = 100.5
		} else {
			closes[i] = 99.5
		}
	}
	sd := &SymbolData{Closes: closes}
	sig, err := MeanRevExpert{}.Evaluate(context.Background(), "X", mdOf("X", sd))
	if err != nil {
		t.Fatal(err)
	}
	if sig[0].Action != ActionBuy {
		t.Fatalf("oversold should BUY, got %+v", sig[0])
	}
	if sig[0].Regime != RegimeRanging {
		t.Fatalf("want ranging regime, got %s", sig[0].Regime)
	}

	// Trending tape: strong drift must suppress the fade even at low z.
	ramp := rampBars(40, 50, 3.5, 1000) // +7%/session drift
	sd2 := &SymbolData{Closes: Closes(ramp), ATR14: 4}
	sig2, err := (MeanRevExpert{}).Evaluate(context.Background(), "X", mdOf("X", sd2))
	if err != nil {
		t.Fatal(err)
	}
	if sig2[0].Action != ActionHold || sig2[0].Regime != RegimeTrending {
		t.Fatalf("drifting tape must abstain as trending-hold, got %+v", sig2[0])
	}

	// Held + overbought exit: spike above SMA on a flat tape.
	flat := append(Closes(flatBars(25, 100)), 109)
	sd3 := &SymbolData{Closes: flat}
	sig3, err := MeanRevExpert{}.Evaluate(context.Background(), "X", mdOf("X", sd3, "X"))
	if err != nil {
		t.Fatal(err)
	}
	if sig3[0].Action != ActionSell {
		t.Fatalf("overbought held position should SELL, got %+v", sig3[0])
	}
}

func TestBreakoutVolumeConfirmed(t *testing.T) {
	// 41 bars: flat 100 with volume 1000, then close 103 (> prior 20d high)
	// with volume surge.
	base := flatBars(41, 100)
	for i := range base {
		base[i].High = 100.2
		base[i].Low = 99.8
		base[i].Volume = 1000
	}
	final := base[40]
	final.Close, final.High, final.Volume = 103, 103.1, 2000
	barsUp := lastBar(base, final)

	atr, _ := ATR(barsUp, 14)
	sd := &SymbolData{Bars: barsUp, Closes: Closes(barsUp), ATR14: atr}
	sig, err := BreakoutExpert{}.Evaluate(context.Background(), "B", mdOf("B", sd))
	if err != nil {
		t.Fatal(err)
	}
	if sig[0].Action != ActionBuy {
		t.Fatalf("breakout+volume must BUY, got %+v", sig[0])
	}

	// Same breakout WITHOUT volume confirmation -> Hold.
	final.Volume = 1100
	barsQuiet := lastBar(base, final)
	sdQ := &SymbolData{Bars: barsQuiet, Closes: Closes(barsQuiet), ATR14: atr}
	sigQ, _ := BreakoutExpert{}.Evaluate(context.Background(), "B", mdOf("B", sdQ))
	if sigQ[0].Action != ActionHold {
		t.Fatalf("quiet-volume breakout must HOLD, got %+v", sigQ[0])
	}

	// Downside breakdown while HELD -> Sell.
	down := rampBars(41, 100, -0.05, 1000)
	last := down[40]
	last.Close, last.Low, last.Volume = 96, 95.9, 2500
	down = lastBar(down, last)
	sdD := &SymbolData{Bars: down, Closes: Closes(down), ATR14: 0.5}
	sdD.Bars[len(sdD.Bars)-1].Close = 96
	sdD.Closes[len(sdD.Closes)-1] = 96
	sigD, _ := BreakoutExpert{}.Evaluate(context.Background(), "B", mdOf("B", sdD, "B"))
	if sigD[0].Action != ActionSell {
		t.Fatalf("volume breakdown while held must SELL, got %+v", sigD[0])
	}
}

func TestPairsLongOnly(t *testing.T) {
	// Build a ratio series where A/B collapses far below its mean at the
	// END: A flat 100 throughout; B flat 100 except a sharp recent jump
	// to 160 over the last 10 sessions -> ratio drops to 0.625 late ->
	// z << -2 -> BUY cheap leg A.
	n := pairMinBars + 20
	a := Closes(flatBars(n, 100))
	b := Closes(flatBars(n, 100))
	for i := n - 10; i < n; i++ {
		b[i] = 100 + float64(i-(n-11))*6 // 106..160 over the final stretch
	}

	sdA := &SymbolData{Closes: a}
	sdB := &SymbolData{Closes: b}
	md := MarketData{Symbols: map[string]*SymbolData{"AAA": sdA, "BBB": sdB}, Held: map[string]bool{}}

	exp := &PairsExpert{Pairs: []Pair{{A: "AAA", B: "BBB"}}}
	sigs, err := exp.Evaluate(context.Background(), "", md)
	if err != nil {
		t.Fatal(err)
	}
	var buyLeg string
	for _, s := range sigs {
		if s.Action == ActionBuy {
			buyLeg = s.Symbol
		}
	}
	if buyLeg != "AAA" {
		t.Fatalf("cheap leg AAA should be bought, sigs=%+v", sigs)
	}

	// Convergence: ratio back near mean while holding AAA -> Sell AAA.
	aFlat := Closes(flatBars(n, 100))
	bFlat := Closes(flatBars(n, 100))
	bFlat[n-1] = 101 // tiny final move keeps |z| small
	mdConv := MarketData{Symbols: map[string]*SymbolData{
		"AAA": {Closes: aFlat}, "BBB": {Closes: bFlat}}, Held: map[string]bool{"AAA": true}}
	sigsConv, _ := exp.Evaluate(context.Background(), "", mdConv)
	foundExit := false
	for _, s := range sigsConv {
		if s.Action == ActionSell && s.Symbol == "AAA" {
			foundExit = true
		}
		if s.Action == ActionBuy {
			t.Fatalf("converged spread must not emit buys: %+v", sigsConv)
		}
	}
	if !foundExit {
		t.Fatalf("held leg should exit on convergence: %+v", sigsConv)
	}
}

// setVolume sets every bar's volume; test-only fluent replacement.
func setVolume(b []tools.Bar, v int64) []tools.Bar {
	for i := range b {
		b[i].Volume = v
	}
	return b
}

func TestXSMomRanks(t *testing.T) {
	mk := func(px float64) *SymbolData {
		return &SymbolData{Closes: Closes(rampBars(xsMomWindow+1, px/2, px/(xsMomWindow), 100))}
	}
	// Direct construction: winners end high, losers end low.
	sd := func(start, end float64) *SymbolData {
		closes := make([]float64, xsMomWindow+1)
		for i := range closes {
			closes[i] = start + (end-start)*float64(i)/xsMomWindow
		}
		return &SymbolData{Closes: closes}
	}
	md := MarketData{
		Symbols: map[string]*SymbolData{
			"WIN1": sd(90, 120), "WIN2": sd(90, 115), "WIN3": sd(90, 112),
			"MID": sd(90, 100), "LOSE1": sd(120, 80), "LOSE2": sd(120, 85),
			"LOSE3": sd(120, 90),
		},
		Held: map[string]bool{"LOSE1": true},
	}
	sigs, err := NewXSMomExpert().Evaluate(context.Background(), "", md)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Action{}
	for _, s := range sigs {
		got[s.Symbol] = s.Action
	}
	for _, w := range []string{"WIN1", "WIN2", "WIN3"} {
		if got[w] != ActionBuy {
			t.Fatalf("%s should be top-3 BUY, got %s (%+v)", w, got[w], sigs)
		}
	}
	if got["LOSE1"] != ActionSell {
		t.Fatalf("held laggard LOSE1 should SELL, got %+v", got)
	}
	_ = mk
}

func TestSeasonalityNeverAloneTriggers(t *testing.T) {
	exp := &SeasonalityExpert{}
	// Turn-of-month window emits weak confidence <= 0.55 always.
	for d := 1; d <= 28; d++ {
		now := timeDate(2026, 7, d)
		sigs, _ := exp.Evaluate(context.Background(), "S",
			MarketData{Now: now})
		for _, s := range sigs {
			if s.Confidence > 0.55 {
				t.Fatalf("seasonality confidence %.2f exceeds 0.55 cap on day %d", s.Confidence, d)
			}
		}
	}
	// TOM boundary days produce calendar signals.
	for _, d := range []int{31, 1, 2, 3} {
		now := timeDate(2026, monthFor(d), d)
		sigs, _ := exp.Evaluate(context.Background(), "S", MarketData{Now: now})
		if len(sigs) == 0 || sigs[0].Regime != RegimeCalendarOnly {
			t.Fatalf("day %d expected calendar signal, got %+v", d, sigs)
		}
	}
}

func monthFor(d int) int {
	if d >= 30 {
		return 7 // July has 31 days
	}
	return 7
}
