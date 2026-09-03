package ensemble

import (
	"strings"
	"testing"
	"time"
)

func TestGaterWeightedVote(t *testing.T) {
	tr := NewTracker(30, "")
	g := NewGater(GaterConfig{Base: DefaultVoiceWeights(), MinConfidence: 0.55}, tr)

	tests := []struct {
		name   string
		votes  map[string][]Signal
		regime RegimeAssessment
		want   Action
		wantC  bool // circuit breaker forced hold
	}{
		{"unanimous buy wins", map[string][]Signal{
			"trend":    {{Symbol: "X", Action: ActionBuy, Confidence: 0.9}},
			"breakout": {{Symbol: "X", Action: ActionBuy, Confidence: 0.8}},
			"xsmom":    {{Symbol: "X", Action: ActionBuy, Confidence: 0.85}},
		}, RegimeAssessment{Level: VolLow}, ActionBuy, false},
		{"hold floor blocks weak buy", map[string][]Signal{
			"trend":       {{Symbol: "X", Action: ActionBuy, Confidence: 0.4}},
			"seasonality": {{Symbol: "X", Action: ActionHold, Confidence: 0.5}},
			"meanrev":     {{Symbol: "X", Action: ActionHold, Confidence: 0.5}},
			"pairs":       {{Symbol: "X", Action: ActionHold, Confidence: 0.5}},
			"xsmom":       {{Symbol: "X", Action: ActionHold, Confidence: 0.5}},
		}, RegimeAssessment{Level: VolLow}, ActionHold, false},
		{"crisis zeroes meanrev, trend carries", map[string][]Signal{
			"trend":   {{Symbol: "X", Action: ActionBuy, Confidence: 0.7}},
			"meanrev": {{Symbol: "X", Action: ActionBuy, Confidence: 1.0}},
		}, RegimeAssessment{Level: VolCrisis}, ActionBuy, false},
		{"crisis: contrarian-only vote dies", map[string][]Signal{
			"meanrev": {{Symbol: "X", Action: ActionBuy, Confidence: 1.0}},
		}, RegimeAssessment{Level: VolCrisis}, ActionHold, false},
		{"disagreement breaker", map[string][]Signal{
			"trend":   {{Symbol: "X", Action: ActionBuy, Confidence: 1.0}},
			"meanrev": {{Symbol: "X", Action: ActionSell, Confidence: 1.0}},
		}, RegimeAssessment{Level: VolLow}, ActionHold, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vs := g.Gate(tc.votes, tc.regime, time.Now())
			if len(vs) != 1 {
				t.Fatalf("want 1 verdict, got %d", len(vs))
			}
			v := vs[0]
			if v.Action != tc.want {
				t.Fatalf("action=%s want %s (verdict %+v)", v.Action, tc.want, v)
			}
			if v.Circuit != tc.wantC {
				t.Fatalf("circuit=%v want %v (variance %.3f)", v.Circuit, tc.wantC, v.Variance)
			}
		})
	}
}

func TestGaterRegimeModifierTable(t *testing.T) {
	tests := []struct {
		expert string
		level  VolLevel
		want   float64
	}{
		{"meanrev", VolRising, 0.5},
		{"pairs", VolCrisis, 0},
		{"trend", VolRising, 1.25},
		{"breakout", VolCrisis, 1.5},
		{"trend", VolLow, 0.75},
		{"meanrev", VolLow, 1.25},
		{"xsmom", VolRising, 1.0}, // untouched voice
	}
	for _, tc := range tests {
		if got := regimeModifier(tc.expert, tc.level); got != tc.want {
			t.Errorf("regimeModifier(%s,%s)=%g want %g", tc.expert, tc.level, got, tc.want)
		}
	}
}

func TestTrackerHitRateAndResolve(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(10, dir+"/state.json")

	// Neutral before any samples.
	if hr := tr.HitRate("e1", 5); hr != 0.5 {
		t.Fatalf("neutral hit-rate want 0.5 got %g", hr)
	}

	now := time.Now()
	sig := Signal{Symbol: "S", Action: ActionBuy, ExpertName: "e1"}
	tr.Record(sig, 100, 2.0, now)

	// Resolve with a favorable move (+1.2 > 0.5*ATR=1.0).
	closes := map[string][]float64{"S": {101.2}}
	n := tr.Resolve(closes)
	if n != 1 {
		t.Fatalf("want 1 resolved, got %d", n)
	}
	if hr := tr.HitRate("e1", 1); hr != 1.0 {
		t.Fatalf("after win hit-rate want 1.0 got %g", hr)
	}

	// Persistence round-trip.
	tr.Record(Signal{Symbol: "T", Action: ActionSell, ExpertName: "e2"}, 50, 1, now)
	if err := tr.Save(); err != nil {
		t.Fatal(err)
	}
	tr2 := NewTracker(10, dir+"/state.json")
	if err := tr2.Load(); err != nil {
		t.Fatal(err)
	}
	if tr2.Samples("e1") != 1 {
		t.Fatalf("history not persisted: e1 samples=%d", tr2.Samples("e1"))
	}

	// Expiry without favorable move counts a miss.
	tr3 := NewTracker(10, "")
	expired := PendingSignal{Expert: "e3", Symbol: "Z", Action: ActionBuy,
		Entry: 100, ATR: 2, EmittedAt: now.Add(-10 * 24 * time.Hour),
		ExpiresAt: now.Add(-time.Hour)}
	tr3.mu.Lock()
	tr3.pending = []PendingSignal{expired}
	tr3.mu.Unlock()
	tr3.Resolve(map[string][]float64{"Z": {99.0}})
	if hr := tr3.HitRate("e3", 1); hr != 0.0 {
		t.Fatalf("expiry should count miss, hit-rate=%g", hr)
	}
}

func TestRiskBudgetATRSizing(t *testing.T) {
	rb := NewRiskBudget(RiskConfig{
		RiskPctPerTrade: 0.01, MaxPositionUSD: 10000, PositionPct: 0.05,
		MaxCorrelation: 0.85, LiquidityPct: 0.01,
	})
	sd := &SymbolData{ATR14: 5.0}
	sd.Bars = flatBars(25, 200)
	sd.Closes = Closes(sd.Bars)
	for i := range sd.Bars {
		sd.Bars[i].Volume = 100000
	}
	md := mdOf("X", sd)

	// pfv=100000 -> risk budget $1000 / (2*5 ATR) = 100 shares.
	// Legacy: min(5000, 10000)/200 = 25 shares -> legacy binds at 25.
	v := Verdict{Symbol: "X", Action: ActionBuy}
	sv := rb.Apply(v, 100000, md)
	if sv.Qty != 25 {
		t.Fatalf("legacy cap should bind at 25 shares, got %d (sv=%+v)", sv.Qty, sv)
	}

	// Small ATR: atrQty huge, legacy still caps.
	sd.ATR14 = 0.5
	sv = rb.Apply(v, 100000, md)
	if sv.Qty != 25 {
		t.Fatalf("legacy cap must still bind, got %d", sv.Qty)
	}

	// Tiny pfv: legacy qty < 1 -> blocked.
	sv = rb.Apply(v, 500, md)
	if !sv.Blocked || sv.Qty != 0 {
		t.Fatalf("sub-share sizing must block, got %+v", sv)
	}
}

func TestRiskBudgetCorrelationBlock(t *testing.T) {
	rc := DefaultRiskConfig()
	rc.PositionPct, rc.MaxPositionUSD = 0.05, 100000
	rb := NewRiskBudget(rc)
	// Candidate perfectly correlated to held symbol.
	closes := make([]float64, 21)
	for i := range closes {
		closes[i] = 100 + float64(i)
	}
	held := append([]float64(nil), closes...)
	md := MarketData{
		Symbols: map[string]*SymbolData{
			"NEW": {Closes: closes, ATR14: 1},
			"OLD": {Closes: held, ATR14: 1},
		},
		Held: map[string]bool{"OLD": true},
	}
	for sym, s := range md.Symbols {
		s.Bars = flatBars(21, s.Closes[len(s.Closes)-1])
		s.Closes = append([]float64(nil), s.Closes...)
		for i := range s.Bars {
			s.Bars[i].Volume = 1000000
		}
		_ = sym
	}
	sv := rb.Apply(Verdict{Symbol: "NEW", Action: ActionBuy}, 100000, md)
	if !sv.Blocked || !strings.Contains(sv.BlockWhy, "correlation") {
		t.Fatalf("corr-1.0 candidate must be blocked: %+v", sv)
	}

	// Uncorrelated candidate passes correlation but may pass fully.
	// Varied zigzag series so returns are non-degenerate.
	down := make([]float64, 21)
	for i := range down {
		down[i] = 120 + float64(i%4)*3 - float64(i%3)*2
	}
	md2 := MarketData{
		Symbols: map[string]*SymbolData{
			"NEW": {Closes: closes, ATR14: 1},
			"OLD": {Closes: down, ATR14: 1},
		},
		Held: map[string]bool{"OLD": true},
	}
	for _, s := range md2.Symbols {
		s.Bars = flatBars(21, s.Closes[len(s.Closes)-1])
		for i := range s.Bars {
			s.Bars[i].Volume = 1000000
		}
	}
	sv2 := rb.Apply(Verdict{Symbol: "NEW", Action: ActionBuy}, 100000, md2)
	if sv2.Blocked {
		t.Fatalf("uncorrelated candidate should pass: %+v", sv2)
	}
}

func TestRiskBudgetLiquidityCap(t *testing.T) {
	rc := DefaultRiskConfig()
	rc.PositionPct, rc.MaxPositionUSD = 0.05, 100000
	rb := NewRiskBudget(rc)
	// Avg dollar volume 20*100*100 = 200k -> 1% cap = 2000.
	// Price 100 -> max 20 shares; legacy would allow more.
	sd := &SymbolData{ATR14: 0.1}
	sd.Bars = flatBars(25, 100)
	sd.Closes = Closes(sd.Bars)
	for i := range sd.Bars {
		sd.Bars[i].Volume = 10000 // ADV$ = 1M -> liquidity cap = 10k = 100 shares
	}
	sv := rb.Apply(Verdict{Symbol: "L", Action: ActionBuy}, 1000000, mdOf("L", sd))
	if sv.Qty != 100 {
		t.Fatalf("liquidity cap should bind at 100 shares, got %d (%+v)", sv.Qty, sv)
	}
}

func TestVolRegimeAssessment(t *testing.T) {
	// Calm benchmark: flat bars -> LowVol.
	calm := flatBars(volRegimeMinBars+20, 100)
	setVolume(calm, 0)
	for i := range calm {
		calm[i].Volume = 1000
	}
	a := AssessVolRegime(mdOf(defBenchmark, &SymbolData{Bars: calm, Closes: Closes(calm)}).WithBenchmark(defBenchmark))
	if a.Level != VolLow {
		t.Fatalf("calm tape want LowVol got %+v", a)
	}

	// Crisis: long calm history then explosive final stretch drives the
	// latest ATR above the 90th percentile of its own history.
	bars := flatBars(volRegimeMinBars+20, 100)
	setVolume(bars, 0)
	for i := len(bars) - 15; i < len(bars); i++ {
		bars[i].High = 130
		bars[i].Low = 70
		bars[i].Close = 100 + float64(i%3)
	}
	closes := Closes(bars)
	a2 := AssessVolRegime(MarketData{Symbols: map[string]*SymbolData{
		defBenchmark: {Bars: bars, Closes: closes}}, Held: map[string]bool{}}.WithBenchmark(defBenchmark))
	if a2.Level != VolCrisis && a2.Level != VolRising {
		t.Fatalf("wild ATR should elevate regime, got %+v", a2)
	}
}
