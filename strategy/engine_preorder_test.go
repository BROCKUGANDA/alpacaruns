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
