package main

// extended_test.go pins the session route: what makes an entry eligible for
// the pre-market and after-hours session, and — the part that matters more —
// everything that refuses it. A route that guesses "live" when it cannot read
// the calendar is the failure mode this exists to prevent.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

var routeET = time.FixedZone("ET", -4*3600)

type routeBroker struct {
	calendar []map[string]any
	snap     map[string]any
	// clockOpen mirrors /v2/clock for the sessions-less fallback.
	clockOpen bool
}

func (b *routeBroker) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/calendar":
		if b.calendar == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(b.calendar)
	case r.URL.Path == "/stocks/snapshots":
		if b.snap == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"AAPL": b.snap})
	case r.URL.Path == "/clock":
		_ = json.NewEncoder(w).Encode(map[string]any{"is_open": b.clockOpen,
			"timestamp": "2026-10-26T05:00:00-04:00"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func tradingDayRow() map[string]any {
	// Measured shapes: session_open/session_close are HHMM, open/close are
	// HH:MM. A fixture that invents one format hides a real parse failure.
	return map[string]any{"date": "2026-10-26", "session_open": "0400",
		"open": "09:30", "close": "16:00", "session_close": "2000"}
}

func routeSnap(bid, ask float64, at time.Time) map[string]any {
	return map[string]any{"latestQuote": map[string]any{"bp": bid, "ap": ask,
		"t": at.UTC().Format(time.RFC3339Nano)}}
}

func atET(hhmm string) time.Time {
	tt, _ := time.ParseInLocation("2006-01-02 15:04", "2026-10-26 "+hhmm, routeET)
	return tt
}

// newRouteLoop builds a loop whose broker is a scripted test server, with the
// clock pinned so a session answer does not depend on when the test runs.
func newRouteLoop(t *testing.T, b *routeBroker, cfg *config.Config, clock string) (*autoLoop, *tools.Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.handler))
	t.Cleanup(srv.Close)
	client := tools.NewClient("k", "s", srv.URL, srv.URL)
	now, err := time.Parse("15:04", clock)
	if err != nil {
		t.Fatalf("clock: %v", err)
	}
	at := time.Date(2026, 10, 26, now.Hour(), now.Minute(), 0, 0, routeET)
	sessions := &tools.Sessions{Client: client, Now: func() time.Time { return at }, Location: routeET}
	extSpec := "04:30-09:25,16:05-19:55"
	if cfg != nil && cfg.ExtendedHoursWindows != "" {
		extSpec = cfg.ExtendedHoursWindows
	}
	extWin, err := strategy.NewTradingWindows(extSpec, extSpec, routeET, func() time.Time { return at })
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	l := &autoLoop{
		cfg: cfg, client: client, sessions: sessions, extWin: extWin,
		exec: &strategy.Executor{Client: client, Validator: approveGate{}},
	}
	return l, client
}

type approveGate struct{}

func (approveGate) Validate(risk.Proposal) risk.Verdict { return risk.Verdict{Approved: true} }

func routeCfg(extended, preOrders bool) *config.Config {
	c := &config.Config{}
	c.ExtendedHours = extended
	c.PreOrders = preOrders
	c.MaxSpreadBps = 25
	c.ExtendedHoursWindows = "04:30-09:25,16:05-19:55"
	c.ExtendedHoursMaxQuoteAgeSeconds = 300
	return c
}

func TestRouteEntryTable(t *testing.T) {
	freshQuote := routeSnap(347.00, 347.10, time.Now().UTC())
	wideQuote := routeSnap(340.00, 355.00, time.Now().UTC())
	staleQuote := routeSnap(347.00, 347.10, time.Now().UTC().Add(-time.Hour))
	futureQuote := routeSnap(347.00, 347.10, time.Now().UTC().Add(time.Hour))

	cases := []struct {
		name     string
		broker   *routeBroker
		cfg      *config.Config
		clock    string
		symbol   string
		wantKind entryKind
		wantSub  string
	}{
		{
			name:   "regular session is a live order",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: freshQuote},
			cfg:    routeCfg(true, false), clock: "10:00", symbol: "AAPL",
			wantKind: routeLive,
		},
		{
			name:   "pre-market with the session on is an extended entry",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: freshQuote},
			cfg:    routeCfg(true, false), clock: "05:00", symbol: "AAPL",
			wantKind: routeExtended,
		},
		{
			name:   "after-hours with the session on is an extended entry",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: freshQuote},
			cfg:    routeCfg(true, false), clock: "17:00", symbol: "AAPL",
			wantKind: routeExtended,
		},
		{
			name:   "session off falls back to a pre-order when enabled",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: freshQuote},
			cfg:    routeCfg(false, true), clock: "05:00", symbol: "AAPL",
			wantKind: routePreOrder,
		},
		{
			name:   "session off and no pre-orders refuses, naming the switch",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: freshQuote},
			cfg:    routeCfg(false, false), clock: "05:00", symbol: "AAPL",
			wantKind: routeBlocked, wantSub: "EXTENDED_HOURS=false",
		},
		{
			name:   "outside the window refuses, naming the spec",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: freshQuote},
			cfg:    routeCfg(true, false), clock: "04:15", symbol: "AAPL",
			wantKind: routeBlocked, wantSub: "EXTENDED_HOURS_WINDOWS",
		},
		{
			// The measured weekend print: a snapshot keeps serving its last
			// quote, so an age-blind price is a price from another day.
			name:   "stale quote refuses",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: staleQuote},
			cfg:    routeCfg(true, false), clock: "05:00", symbol: "AAPL",
			wantKind: routeBlocked, wantSub: "old",
		},
		{
			// A stamp ahead of this clock is not freshness. time.Since goes
			// negative and a bare age comparison reads the print as
			// infinitely new — which is how a broken stamp buys a trade.
			name:   "future-dated quote refuses",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: futureQuote},
			cfg:    routeCfg(true, false), clock: "05:00", symbol: "AAPL",
			wantKind: routeBlocked, wantSub: "in the future",
		},
		{
			name:   "wide extended spread refuses on the session cap",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: wideQuote},
			cfg:    routeCfg(true, false), clock: "05:00", symbol: "AAPL",
			wantKind: routeBlocked, wantSub: "extended-session cap",
		},
		{
			name:   "no quote at all refuses rather than prices on nothing",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: nil},
			cfg:    routeCfg(true, false), clock: "05:00", symbol: "AAPL",
			wantKind: routeBlocked, wantSub: "quote",
		},
		{
			// Alpaca refuses extended_hours on options (measured 422), so the
			// overlay can never ride this route.
			name:   "options are never extended",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: freshQuote},
			cfg:    routeCfg(true, false), clock: "05:00", symbol: "AAPL240119C00100000",
			wantKind: routeBlocked, wantSub: "options do not trade outside the regular session",
		},
		{
			name:   "crypto never consults the equity session",
			broker: &routeBroker{calendar: []map[string]any{}, snap: nil},
			cfg:    routeCfg(false, false), clock: "03:00", symbol: "BTC/USD",
			wantKind: routeLive,
		},
		{
			// A holiday is not a weekday-that-trades: the calendar omits it.
			name:   "holiday pre-market is closed, not extended",
			broker: &routeBroker{calendar: []map[string]any{}, snap: freshQuote},
			cfg:    routeCfg(true, false), clock: "05:00", symbol: "AAPL",
			wantKind: routeBlocked, wantSub: "market closed",
		},
		{
			name:   "night between sessions is closed",
			broker: &routeBroker{calendar: []map[string]any{tradingDayRow()}, snap: freshQuote},
			cfg:    routeCfg(true, false), clock: "22:00", symbol: "AAPL",
			wantKind: routeBlocked, wantSub: "market closed",
		},
		{
			// The route must not guess: an unreadable calendar blocks, and
			// says so, instead of defaulting to a session it cannot see.
			name:   "calendar outage blocks with the reason",
			broker: &routeBroker{calendar: nil, snap: freshQuote},
			cfg:    routeCfg(true, false), clock: "05:00", symbol: "AAPL",
			wantKind: routeBlocked, wantSub: "session unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newRouteLoop(t, tc.broker, tc.cfg, tc.clock)
			got := l.routeEntry(context.Background(), tc.symbol)
			if got.kind != tc.wantKind {
				t.Fatalf("route = %v (%s), want %v", got.kind, got.why, tc.wantKind)
			}
			if tc.wantSub != "" && !strings.Contains(got.why, tc.wantSub) {
				t.Fatalf("reason %q missing %q", got.why, tc.wantSub)
			}
			if tc.wantKind == routeExtended && got.quote.Valid() == false {
				t.Fatal("an extended route must carry the quote it will be priced from")
			}
		})
	}
}

// Without a session source the loop keeps the historical clock behaviour —
// the unit-test path and any loop built before this feature existed.
func TestRouteEntryWithoutSessionsFallsBackToClock(t *testing.T) {
	b := &routeBroker{clockOpen: false}
	srv := httptest.NewServer(http.HandlerFunc(b.handler))
	t.Cleanup(srv.Close)
	client := tools.NewClient("k", "s", srv.URL, srv.URL)
	l := &autoLoop{cfg: routeCfg(true, false), client: client,
		exec: &strategy.Executor{Client: client, Validator: approveGate{}}}
	if got := l.routeEntry(context.Background(), "AAPL"); got.kind != routeBlocked ||
		!strings.Contains(got.why, "market closed") {
		t.Fatalf("clock fallback = %v (%s)", got.kind, got.why)
	}
	b.clockOpen = true
	if got := l.routeEntry(context.Background(), "AAPL"); got.kind != routeLive {
		t.Fatalf("open clock fallback = %v, want live", got.kind)
	}
	l.cfg.PreOrders = true
	b.clockOpen = false
	if got := l.routeEntry(context.Background(), "AAPL"); got.kind != routePreOrder {
		t.Fatalf("closed clock fallback with PRE_ORDERS = %v, want pre-order", got.kind)
	}
}

// An unset window is not a licence to trade the whole session.
func TestInExtendedWindowFailsClosed(t *testing.T) {
	l := &autoLoop{}
	if l.inExtendedWindow("AAPL") {
		t.Fatal("no window configured reported a tradeable session")
	}
}

// The extended route must be the one that skips the options overlay: an option
// order placed in the extended session rests until the bell and fills at a
// price the signal never described.
func TestExtendedRouteCarriesTheSessionQuote(t *testing.T) {
	b := &routeBroker{calendar: []map[string]any{tradingDayRow()},
		snap: routeSnap(100.00, 100.05, time.Now().UTC())}
	l, _ := newRouteLoop(t, b, routeCfg(true, false), "05:00")
	got := l.routeEntry(context.Background(), "AAPL")
	if got.kind != routeExtended {
		t.Fatalf("route = %v (%s)", got.kind, got.why)
	}
	if got.quote.Bid != 100.00 || got.quote.Ask != 100.05 {
		t.Fatalf("quote = %+v", got.quote)
	}
	if got.quote.SpreadBps() < 4 || got.quote.SpreadBps() > 6 {
		t.Fatalf("spread = %.2fbps, want ~5bps for a 5c touch on $100", got.quote.SpreadBps())
	}
}
