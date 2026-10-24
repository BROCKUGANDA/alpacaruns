package risk

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/config"
)

type fakeKill struct{ halted bool }

func (f fakeKill) Halted() bool { return f.halted }

type fakeClock struct {
	open         bool
	sessionOpen  bool
	sessionKnown bool
}

func (f fakeClock) MarketOpen() bool          { return f.open }
func (f fakeClock) SessionOpen(Proposal) bool { return f.sessionOpen }
func (f fakeClock) SessionKnown() bool        { return f.sessionKnown }

func baseCfg() *config.Config {
	return &config.Config{
		MinConfidence:   0.7,
		MaxPositionUSD:  10000,
		MaxPortfolioPct: 0.20,
	}
}

func conf(c float64) *float64 { return &c }

// newValidator builds a validator with fixed live state for tests.
func newValidator(cfg *config.Config, kill HaltSource, clock MarketClock, equity float64, positions map[string]float64) *Validator {
	v := &Validator{
		Cfg:   cfg,
		Kill:  kill,
		Clock: clock,
		Portfolio: func() (Portfolio, error) {
			return Portfolio{Equity: equity, PortfolioValue: equity}, nil
		},
		// A fixed price keeps sizing deterministic: a qty-only proposal
		// is priced out into USD before the caps are applied.
		Prices: staticPrices{price: 100},
	}
	if positions != nil {
		v.Positions = func(sym string) float64 { return positions[sym] }
	} else {
		// An empty book is wired explicitly rather than left nil: a nil
		// source now means "unknown", and the gate refuses rather than
		// sizing against a book it never read. Tests that want a flat book
		// say so.
		v.Positions = func(string) float64 { return 0 }
	}
	return v
}

// staticPrices is a PriceSource that always answers the same price.
type staticPrices struct{ price float64 }

func (s staticPrices) Price(context.Context, string, bool) (float64, error) {
	return s.price, nil
}

func TestValidateTable(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *config.Config
		halted   bool
		open     bool
		equity   float64
		pos      map[string]float64
		p        Proposal
		approved bool
		wantSub  string // substring expected in rejection reasons
	}{
		{
			name: "clean buy passes",
			cfg:  baseCfg(), halted: false, open: true, equity: 100000,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9)},
			approved: true,
		},
		{
			name: "notional over per-position cap rejected",
			cfg:  baseCfg(), halted: false, open: true, equity: 100000,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "20000", Confidence: conf(0.9)},
			approved: false, wantSub: "per-position cap",
		},
		{
			name: "confidence below minimum rejected",
			cfg:  baseCfg(), halted: false, open: true, equity: 100000,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "1000", Confidence: conf(0.5)},
			approved: false, wantSub: "confidence 0.50 < minimum 0.70",
		},
		{
			name: "missing confidence rejected",
			cfg:  baseCfg(), halted: false, open: true, equity: 100000,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "1000"},
			approved: false, wantSub: "missing confidence",
		},
		{
			name: "kill switch halts everything",
			cfg:  baseCfg(), halted: true, open: true, equity: 100000,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "10", Confidence: conf(0.99)},
			approved: false, wantSub: "kill switch engaged",
		},
		{
			name: "market closed rejects",
			cfg:  baseCfg(), halted: false, open: false, equity: 100000,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "10", Confidence: conf(0.99)},
			approved: false, wantSub: "market closed",
		},
		{
			name: "pct cap breached by notional alone",
			cfg:  baseCfg(), halted: false, open: true, equity: 20000, // 10000 = 50% > 20%
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "10000", Confidence: conf(0.9)},
			approved: false, wantSub: "exceeds cap 20.0%",
		},
		{
			name: "pct cap breached additively with existing position",
			cfg:  baseCfg(), halted: false, open: true, equity: 100000,
			pos:      map[string]float64{"AAPL": 15000},                                              // already 15%
			p:        Proposal{Symbol: "aapl", Side: "buy", Notional: "8000", Confidence: conf(0.9)}, // +8% -> 23% > 20%
			approved: false, wantSub: "combined exposure",
		},
		{
			name: "existing position on other symbol does not block",
			cfg:  baseCfg(), halted: false, open: true, equity: 100000,
			pos:      map[string]float64{"MSFT": 15000},
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9)},
			approved: true,
		},
		{
			name: "invalid side rejected",
			cfg:  baseCfg(), halted: false, open: true, equity: 100000,
			p:        Proposal{Symbol: "AAPL", Side: "hold", Notional: "1000", Confidence: conf(0.9)},
			approved: false, wantSub: `invalid side`,
		},
		{
			name: "missing symbol rejected",
			cfg:  baseCfg(), halted: false, open: true, equity: 100000,
			p:        Proposal{Symbol: "", Side: "buy", Notional: "1000", Confidence: conf(0.9)},
			approved: false, wantSub: "missing symbol",
		},
		{
			name: "zero notional rejected",
			cfg:  baseCfg(), halted: false, open: true, equity: 100000,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "abc", Confidence: conf(0.9)},
			approved: false, wantSub: "no positive qty or notional",
		},
		{
			name: "boundary notional exactly at caps passes",
			cfg:  baseCfg(), halted: false, open: true, equity: 50000, // 10000 = exactly 20%
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "10000", Confidence: conf(0.7)},
			approved: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newValidator(tt.cfg, fakeKill{tt.halted}, fakeClock{open: tt.open, sessionKnown: true}, tt.equity, tt.pos)
			got := v.Validate(tt.p)
			if got.Approved != tt.approved {
				t.Fatalf("approved = %v (%v), want %v", got.Approved, got.Reasons, tt.approved)
			}
			if !tt.approved && !strings.Contains(got.String(), tt.wantSub) {
				t.Fatalf("verdict %q missing %q", got.String(), tt.wantSub)
			}
		})
	}
}

func TestValidateFailsClosedOnAccountError(t *testing.T) {
	v := &Validator{
		Cfg:   baseCfg(),
		Kill:  fakeKill{false},
		Clock: fakeClock{open: true, sessionKnown: true},
		Portfolio: func() (Portfolio, error) {
			return Portfolio{}, errors.New("alpaca down")
		},
	}
	got := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Notional: "1", Confidence: conf(0.99)})
	if got.Approved {
		t.Fatal("must fail closed when account is unavailable")
	}
	if !strings.Contains(got.String(), "account unavailable") {
		t.Fatalf("unexpected verdict: %q", got.String())
	}
}

// TestNilOptionalDependencies pins what "no exposure source" MEANS.
//
// A nil source used to be read as a flat book, so the portfolio-percentage
// cap evaluated the new order alone and approved it — which is precisely
// the behaviour that lets twenty positions each under 20% produce a 400%
// book while every check reports green. An unreadable book is not an empty
// book: with no way to know what is already committed, the only honest
// verdict is refusal.
func TestNilOptionalDependencies(t *testing.T) {
	v := &Validator{
		Cfg: baseCfg(),
		Portfolio: func() (Portfolio, error) {
			return Portfolio{Equity: 100000}, nil
		},
	}
	got := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9)})
	if got.Approved {
		t.Fatal("approved an order it could not size against the book: the additive cap was silently skipped")
	}
	if len(got.Reasons) == 0 || got.Reasons[0] == "" {
		t.Fatalf("refusal carried no reason: %+v", got)
	}
	// The other caps still bite once a book is actually wired.
	v.Positions = func(string) float64 { return 0 }
	if ok := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9)}); !ok.Approved {
		t.Fatalf("a flat, readable book must still allow a sized order: %v", ok.Reasons)
	}
	if bad := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Notional: "999999", Confidence: conf(0.9)}); bad.Approved {
		t.Fatal("cap must still apply without optional deps")
	}
	// And a book that already holds exposure is counted, not ignored.
	v.Positions = func(string) float64 { return 90000 }
	if crowd := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Notional: "20000", Confidence: conf(0.9)}); crowd.Approved {
		t.Fatal("combined exposure over the portfolio cap was approved")
	}
}

// A proven reducing order is exempt from exactly the policies that exist to
// stop NEW risk, and is still refused when it would oversell.
func TestReducingOrdersAreProvenNotTrusted(t *testing.T) {
	hold := map[string]float64{"AAPL": 10, "TSLA": -4}
	newV := func(halted bool, holdings map[string]float64) *Validator {
		k := &fakeKill{halted: halted}
		v := &Validator{
			Cfg:  baseCfg(),
			Kill: k,
			Portfolio: func() (Portfolio, error) {
				return Portfolio{Equity: 100000}, nil
			},
			Positions: func(string) float64 { return 0 },
			Factors: &fakeScorer{result: FactorResult{
				Composite: 0.1, MinScore: 0.6, Passed: false,
				Factors: map[string]float64{"trend": 0.1},
			}},
		}
		if holdings != nil {
			v.Holdings = func(sym string) (float64, error) {
				q, ok := holdings[sym]
				if !ok {
					return 0, nil
				}
				return q, nil
			}
		}
		return v
	}

	// Halted, weak factors, no confidence at all: an exit still goes.
	halt := newV(true, hold)
	got := halt.Validate(Proposal{Symbol: "AAPL", Side: "sell", Qty: "10", Reducing: true})
	if !got.Approved {
		t.Fatalf("a flatten was refused while halted: %v", got.Reasons)
	}

	// Oversell: held 10, asked to sell 11.
	if bad := halt.Validate(Proposal{Symbol: "AAPL", Side: "sell", Qty: "11", Reducing: true}); bad.Approved {
		t.Fatalf("overselling exit approved: %v", bad.Reasons)
	}
	// Wrong direction: short TSLA, so a reducing SELL would ADD exposure.
	if bad := halt.Validate(Proposal{Symbol: "TSLA", Side: "sell", Qty: "4", Reducing: true}); bad.Approved {
		t.Fatalf("a short-side sell was approved as reducing: %v", bad.Reasons)
	}
	// Covering the short is the reducing direction.
	if ok := halt.Validate(Proposal{Symbol: "TSLA", Side: "buy", Qty: "4", Reducing: true}); !ok.Approved {
		t.Fatalf("covering a short refused: %v", ok.Reasons)
	}
	// No position to reduce.
	if bad := halt.Validate(Proposal{Symbol: "NVDA", Side: "sell", Qty: "5", Reducing: true}); bad.Approved {
		t.Fatal("a reducing order against a flat position was approved")
	}
	// The assertion is worthless without a book to check it against.
	blind := newV(false, nil)
	if bad := blind.Validate(Proposal{Symbol: "AAPL", Side: "sell", Qty: "5", Reducing: true}); bad.Approved {
		t.Fatal("reducing accepted with no holdings source wired")
	}
	// An UNproven reducing claim does not buy the exemptions for an entry:
	// a buy with Reducing asserted against a long position is refused above,
	// and a plain buy while halted is still refused.
	if bad := halt.Validate(Proposal{Symbol: "AAPL", Side: "buy", Qty: "1", Confidence: conf(0.9)}); bad.Approved {
		t.Fatal("new exposure approved while the kill switch is engaged")
	}
}

// TestQtyIsPricedIntoNotional is the regression guard for the sizing
// hole where a share count was used AS a dollar amount: a 1,000-share
// order at $100/share looked like a $1,000 order and sailed under both
// the per-position and the portfolio-percentage caps.
func TestQtyIsPricedIntoNotional(t *testing.T) {
	v := newValidator(baseCfg(), fakeKill{false}, fakeClock{open: true, sessionKnown: true}, 100000, nil)
	v.Prices = staticPrices{price: 100}

	// 1,000 shares x $100 = $100,000 — far above the $10,000 cap.
	got := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Qty: "1000", Confidence: ptr(0.9)})
	if got.Approved {
		t.Fatal("a 1,000-share order must not be approved as a $1,000 order")
	}
	if got.Notional != 100000 {
		t.Fatalf("notional = %v, want 100000 (qty priced at 100)", got.Notional)
	}
	if !strings.Contains(got.String(), "exceeds per-position cap") {
		t.Fatalf("verdict must name the cap that rejected it: %q", got.String())
	}

	// 10 shares x $100 = $1,000 — inside the caps, so it must pass.
	ok := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Qty: "10", Confidence: ptr(0.9)})
	if !ok.Approved {
		t.Fatalf("a $1,000 order must be approved: %s", ok.String())
	}
	if ok.Notional != 1000 {
		t.Fatalf("notional = %v, want 1000", ok.Notional)
	}
}

// TestUnpriceableQtyFailsClosed proves a proposal whose size cannot be
// priced is rejected instead of passing the caps unchecked.
func TestUnpriceableQtyFailsClosed(t *testing.T) {
	v := newValidator(baseCfg(), fakeKill{false}, fakeClock{open: true, sessionKnown: true}, 100000, nil)
	v.Prices = errorPrices{}
	got := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Qty: "10", Confidence: ptr(0.9)})
	if got.Approved {
		t.Fatal("an unpriceable order must not be approved")
	}
	if !strings.Contains(got.String(), "price unavailable") {
		t.Fatalf("verdict must name the price failure: %q", got.String())
	}
}

type errorPrices struct{}

func (errorPrices) Price(context.Context, string, bool) (float64, error) {
	return 0, errors.New("no price")
}
