package risk

import (
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
	}
	if positions != nil {
		v.Positions = func(sym string) float64 { return positions[sym] }
	}
	return v
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

func TestNilOptionalDependencies(t *testing.T) {
	// No kill switch, no clock, no positions: remaining checks still run.
	v := &Validator{
		Cfg: baseCfg(),
		Portfolio: func() (Portfolio, error) {
			return Portfolio{Equity: 100000}, nil
		},
	}
	if got := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9)}); !got.Approved {
		t.Fatalf("expected approval, got: %v", got.Reasons)
	}
	if got := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Notional: "999999", Confidence: conf(0.9)}); got.Approved {
		t.Fatal("cap must still apply without optional deps")
	}
}
