package risk

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakePrices is an injectable PriceSource for deterministic notional math.
type fakePrices struct {
	price float64
	err   error
	calls int
}

func (f *fakePrices) Price(ctx context.Context, symbol string, isOption bool) (float64, error) {
	f.calls++
	return f.price, f.err
}

// TestValidateOptionsNotionalMath checks the options premium-outlay
// formula: notional = qty x 100 (contract multiplier) x mark price, and
// that the resulting notional feeds the existing caps.
func TestValidateOptionsNotionalMath(t *testing.T) {
	tests := []struct {
		name         string
		qty          string
		price        float64
		maxPosition  float64
		wantApproved bool
		wantNotional float64
		wantReason   string // substring expected in rejection reasons
	}{
		{
			name:         "1 contract at $5 = $500 premium",
			qty:          "1",
			price:        5.0,
			maxPosition:  10000,
			wantApproved: true,
			wantNotional: 500,
		},
		{
			name:         "3 contracts at $12.25 = $3675",
			qty:          "3",
			price:        12.25,
			maxPosition:  10000,
			wantApproved: true,
			wantNotional: 3675,
		},
		{
			name:         "premium exceeds MaxPositionUSD cap",
			qty:          "10",
			price:        20.0,
			maxPosition:  10000, // 10x100x20 = 20000 > 10000
			wantApproved: false,
			wantNotional: 20000,
			wantReason:   "exceeds per-position cap",
		},
		{
			name:         "premium exceeds portfolio pct cap",
			qty:          "1",
			price:        50.0,
			maxPosition:  100000, // 5000 > 20% of 20000 equity
			wantApproved: false,
			wantNotional: 5000,
			wantReason:   "of portfolio",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseCfg()
			cfg.MaxPositionUSD = tt.maxPosition
			v := newValidator(cfg, fakeKill{}, fakeClock{open: true}, 20000, nil)
			ps := &fakePrices{price: tt.price}
			v.Prices = ps
			got := v.Validate(Proposal{
				Symbol:      "AAPL240119C00100000",
				Side:        "buy",
				Qty:         tt.qty,
				Confidence:  conf(0.9),
				OrderType:   "market",
				TimeInForce: "day",
			})
			if got.Approved != tt.wantApproved {
				t.Fatalf("approved = %v (%v), want %v", got.Approved, got.Reasons, tt.wantApproved)
			}
			if got.Notional != tt.wantNotional {
				t.Fatalf("notional = %.2f, want %.2f", got.Notional, tt.wantNotional)
			}
			if ps.calls != 1 {
				t.Fatalf("PriceSource calls = %d, want 1", ps.calls)
			}
			if tt.wantReason != "" && !strings.Contains(strings.Join(got.Reasons, "; "), tt.wantReason) {
				t.Fatalf("reasons %v missing substring %q", got.Reasons, tt.wantReason)
			}
		})
	}
}

// TestValidateOptionsFailsClosedOnPriceError verifies that a PriceSource
// error rejects the proposal — never approves blind.
func TestValidateOptionsFailsClosedOnPriceError(t *testing.T) {
	v := newValidator(baseCfg(), fakeKill{}, fakeClock{open: true}, 20000, nil)
	v.Prices = &fakePrices{err: errors.New("data outage")}
	got := v.Validate(Proposal{
		Symbol:      "AAPL240119P00100000",
		Side:        "buy",
		Qty:         "1",
		Confidence:  conf(0.9),
		OrderType:   "market",
		TimeInForce: "day",
	})
	if got.Approved {
		t.Fatal("approved despite price-source error; must fail closed")
	}
	if !strings.Contains(got.String(), "option price unavailable") {
		t.Fatalf("unexpected reasons: %v", got.Reasons)
	}
}

// TestValidateOptionsHardRules covers the client-side mirror of Alpaca's
// options validations inside the risk gate.
func TestValidateOptionsHardRules(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Proposal)
		wantReason string
	}{
		{
			name:       "notional rejected for options",
			mutate:     func(p *Proposal) { p.Notional = "500"; p.Qty = "" },
			wantReason: "not notional",
		},
		{
			name:       "extended hours rejected for options",
			mutate:     func(p *Proposal) { p.ExtendedHours = true },
			wantReason: "extended_hours",
		},
		{
			name:       "ioc tif rejected for options",
			mutate:     func(p *Proposal) { p.TimeInForce = "ioc" },
			wantReason: "time_in_force=day|gtc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newValidator(baseCfg(), fakeKill{}, fakeClock{open: true}, 20000, nil)
			v.Prices = &fakePrices{price: 5}
			p := Proposal{
				Symbol:      "AAPL240119C00100000",
				Side:        "buy",
				Qty:         "1",
				Confidence:  conf(0.9),
				OrderType:   "market",
				TimeInForce: "day",
			}
			tt.mutate(&p)
			got := v.Validate(p)
			if got.Approved {
				t.Fatalf("expected rejection, got approved")
			}
			if !strings.Contains(got.String(), tt.wantReason) {
				t.Fatalf("reasons %v missing %q", got.Reasons, tt.wantReason)
			}
		})
	}
}

// TestValidateEquityUnchangedByOptionsPath guards the legacy behavior:
// equity proposals must not consult the PriceSource.
func TestValidateEquityUnchangedByOptionsPath(t *testing.T) {
	v := newValidator(baseCfg(), fakeKill{}, fakeClock{open: true}, 20000, nil)
	ps := &fakePrices{price: 999}
	v.Prices = ps
	notional := "500"
	got := v.Validate(Proposal{
		Symbol:      "AAPL",
		Side:        "buy",
		Notional:    notional,
		Confidence:  conf(0.9),
		OrderType:   "market",
		TimeInForce: "gtc",
	})
	if !got.Approved {
		t.Fatalf("equity path broken: %v", got.Reasons)
	}
	if ps.calls != 0 {
		t.Fatalf("equity path consulted PriceSource %d times; should be 0", ps.calls)
	}
	if got.Notional != 500 {
		t.Fatalf("notional = %.2f, want explicit 500", got.Notional)
	}
}

// TestValidateOptionsMarketClosed ensures options are never allowed in
// extended sessions even when EXTENDED_HOURS is on.
func TestValidateOptionsMarketClosed(t *testing.T) {
	cfg := baseCfg()
	cfg.ExtendedHours = true
	v := newValidator(cfg, fakeKill{}, fakeClock{}, 20000, nil) // market closed
	v.Prices = &fakePrices{price: 5}
	got := v.Validate(Proposal{
		Symbol:      "AAPL240119C00100000",
		Side:        "buy",
		Qty:         "1",
		Confidence:  conf(0.9),
		OrderType:   "limit",
		TimeInForce: "day",
	})
	if got.Approved {
		t.Fatal("options order approved while market closed")
	}
	if !strings.Contains(got.String(), "market closed") {
		t.Fatalf("unexpected reasons: %v", got.Reasons)
	}
}
