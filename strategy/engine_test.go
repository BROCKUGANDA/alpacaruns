package strategy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// ---- fakes ----

type fakeScorer struct {
	res map[string]factorsScored
	err map[string]error
}

func (f fakeScorer) ScoreFactorsFlat(_ context.Context, symbol string) (factorsScored, error) {
	if err, ok := f.err[symbol]; ok {
		return factorsScored{}, err
	}
	return f.res[symbol], nil
}

type fakePrices struct{ px map[string]float64 }

func (f fakePrices) Price(_ context.Context, symbol string) (float64, error) {
	if p, ok := f.px[symbol]; ok {
		return p, nil
	}
	return 0, errNoPrice
}

var errNoPrice = &testErr{"no price"}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }

// ---- decision thresholds: both sides of every boundary ----

func TestDecideThresholds(t *testing.T) {
	th := Thresholds{FactorMinScore: 0.6, TrendBuy: 0.7, MomentumBuy: 0.6,
		ExitComposite: 0.4, ExitMomentum: 0.35}

	tests := []struct {
		name      string
		held      bool
		composite float64
		trend     float64
		momentum  float64
		want      Signal
	}{
		{"buy at exact boundaries", false, 0.6, 0.7, 0.6, SignalBuy},
		{"buy above boundaries", false, 0.9, 0.8, 0.9, SignalBuy},
		{"hold composite below min", false, 0.59, 0.9, 0.9, SignalHold},
		{"hold trend below threshold", false, 0.9, 0.69, 0.9, SignalHold},
		{"hold momentum below threshold", false, 0.9, 0.9, 0.59, SignalHold},
		{"sell composite at exit boundary", true, 0.4, 0.9, 0.9, SignalSell},
		{"sell momentum at exit boundary", true, 0.5, 0.9, 0.35, SignalSell},
		{"sell on composite alone", true, 0.3, 0.2, 0.1, SignalSell},
		{"held no trigger stays hold", true, 0.41, 0.9, 0.36, SignalHold},
		{"unheld weak tape never sell-signal", false, 0.1, 0.1, 0.1, SignalHold},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide("TEST", tt.held, tt.composite, tt.trend, tt.momentum, th)
			if got.Signal != tt.want {
				t.Fatalf("Decide() = %s (%s), want %s", got.Signal, got.Reason, tt.want)
			}
			// Determinism: same inputs -> identical verdict.
			again := Decide("TEST", tt.held, tt.composite, tt.trend, tt.momentum, th)
			if again.Signal != got.Signal || again.Reason != got.Reason {
				t.Fatalf("non-deterministic decision: %+v vs %+v", again, got)
			}
		})
	}
}

// ---- sizing math incl. cap and min-qty skip ----

func TestSizing(t *testing.T) {
	s := Sizing{PositionPct: 0.05, MaxPositionUSD: 10000}

	tests := []struct {
		name       string
		pfv, price float64
		sizing     *Sizing
		wantQty    int
		wantBudget float64
		wantSkip   bool
	}{
		{"basic fixed-fractional", 100000, 50, nil, 100, 5000, false},
		{"cap binds below pct", 1000000, 10, nil, 1000, 10000, false},
		{"floor rounds down", 99999, 33, nil, 151, 0.05 * 99999, false},
		{"skip when qty < 1", 1000, 400, nil, 0, 50, true},
		{"zero price skips", 100000, 0, nil, 0, 0, true},
		{"zero pfv skips", 0, 10, nil, 0, 0, true},
		{"tiny cap still respects floor", 20000, 1500, &Sizing{PositionPct: 0.05, MaxPositionUSD: 100}, 0, 100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sz := s
			if tt.sizing != nil {
				sz = *tt.sizing
			}
			got := sz.SizeForPrice(tt.pfv, tt.price)
			if got.Qty != tt.wantQty {
				t.Fatalf("Qty = %d, want %d (skip=%q)", got.Qty, tt.wantQty, got.Skip)
			}
			if tt.wantSkip && got.Skip == "" {
				t.Fatal("expected skip reason")
			}
			if !tt.wantSkip && tt.wantBudget > 0 && abs(got.Budget-tt.wantBudget) > 1e-9 {
				t.Fatalf("Budget = %.4f, want %.4f", got.Budget, tt.wantBudget)
			}
		})
	}
}

func TestOptionContractSizing(t *testing.T) {
	s := Sizing{PositionPct: 0.05, MaxPositionUSD: 10000}
	got := s.SizeOptionContract(100000, 250) // budget 5000 -> 20 contracts
	if got.Qty != 20 || abs(got.Budget-5000) > 1e-9 {
		t.Fatalf("got %+v, want 20 contracts within 5000", got)
	}
	// Premium above budget: never fractional contracts.
	if got := s.SizeOptionContract(1000, 800); got.Qty != 0 || got.Skip == "" {
		t.Fatalf("expected skip for unaffordable contract, got %+v", got)
	}
}

// ---- bracket math ----

func TestComputeBrackets(t *testing.T) {
	lv, err := ComputeBrackets(100, 0.08, 0.04)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lv.TakeProfit != 108 || lv.StopLoss != 96 {
		t.Fatalf("TP = %.4f SL = %.4f, want 108 / 96", lv.TakeProfit, lv.StopLoss)
	}
	if _, err := ComputeBrackets(100, 0.04, 0.08); err == nil {
		t.Fatal("expected error when TP <= SL")
	}
	if _, err := ComputeBrackets(100, 0.08, 0); err == nil {
		t.Fatal("expected error when SL = 0")
	}
}

// crypto no-bracket enforcement: PlanEntry must zero brackets for BASE/USD
// and populate them for equities.
func TestPlanEntryCryptoNoBracket(t *testing.T) {
	e, err := NewEngine(EngineConfig{
		Scorer: fakeScorer{res: map[string]factorsScored{
			"AAPL":    {Composite: .9, Trend: .9, Momentum: .9},
			"BTC/USD": {Composite: .9, Trend: .9, Momentum: .9},
		}},
		Prices:    fakePrices{px: map[string]float64{"AAPL": 100, "BTC/USD": 50000}},
		Threshold: Thresholds{FactorMinScore: 0.6, TrendBuy: 0.7, MomentumBuy: 0.6},
		Sizing:    Sizing{PositionPct: 0.05, MaxPositionUSD: 10000},
		TPPct:     0.08, SLPct: 0.04,
	})
	if err != nil {
		t.Fatal(err)
	}
	eq, err := e.PlanEntry(context.Background(), Decision{Symbol: "AAPL"}, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if eq.Brackets.TakeProfit != 108 || eq.Brackets.StopLoss != 96 {
		t.Fatalf("equity brackets wrong: %+v", eq.Brackets)
	}
	// Budget 5000 cannot buy a whole BTC at 50000: min-qty skip fires.
	if _, err := e.PlanEntry(context.Background(), Decision{Symbol: "BTC/USD"}, 100000); err == nil {
		t.Fatal("expected sizing skip for unaffordable whole BTC")
	} else if !strings.Contains(err.Error(), "below one unit") {
		t.Fatalf("skip reason should mention min-qty rule, got: %v", err)
	}
	// A lower crypto price (500, from the fake price table) is affordable
	// and must yield a bracket-free plan. The fake returns one price per
	// symbol, so use a second engine wired to the cheaper mark.
	e2, err := NewEngine(EngineConfig{
		Scorer: fakeScorer{res: map[string]factorsScored{
			"BTC/USD": {Composite: .9, Trend: .9, Momentum: .9},
		}},
		Prices:    fakePrices{px: map[string]float64{"BTC/USD": 500}},
		Threshold: Thresholds{FactorMinScore: 0.6, TrendBuy: 0.7, MomentumBuy: 0.6},
		Sizing:    Sizing{PositionPct: 0.05, MaxPositionUSD: 10000},
		TPPct:     0.08, SLPct: 0.04,
	})
	if err != nil {
		t.Fatal(err)
	}
	cr2, err := e2.PlanEntry(context.Background(), Decision{Symbol: "BTC/USD"}, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if !cr2.Crypto || cr2.Brackets.TakeProfit != 0 || cr2.Brackets.StopLoss != 0 {
		t.Fatalf("crypto must carry NO server-side brackets, got %+v", cr2)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// silence unused warnings for helpers exercised in other files' tests
var (
	_ = json.Marshal
	_ = os.ReadFile
	_ = filepath.Join
	_ = strings.ToUpper
	_ = time.Now
	_ = tools.Bar{}
)
