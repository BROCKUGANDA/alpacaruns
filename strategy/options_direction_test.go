package strategy

// options_direction_test.go: the planner must pick the contract type the
// caller asked for, and must not fall back to the other one.

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/options"
)

func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}

type fakeChain struct{ contracts []options.Contract }

func (f fakeChain) GetContracts(_ context.Context, q options.ContractsQuery) ([]options.Contract, string, error) {
	var out []options.Contract
	for _, c := range f.contracts {
		if q.Type == "" || c.Type == q.Type {
			out = append(out, c)
		}
	}
	return out, "", nil
}

func (f fakeChain) GetSnapshots(_ context.Context, symbols []string) (map[string]options.Snapshot, error) {
	out := map[string]options.Snapshot{}
	for _, s := range symbols {
		out[s] = options.Snapshot{
			LatestQuote: options.Quote{BidPrice: 9.9, AskPrice: 10.1},
			Greeks:      options.Greeks{Delta: -0.65}, // puts report a negative delta
		}
	}
	return out, nil
}

func TestPlannerSelectsTheRequestedType(t *testing.T) {
	chain := fakeChain{contracts: []options.Contract{
		{Symbol: "AAPL260116C00100000", Type: "call", Tradable: true, StrikePriceRaw: "100", Size: "100"},
		{Symbol: "AAPL260116P00100000", Type: "put", Tradable: true, StrikePriceRaw: "100", Size: "100"},
	}}
	p := &OptionPlanner{
		Opts: chain, DeltaMin: 0.5, DeltaMax: 0.8,
		MinDTE: 30, MaxDTE: 45, Sizing: Sizing{PositionPct: 0.05, MaxPositionUSD: 10000}, Log: testLogger(t),
	}

	call, why := p.PlanDirection(context.Background(), "AAPL", 100000, "call")
	if call == nil {
		t.Fatalf("call not planned: %s", why)
	}
	if call.Type != "call" || call.OCC != "AAPL260116C00100000" {
		t.Fatalf("wrong call selected: %+v", call)
	}

	put, why := p.PlanDirection(context.Background(), "AAPL", 100000, "put")
	if put == nil {
		t.Fatalf("put not planned: %s", why)
	}
	if put.Type != "put" || put.OCC != "AAPL260116P00100000" {
		t.Fatalf("wrong put selected: %+v", put)
	}
	if put.Delta <= 0 {
		t.Fatalf("put delta should be reported as a magnitude: %v", put.Delta)
	}
	// Premium is per CONTRACT (quote x 100), not per share.
	if put.Premium != 1000 {
		t.Fatalf("premium = %v, want 1000 per contract", put.Premium)
	}

	// Buying a call for a bullish view stays the default.
	def, _ := p.Plan(context.Background(), "AAPL", 100000)
	if def == nil || def.Type != "call" {
		t.Fatalf("Plan() should default to a call: %+v", def)
	}
}

func TestPlannerRejectsUnknownKindAndMissingType(t *testing.T) {
	p := &OptionPlanner{
		Opts: fakeChain{contracts: []options.Contract{
			{Symbol: "AAPL260116C00100000", Type: "call", Tradable: true, Size: "100"},
		}},
		DeltaMin: 0.5, DeltaMax: 0.8, MinDTE: 30, MaxDTE: 45,
		Sizing: Sizing{PositionPct: 0.05, MaxPositionUSD: 10000}, Log: testLogger(t),
	}
	if leg, _ := p.PlanDirection(context.Background(), "AAPL", 100000, "straddle"); leg != nil {
		t.Fatal("an unknown option kind produced a leg")
	}
	// Asking for a put when the chain has none must fail, not fall back to a call.
	if leg, why := p.PlanDirection(context.Background(), "AAPL", 100000, "put"); leg != nil {
		t.Fatalf("fell back to a call when asked for a put: %+v (%s)", leg, why)
	}
}
