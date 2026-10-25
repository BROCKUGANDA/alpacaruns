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

// TestExtendedHoursOrderClassRules pins the two refusals the broker issues at
// any hour, independent of the session: an order that carries protection legs
// cannot be an extended-hours order, so a bracket or OCO proposal asking for
// the extended session is refused here rather than 403'd at the wire on every
// tick.
//
// Measured against the paper API on 2026-10-25:
//
//	order_class=bracket + extended_hours -> HTTP 403 40310000
//	    "bracket orders do not support extended hours trading"
//	order_class=oco + extended_hours     -> HTTP 403 40310000
//	    "oco orders do not support extended hours trading"
func TestExtendedHoursOrderClassRules(t *testing.T) {
	cases := []struct {
		name     string
		p        Proposal
		open     bool
		approved bool
		wantSub  string
	}{
		{
			name:     "bracket refused in the extended session",
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9), OrderType: "limit", TimeInForce: "day", ExtendedHours: true, OrderClass: "bracket"},
			open:     false,
			approved: false, wantSub: "cannot use order_class=bracket",
		},
		{
			name:     "oco refused in the extended session",
			p:        Proposal{Symbol: "AAPL", Side: "sell", Qty: "5", Confidence: conf(0.9), OrderType: "limit", TimeInForce: "day", ExtendedHours: true, OrderClass: "oco", Reducing: true},
			open:     false,
			approved: false, wantSub: "cannot use order_class=oco",
		},
		{
			// The refusal is about the order, not the clock: asking for the
			// extended session while the regular market is open is the same
			// impossible shape, and catching it here is what keeps a doomed
			// order from being re-tried all day.
			name:     "bracket refused while the market is open",
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9), OrderType: "limit", TimeInForce: "gtc", ExtendedHours: true, OrderClass: "bracket"},
			open:     true,
			approved: false, wantSub: "cannot use order_class=bracket",
		},
		{
			name:     "market type refused at any hour",
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9), OrderType: "market", TimeInForce: "day", ExtendedHours: true},
			open:     true,
			approved: false, wantSub: "must be type=limit",
		},
		{
			name:     "plain limit/day allowed while open",
			p:        Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9), OrderType: "limit", TimeInForce: "day", ExtendedHours: true, OrderClass: "simple"},
			open:     true,
			approved: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newValidator(extCfg(), fakeKill{false}, fakeClock{
				open: tc.open, sessionOpen: true, sessionKnown: true,
			}, 100000, nil)
			// A reducing order's claim is proved against the holdings hook,
			// or the gate refuses it for a reason unrelated to the class rule.
			v.Holdings = func(string) (float64, error) { return 5, nil }
			got := v.Validate(tc.p)
			if got.Approved != tc.approved {
				t.Fatalf("approved = %v (%v), want %v", got.Approved, got.Reasons, tc.approved)
			}
			if !tc.approved && !containsReason(got.Reasons, tc.wantSub) {
				t.Fatalf("verdict %q missing %q", got.String(), tc.wantSub)
			}
		})
	}
}

// TestSessionClockAdapters documents that the calendar-backed clock is the one
// the live paths use; the weekday approximation stays as the fallback for a
// caller that only has a timestamp, and a holiday is exactly where the two
// disagree.
func TestSessionClockAdaptersDocumentFallback(t *testing.T) {
	// Tuesday 05:00: the approximation says extended.
	if !InExtendedSession(time.Tuesday, 5, 0) {
		t.Fatal("weekday approximation should report Tuesday 05:00 as extended")
	}
	// A calendar-driven source is the one that knows it is a holiday; that
	// knowledge lives in tools.Sessions, and risk.SessionClock is the adapter
	// the gate takes. This test exists so the fallback is not mistaken for
	// the authority.
	if SessionClock(nil) == nil {
		t.Fatal("SessionClock must return a usable clock even without a source (it fails closed)")
	}
	c := SessionClock(nil)
	if c.MarketOpen() || c.SessionOpen(Proposal{}) || c.SessionKnown() {
		t.Fatal("a nil session source must fail closed on every question")
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
