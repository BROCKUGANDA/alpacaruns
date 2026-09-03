package risk

import (
	"strings"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
)

// extCfg returns a config with EXTENDED_HOURS enabled.
func extCfg() *config.Config {
	c := baseCfg()
	c.ExtendedHours = true
	return c
}

// TestExtendedHoursGate covers the nuanced market-hours behavior: with
// EXTENDED_HOURS on, limit/day|gtc orders pass during the approximated US
// extended session (weekdays 04:00-20:00 ET); market orders, exotic TIFs,
// weekends and unknown clock state still reject (fail closed).
func TestExtendedHoursGate(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *config.Config
		open      bool
		sessOpen  bool
		sessKnown bool
		p         Proposal
		approved  bool
		wantSub   string
	}{
		{
			name: "limit+gtc accepted in extended session",
			cfg:  extCfg(), open: false, sessOpen: true, sessKnown: true,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9), OrderType: "limit", TimeInForce: "gtc"},
			approved: true,
		},
		{
			name: "limit+day accepted in extended session",
			cfg:  extCfg(), open: false, sessOpen: true, sessKnown: true,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9), OrderType: "limit", TimeInForce: "day"},
			approved: true,
		},
		{
			name: "market order rejected in extended session",
			cfg:  extCfg(), open: false, sessOpen: true, sessKnown: true,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9), OrderType: "market", TimeInForce: "gtc"},
			approved: false, wantSub: "extended-hours orders must be type=limit",
		},
		{
			name: "ioc tif rejected in extended session",
			cfg:  extCfg(), open: false, sessOpen: true, sessKnown: true,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9), OrderType: "limit", TimeInForce: "ioc"},
			approved: false, wantSub: "time_in_force=day|gtc",
		},
		{
			name: "weekend rejected even for valid combo",
			cfg:  extCfg(), open: false, sessOpen: false, sessKnown: true,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9), OrderType: "limit", TimeInForce: "gtc"},
			approved: false, wantSub: "outside extended-hours window",
		},
		{
			name: "unknown session fails closed",
			cfg:  extCfg(), open: false, sessOpen: false, sessKnown: false,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9), OrderType: "limit", TimeInForce: "gtc"},
			approved: false, wantSub: "market closed",
		},
		{
			name: "extended hours off keeps plain rejection",
			cfg:  baseCfg(), open: false, sessOpen: true, sessKnown: true,
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9), OrderType: "limit", TimeInForce: "gtc"},
			approved: false, wantSub: "market closed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newValidator(tt.cfg, fakeKill{false}, fakeClock{
				open:         tt.open,
				sessionOpen:  tt.sessOpen,
				sessionKnown: tt.sessKnown,
			}, 100000, nil)
			got := v.Validate(tt.p)
			if got.Approved != tt.approved {
				t.Fatalf("approved = %v (%v), want %v", got.Approved, got.Reasons, tt.approved)
			}
			if !tt.approved && !containsReason(got.Reasons, tt.wantSub) {
				t.Fatalf("verdict %q missing %q", got.String(), tt.wantSub)
			}
		})
	}
}

func containsReason(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

// TestSessionWindowMath exercises the weekday/hour approximation directly.
func TestSessionWindowMath(t *testing.T) {
	tests := []struct {
		name    string
		weekday time.Weekday
		hour    int
		minute  int
		want    bool
	}{
		{"before open", time.Monday, 3, 59, false},
		{"window opens", time.Monday, 4, 0, true},
		{"premarket mid", time.Wednesday, 6, 30, true},
		{"friday evening inside", time.Friday, 19, 59, true},
		{"after close", time.Friday, 20, 0, false},
		{"saturday never", time.Saturday, 12, 0, false},
		{"sunday never", time.Sunday, 10, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InExtendedSession(tt.weekday, tt.hour, tt.minute); got != tt.want {
				t.Errorf("InExtendedSession(%v, %d:%02d) = %v, want %v",
					tt.weekday, tt.hour, tt.minute, got, tt.want)
			}
		})
	}
}
