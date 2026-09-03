package risk

import (
	"testing"
)

// Pre-orders: with PRE_ORDERS on, a resting limit/GTC equity order is
// accepted while the market is closed; market orders and non-enabled
// configs still reject. Fail-closed preserved for unknown clock state.
func TestPreOrderGate(t *testing.T) {
	newV := func(pre bool) *Validator {
		v := newValidator(extCfg(), fakeKill{false}, fakeClock{open: false, sessionKnown: true}, 100000, nil)
		v.Cfg.PreOrders = pre
		return v
	}
	limit := Proposal{Symbol: "AAPL", Side: "buy", Qty: "1",
		Confidence: ptr(0.9), OrderType: "limit", TimeInForce: "gtc"}
	mkt := limit
	mkt.OrderType = "market"

	if v := newV(true).Validate(limit); !v.Approved {
		t.Fatalf("PRE_ORDERS should accept closed-market limit/gtc: %v", v.Reasons)
	}
	if v := newV(false).Validate(limit); v.Approved {
		t.Fatal("without PRE_ORDERS, closed-market order must reject")
	}
	if v := newV(true).Validate(mkt); v.Approved {
		t.Fatal("market orders must reject while closed even with PRE_ORDERS")
	}
	ioc := limit
	ioc.TimeInForce = "ioc"
	if v := newV(true).Validate(ioc); v.Approved {
		t.Fatal("pre-orders require day|gtc")
	}
	opt := limit
	opt.Symbol = "AAPL240119C00100000"
	if v := newV(true).Validate(opt); v.Approved {
		t.Fatal("options must not pass the pre-order path")
	}
}

func ptr(f float64) *float64 { return &f }
