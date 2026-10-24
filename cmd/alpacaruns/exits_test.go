package main

// exits_test.go proves the two policy inversions in the exit path: an exit
// is no longer gated by the ENTRY window, and a halt closes new risk
// without closing the ability to reduce it.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// exitStub is a scripted paper broker that records what it was asked to do,
// in order.
type exitStub struct {
	mu        sync.Mutex
	positions []map[string]any
	posted    []map[string]any
	deletes   []string
}

func (s *exitStub) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/positions":
		_ = json.NewEncoder(w).Encode(s.positions)
	case r.Method == http.MethodGet && r.URL.Path == "/orders":
		list := []map[string]any{}
		if r.URL.Query().Get("status") == "open" {
			// A resting protective leg on the same symbol: the exit must
			// clear it before selling into it.
			list = append(list, map[string]any{
				"id": "leg-1", "symbol": "AAPL", "side": "sell", "type": "stop",
				"order_class": "oco", "status": "accepted", "qty": "5", "filled_qty": "0",
			})
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodDelete:
		s.deletes = append(s.deletes, strings.TrimPrefix(r.URL.Path, "/orders/"))
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPost && r.URL.Path == "/orders":
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.posted = append(s.posted, body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "exit-1", "client_order_id": body["client_order_id"],
			"symbol": body["symbol"], "side": body["side"], "qty": body["qty"],
			"status": "accepted", "filled_qty": "0",
		})
	case r.URL.Path == "/account":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "PA-TEST", "equity": "100000", "cash": "100000",
			"buying_power": "100000", "portfolio_value": "100000", "status": "ACTIVE",
		})
	case r.URL.Path == "/clock":
		// The market is OPEN in these fixtures. Otherwise a blocked BUY
		// would be indistinguishable from a BUY skipped because the
		// session was closed, and the halt test would prove nothing.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"timestamp": time.Now().UTC().Format(time.RFC3339), "is_open": true,
		})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *exitStub) snapshotPositions() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.positions...)
}

func (s *exitStub) postedBodies() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.posted...)
}

func (s *exitStub) deletedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deletes...)
}

func field(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func exitLoop(t *testing.T, positions []map[string]any) (*autoLoop, *exitStub, *pnl.Journal) {
	t.Helper()
	dir := t.TempDir()
	jpath := filepath.Join(dir, "trades.jsonl")
	j, err := pnl.Open(jpath)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })

	stub := &exitStub{positions: positions}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(srv.Close)
	client := tools.NewClient("k", "s", srv.URL, srv.URL)

	kill := agents.NewKillSwitch()
	val := &risk.Validator{
		Cfg: &config.Config{
			MinConfidence: 0.5, MaxPositionUSD: 100000,
			MaxPortfolioPct: 0.5, CryptoMaxPositionUSD: 100000,
		},
		Kill: kill,
		Portfolio: func() (risk.Portfolio, error) {
			return risk.Portfolio{Equity: 100000, PortfolioValue: 100000}, nil
		},
		Positions: func(string) float64 { return 0 },
		// Holdings reads the same scripted book the exit reads, so the
		// gate's reducing proof is checking the live numbers.
		Holdings: func(sym string) (float64, error) {
			for _, p := range stub.snapshotPositions() {
				if tools.SameSymbol(field(p["symbol"]), sym) {
					return strconv.ParseFloat(field(p["qty"]), 64)
				}
			}
			return 0, nil
		},
	}
	l := &autoLoop{
		cfg:     &config.Config{FactorMinScore: 0.5, MaxPositionUSD: 100000},
		journal: j,
		client:  client,
		book:    newLiveBook(client),
	}
	l.exec = &strategy.Executor{
		Client: client, Validator: val, Kill: kill,
		State: strategy.NewStateStore(jpath), Journal: j,
	}
	return l, stub, j
}

// closedWindows refuses every symbol at every hour, standing in for
// "outside the configured entry window".
func closedWindows(t *testing.T) *strategy.TradingWindows {
	t.Helper()
	w, err := strategy.NewTradingWindows("00:00-00:01", "00:00-00:01", nil, nil)
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	return w
}

// The signal says get out; the entry window says it is not a time to open
// positions. Those used to be the same gate, so an exit could be postponed
// for hours by a schedule that only governs taking risk.
func TestHandleSellIgnoresTheEntryWindow(t *testing.T) {
	l, stub, _ := exitLoop(t, []map[string]any{
		{"symbol": "AAPL", "qty": "5", "current_price": "320", "avg_entry_price": "319.69"},
	})
	l.windows = closedWindows(t)
	if l.windows.CanTrade("AAPL") {
		t.Skip("the fixture window unexpectedly allows trading at this hour")
	}
	l.handleSell(context.Background(), strategy.Decision{Symbol: "AAPL", Reason: "signal exit"})

	post := stub.postedBodies()
	if len(post) != 1 {
		t.Fatalf("the exit was blocked by the ENTRY window: %d orders placed", len(post))
	}
	if field(post[0]["side"]) != "sell" || field(post[0]["qty"]) != "5" {
		t.Fatalf("wrong exit order built: %+v", post[0])
	}
	if len(stub.deletedIDs()) == 0 {
		t.Fatal("the resting protective leg survived the exit")
	}
	// And the exit went out with a broker-deduplicable id.
	if field(post[0]["client_order_id"]) == "" {
		t.Fatal("the exit carried no client_order_id, so a retry could double the sell")
	}
}

// A halt must stop new exposure and must NOT stop the flatten. Before this,
// the reducing order met the kill switch and was refused — by the very
// control that was supposed to be protecting the account.
func TestHandleBuyBlockedByHaltButExitAllowed(t *testing.T) {
	l, stub, _ := exitLoop(t, []map[string]any{
		{"symbol": "AAPL", "qty": "5", "current_price": "320", "avg_entry_price": "319.69"},
	})
	l.exec.Kill.Halt()

	l.handleBuy(context.Background(), strategy.Decision{
		Symbol: "AAPL", Signal: strategy.SignalBuy, Composite: 0.99,
	}, 100000, 0.99)
	if n := len(stub.postedBodies()); n != 0 {
		t.Fatalf("a BUY reached the broker while the kill switch was engaged: %d orders", n)
	}

	l.handleSell(context.Background(), strategy.Decision{Symbol: "AAPL", Reason: "halt flatten"})
	post := stub.postedBodies()
	if len(post) != 1 {
		t.Fatalf("the flatten was blocked while halted: %d orders placed", len(post))
	}
	if field(post[0]["side"]) != "sell" {
		t.Fatalf("halt path placed the wrong order: %+v", post[0])
	}
}

// The path this replaced sized itself from the tick-frozen position cache,
// so a close could sell a quantity the same tick had already reduced. Each
// call must re-read the broker.
func TestExecuteReduceReReadsPositionsPerCall(t *testing.T) {
	l, stub, _ := exitLoop(t, []map[string]any{
		{"symbol": "AAPL", "qty": "5", "current_price": "320", "avg_entry_price": "319.69"},
	})
	if _, err := l.exec.ExecuteReduce(context.Background(), "AAPL", "first"); err != nil {
		t.Fatalf("first reduce: %v", err)
	}
	stub.mu.Lock()
	stub.positions = []map[string]any{
		{"symbol": "AAPL", "qty": "2", "current_price": "320", "avg_entry_price": "319.69"},
	}
	stub.mu.Unlock()
	if _, err := l.exec.ExecuteReduce(context.Background(), "AAPL", "second"); err != nil {
		t.Fatalf("second reduce: %v", err)
	}
	post := stub.postedBodies()
	if len(post) != 2 {
		t.Fatalf("want two closes, got %d", len(post))
	}
	if field(post[0]["qty"]) != "5" || field(post[1]["qty"]) != "2" {
		t.Fatalf("a stale quantity was reused: %s then %s",
			field(post[0]["qty"]), field(post[1]["qty"]))
	}
	if field(post[0]["client_order_id"]) == field(post[1]["client_order_id"]) {
		t.Fatal("two different closes shared one client_order_id")
	}
}

// The live book backs the gate's exposure maths. A read failure must not be
// reported as a flat account — that is how a cap becomes a guess.
func TestLiveBookTreatsUnavailableAsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	b := newLiveBook(tools.NewClient("k", "s", srv.URL, srv.URL))
	if _, err := b.signedQty(context.Background(), "AAPL"); err == nil {
		t.Fatal("an unreadable book reported a quantity")
	}
	if _, err := b.exposureUSD(context.Background(), "AAPL"); err == nil {
		t.Fatal("an unreadable book reported zero exposure")
	}
	// A nil book is inert, not fatal: several paths run without one.
	var nilBook *liveBook
	nilBook.Invalidate()
	if _, err := nilBook.signedQty(context.Background(), "AAPL"); err == nil {
		t.Fatal("nil book answered a holdings question")
	}
}

// Invalidating the book must actually force a fresh read; otherwise the
// same-tick double-buy this cache fixes stays possible.
func TestLiveBookInvalidationForcesRefetch(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"symbol": "AAPL", "qty": strconv.Itoa(calls), "current_price": "100"},
		})
	}))
	t.Cleanup(srv.Close)
	b := newLiveBook(tools.NewClient("k", "s", srv.URL, srv.URL))
	b.ttl = time.Hour // long TTL: only Invalidate() may cause a refetch

	q1, err := b.signedQty(context.Background(), "AAPL")
	if err != nil || q1 != 1 {
		t.Fatalf("first read: q=%v err=%v", q1, err)
	}
	q2, _ := b.signedQty(context.Background(), "AAPL")
	if q2 != 1 {
		t.Fatalf("the cached view was bypassed without an invalidation: %v", q2)
	}
	b.Invalidate()
	q3, err := b.signedQty(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("after invalidate: %v", err)
	}
	if q3 != 2 {
		t.Fatalf("invalidation did not force a fresh broker read: got %v", q3)
	}
}
