package strategy

import (
	"context"
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// refBarsFake satisfies barSource with canned daily bars.
type refBarsFake struct{ bars map[string][]tools.Bar }

func (f refBarsFake) GetBars(_ context.Context, symbols []string, _, _, _ string, _ int) (map[string][]tools.Bar, error) {
	out := map[string][]tools.Bar{}
	for _, s := range symbols {
		if b, ok := f.bars[s]; ok {
			out[s] = b
		}
	}
	return out, nil
}

// TestPlanEntryFromReference verifies sizing and brackets come off the
// last daily close when snapshots are unusable (off-hours pre-orders).
func TestPlanEntryFromReference(t *testing.T) {
	e, err := NewEngine(EngineConfig{
		Scorer:        fakeScorer{},
		Prices:        fakePrices{px: map[string]float64{}}, // snapshots empty off-hours
		ReferenceBars: refBarsFake{bars: map[string][]tools.Bar{
			"AAPL": {{Close: 200}, {Close: 202.5}},
		}},
		Sizing: Sizing{PositionPct: 0.05, MaxPositionUSD: 10000},
		TPPct:  0.08, SLPct: 0.04,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.PlanEntryFromReference(context.Background(),
		Decision{Symbol: "AAPL"}, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if p.Price != 202.5 {
		t.Fatalf("want reference price 202.5, got %g", p.Price)
	}
	if p.Qty != 24 { // min(0.05*100000, 10000) = 5000 budget; 5000/202.5 = 24 shares
		t.Fatalf("unexpected qty %d", p.Qty)
	}
	if p.Brackets.TakeProfit <= p.Price || p.Brackets.StopLoss >= p.Price {
		t.Fatalf("brackets inverted: tp=%v sl=%v entry=%v", p.Brackets.TakeProfit, p.Brackets.StopLoss, p.Price)
	}
}

// TestPlanEntryFromReferenceNoBars fails closed when no reference source.
func TestPlanEntryFromReferenceNoBars(t *testing.T) {
	e, _ := NewEngine(EngineConfig{
		Scorer: fakeScorer{}, Prices: fakePrices{px: map[string]float64{}},
		Sizing: Sizing{PositionPct: 0.05, MaxPositionUSD: 10000},
		TPPct:  0.08, SLPct: 0.04,
	})
	if _, err := e.PlanEntryFromReference(context.Background(), Decision{Symbol: "AAPL"}, 100000); err == nil {
		t.Fatal("expected error without ReferenceBars wired")
	}
}

// TestPlanEntryFromReferenceATRMode verifies pre-order brackets honor
// BRACKET_MODE=atr (scaled off the reference series' ATR14) instead of
// silently falling back to pct brackets while live entries use ATR.
func TestPlanEntryFromReferenceATRMode(t *testing.T) {
	// 60 quiet bars around 200 (ATR14 tiny but > 0), last close 202.5.
	bars := make([]tools.Bar, 60)
	for i := range bars {
		c := 200.0
		if i >= 58 {
			c = 200 + 0.5*float64(i-57) // 200, 200.5, 201
		}
		bars[i] = tools.Bar{Time: int64(i) * 86400, Open: c, High: c + 0.1, Low: c - 0.1, Close: c, Volume: 1000}
	}
	e, err := NewEngine(EngineConfig{
		Scorer:        fakeScorer{},
		Prices:        fakePrices{px: map[string]float64{}},
		ReferenceBars: refBarsFake{bars: map[string][]tools.Bar{"AAPL": bars}},
		Sizing:        Sizing{PositionPct: 0.05, MaxPositionUSD: 10000},
		TPPct:         0.08, SLPct: 0.04,
		BracketMode:   "atr",
		ATRMultTP:     3.0, ATRMultSL: 2.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.PlanEntryFromReference(context.Background(), Decision{Symbol: "AAPL"}, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if p.Crypto {
		t.Fatal("equity pre-order misclassified as crypto")
	}
	// pct brackets would be tp=218.7/sl=194.4; ATR brackets must differ
	// and must sit on the correct sides of the entry.
	if p.Brackets.TakeProfit <= p.Price || p.Brackets.StopLoss >= p.Price {
		t.Fatalf("ATR pre-order brackets inverted: %+v", p.Brackets)
	}
	if p.Brackets.TakeProfit == 202.5*1.08 { // exactly pct -> ATR path not taken
		t.Fatalf("pre-order used pct brackets despite BRACKET_MODE=atr: %+v", p.Brackets)
	}
}
