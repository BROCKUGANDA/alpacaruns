package factors

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// fakeBars is a scripted BarSource.
type fakeBars struct {
	bars map[string][]tools.Bar
	err  error
}

func (f fakeBars) GetBars(ctx context.Context, symbols []string, timeframe, start, end string, limit int) (map[string][]tools.Bar, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string][]tools.Bar{}
	for _, s := range symbols {
		out[s] = f.bars[s]
	}
	return out, nil
}

// fakeNews is a scripted NewsSource.
type fakeNews struct {
	items []tools.NewsItem
	err   error
}

func (f fakeNews) GetNews(ctx context.Context, symbols []string, limit int) ([]tools.NewsItem, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

const testDay = 86400 // seconds; bar times just need to be ascending

// dailyBars builds n ascending daily bars with the given closes and a
// constant volume (overridable for the last bar via lastVol).
func dailyBars(closes []float64, vol int64) []tools.Bar {
	bars := make([]tools.Bar, len(closes))
	price := 0.0
	for i, c := range closes {
		if c == 0 { // zero means "carry previous close" convenience not used here
			c = price
		}
		price = c
		bars[i] = tools.Bar{
			Time: int64(i) * testDay,
			Open: c, High: c, Low: c, Close: c,
			Volume: vol,
		}
	}
	return bars
}

func risingCloses(n int, start, step float64) []float64 {
	c := make([]float64, n)
	for i := range c {
		c[i] = start + step*float64(i)
	}
	return c
}

func fallingCloses(n int, start, step float64) []float64 {
	c := make([]float64, n)
	for i := range c {
		c[i] = start - step*float64(i)
	}
	return c
}

func flatCloses(n int, at float64) []float64 {
	c := make([]float64, n)
	for i := range c {
		c[i] = at
	}
	return c
}

func newTestEngine(b BarSource, news NewsSource) *Engine {
	cfg := &config.Config{FactorWeights: config.DefaultFactorWeights, FactorMinScore: 0.6}
	return NewEngine(cfg, b, news, Options{})
}

func TestScoreTrend(t *testing.T) {
	tests := []struct {
		name       string
		closes     []float64
		wantScore  float64
		wantSubstr string
	}{
		{"uptrend above both MAs", risingCloses(60, 100, 1), 1.0, "above SMA20"},
		{"downtrend below both MAs", fallingCloses(60, 200, 1), 0.0, "below SMA20"},
		{"flat at both MAs", flatCloses(60, 100), 0.5, "at SMA20"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoreTrend(tt.closes)
			if math.Abs(got.Score-tt.wantScore) > 1e-9 {
				t.Fatalf("score = %.2f, want %.2f (%s)", got.Score, tt.wantScore, got.Rationale)
			}
			if !contains(got.Rationale, tt.wantSubstr) {
				t.Fatalf("rationale %q missing %q", got.Rationale, tt.wantSubstr)
			}
		})
	}
}

func TestScoreMomentum(t *testing.T) {
	tests := []struct {
		name      string
		prevLast  [2]float64
		days      int
		wantScore float64
	}{
		{"strong up", [2]float64{100, 115}, 10, 1.0}, // +15% -> clamped to 1
		{"mild up", [2]float64{100, 104}, 10, 0.7},   // +4% -> .5+.2
		{"flat", [2]float64{100, 100}, 10, 0.5},
		{"mild down", [2]float64{100, 96}, 10, 0.3},
		{"crash", [2]float64{100, 80}, 10, 0.0}, // -20% -> clamped to 0
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closes := make([]float64, tt.days+1)
			for i := range closes {
				closes[i] = tt.prevLast[0]
			}
			closes[len(closes)-1] = tt.prevLast[1]
			got := scoreMomentum(closes, tt.days)
			if math.Abs(got.Score-tt.wantScore) > 1e-9 {
				t.Fatalf("score = %.3f, want %.3f (%s)", got.Score, tt.wantScore, got.Rationale)
			}
		})
	}
}

func TestScoreVolatility(t *testing.T) {
	const thr = DefaultVolThreshold
	// Alternating +/-thr daily returns: mean 0, stdev exactly thr -> score 0.5.
	closes := []float64{100}
	for i := 0; i < DefaultVolWindow; i++ {
		factor := 1 + thr
		if i%2 == 1 {
			factor = 1 - thr
		}
		closes = append(closes, closes[len(closes)-1]*factor)
	}
	got := scoreVolatility(closes, DefaultVolWindow, thr)
	if math.Abs(got.Score-0.5) > 0.01 {
		t.Fatalf("score = %.4f, want ~0.5 when stdev == threshold (%s)", got.Score, got.Rationale)
	}

	// Wild swings (+/-3x thr): stdev ~3x threshold -> score clamps to 0.
	wild := []float64{100}
	for i := 0; i < DefaultVolWindow; i++ {
		factor := 1 + 3*thr
		if i%2 == 1 {
			factor = 1 - 3*thr
		}
		wild = append(wild, wild[len(wild)-1]*factor)
	}
	got2 := scoreVolatility(wild, DefaultVolWindow, thr)
	if got2.Score > 0.05 {
		t.Fatalf("extreme vol score = %.4f, want <= 0.05 (%s)", got2.Score, got2.Rationale)
	}

	// Calm tape (tiny alternating drift): near-perfect score.
	calm := []float64{100}
	for i := 0; i < DefaultVolWindow; i++ {
		factor := 1.0002
		if i%2 == 1 {
			factor = 0.9998
		}
		calm = append(calm, calm[len(calm)-1]*factor)
	}
	got3 := scoreVolatility(calm, DefaultVolWindow, thr)
	if got3.Score < 0.99 {
		t.Fatalf("calm tape score = %.4f, want >= 0.99 (%s)", got3.Score, got3.Rationale)
	}
}

func TestScoreVolume(t *testing.T) {
	base := dailyBars(flatCloses(21, 100), 1000)

	surge := append([]tools.Bar{}, base...)
	surge[len(surge)-1].Volume = 4000 // 4x average -> score 2.0 -> clamped 1
	if got := scoreVolume(surge); got.Score != 1.0 {
		t.Fatalf("surge score = %.2f, want 1.0 (%s)", got.Score, got.Rationale)
	}

	normal := append([]tools.Bar{}, base...)
	normal[len(normal)-1].Volume = 2000 // 2x average -> score 1.0 boundary
	if got := scoreVolume(normal); math.Abs(got.Score-1.0) > 1e-9 {
		t.Fatalf("2x volume score = %.3f, want 1.0 (%s)", got.Score, got.Rationale)
	}

	thin := append([]tools.Bar{}, base...)
	thin[len(thin)-1].Volume = 500 // half average -> score 0.25
	if got := scoreVolume(thin); math.Abs(got.Score-0.25) > 1e-9 {
		t.Fatalf("thin score = %.3f, want 0.25 (%s)", got.Score, got.Rationale)
	}
}

func TestScoreSentimentNeverFatal(t *testing.T) {
	// Nil source: neutral.
	if got := scoreSentiment(context.Background(), nil, "AAPL"); got.Score != 0.5 {
		t.Fatalf("nil source score = %.2f, want 0.5", got.Score)
	}
	// Erroring source: neutral, no panic.
	if got := scoreSentiment(context.Background(), fakeNews{err: errors.New("feed down")}, "AAPL"); got.Score != 0.5 {
		t.Fatalf("erroring source score = %.2f, want 0.5", got.Score)
	}
	// Empty feed: neutral.
	if got := scoreSentiment(context.Background(), fakeNews{}, "AAPL"); got.Score != 0.5 {
		t.Fatalf("empty feed score = %.2f, want 0.5", got.Score)
	}
	// Busy feed: slight positive.
	busy := make([]tools.NewsItem, 6)
	if got := scoreSentiment(context.Background(), fakeNews{items: busy}, "AAPL"); got.Score != 0.55 {
		t.Fatalf("busy feed score = %.2f, want 0.55", got.Score)
	}
}

func TestEngineCompositeAndPass(t *testing.T) {
	up := newTestEngine(fakeBars{bars: map[string][]tools.Bar{
		"AAPL": dailyBars(risingCloses(60, 100, 0.2), 2_000_000),
	}}, fakeNews{items: make([]tools.NewsItem, 6)})
	res, err := up.Score(context.Background(), "AAPL")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Factors) != 5 {
		t.Fatalf("expected 5 factors, got %d", len(res.Factors))
	}
	// Strong uptrend, strong momentum, calm vol, steady volume, active news:
	// every factor >= its weight-neutral baseline, so composite should clear
	// a 0.6 minimum comfortably.
	if !res.Passed || res.Composite < 0.6 {
		t.Fatalf("healthy uptrend should pass: composite=%.3f passed=%v factors=%+v",
			res.Composite, res.Passed, res.Factors)
	}

	down := newTestEngine(fakeBars{bars: map[string][]tools.Bar{
		"AAPL": dailyBars(fallingCloses(60, 300, 1.0), 500_000),
	}}, fakeNews{})
	resD, err := down.Score(context.Background(), "AAPL")
	if err != nil {
		t.Fatal(err)
	}
	if resD.Passed {
		t.Fatalf("downtrend should fail the gate: composite=%.3f", resD.Composite)
	}
}

func TestEngineInsufficientBarsErrors(t *testing.T) {
	e := newTestEngine(fakeBars{bars: map[string][]tools.Bar{
		"AAPL": dailyBars(risingCloses(30, 100, 1), 1000),
	}}, nil)
	if _, err := e.Score(context.Background(), "AAPL"); err == nil {
		t.Fatal("expected error with <51 bars")
	}
}

func TestEngineBarFetchErrorPropagates(t *testing.T) {
	e := newTestEngine(fakeBars{err: errors.New("alpaca 500")}, nil)
	if _, err := e.Score(context.Background(), "AAPL"); err == nil {
		t.Fatal("expected error from bar fetch failure (risk gate relies on it)")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ = fmt.Sprintf // keep fmt imported if assertions change
