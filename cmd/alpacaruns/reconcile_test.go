package main

// reconcile_test.go proves the fill-recovery pass against the broker's
// actual wire shape — the field names and statuses here are the ones
// measured against the live paper account, not invented from this code.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// brokerStub answers the two calls the reconciliation pass makes and
// records what it was asked for.
type brokerStub struct {
	mu       sync.Mutex
	orders   []map[string]any
	lists    int
	notFound bool
}

func (b *brokerStub) handler(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.notFound {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 40010000, "message": "nope"})
		return
	}
	// Client.BaseURL already carries /v2 (it is the ALPACA_BASE_URL value,
	// e.g. https://paper-api.alpaca.markets/v2), so paths arrive without it.
	switch {
	case r.URL.Path == "/orders" && r.Method == http.MethodGet:
		b.lists++
		if got := r.URL.Query().Get("status"); got != "all" {
			// Bracket and OCO children are only listed under status=all;
			// asking for anything else means the pass would silently miss
			// the stop leg that actually protects the position.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"reconciliation must list status=all"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(b.orders)
	case r.URL.Path == "/account":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "PA-PAPER", "equity": "100000", "cash": "50000",
			"buying_power": "100000", "portfolio_value": "100000", "status": "ACTIVE",
		})
	case r.URL.Path == "/positions":
		_ = json.NewEncoder(w).Encode([]any{})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"unexpected path ` + r.URL.Path + `"}`))
	}
}

func (b *brokerStub) client(t *testing.T) *tools.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.handler))
	t.Cleanup(srv.Close)
	return tools.NewClient("k", "s", srv.URL, srv.URL)
}

func fillOrder(id, coid, sym, side, qty, avg, status string, submitted time.Time) map[string]any {
	return map[string]any{
		"id": id, "client_order_id": coid, "symbol": sym, "side": side,
		"qty": qty, "filled_qty": strings.Split(qty, ".")[0], "filled_avg_price": avg,
		"status": status, "type": "market", "order_class": "", "group_role": nil,
		"position_intent": "buy_to_open",
		"submitted_at":    submitted.UTC().Format(time.RFC3339Nano),
		"created_at":      submitted.UTC().Format(time.RFC3339Nano),
	}
}

func newReconcileLoop(t *testing.T, orders []map[string]any) (*autoLoop, *pnl.Journal, *brokerStub) {
	t.Helper()
	dir := t.TempDir()
	j, err := pnl.Open(filepath.Join(dir, "trades.jsonl"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	stub := &brokerStub{orders: orders}
	l := &autoLoop{
		cfg:     &config.Config{FactorMinScore: 0.5},
		journal: j,
		client:  stub.client(t),
	}
	return l, j, stub
}

// An order the broker filled and the journal never recorded must be
// recovered WITH the score from its intent, or the learner sees a trade
// with no decision behind it.
func TestReconcileRecoversLostFill(t *testing.T) {
	sub := time.Now().UTC().Add(-time.Hour)
	l, j, _ := newReconcileLoop(t, []map[string]any{
		fillOrder("o-1", "alp-entry-AAPL-1", "AAPL", "buy", "5", "319.69", "filled", sub),
	})
	// The intent that produced it (written before submission).
	sc := 0.72
	if err := j.Append(pnl.Record{
		Kind: pnl.KindIntent, Source: "strategy:auto", ClientOrderID: "alp-entry-AAPL-1",
		Symbol: "AAPL", Side: "buy", Qty: "5", Confidence: &sc,
	}); err != nil {
		t.Fatalf("intent: %v", err)
	}

	n, err := l.reconcileFills(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered %d, want 1", n)
	}
	recs, _ := j.Records()
	var fill *pnl.Record
	for i := range recs {
		if recs[i].Kind == pnl.KindFill {
			fill = &recs[i]
		}
	}
	if fill == nil {
		t.Fatal("no fill row written")
	}
	if fill.OrderID != "o-1" || fill.Symbol != "AAPL" || fill.Side != "buy" ||
		fill.Qty != "5" || fill.Price != "319.69" {
		t.Fatalf("recovered fill does not match the broker: %+v", fill)
	}
	if fill.ClientOrderID != "alp-entry-AAPL-1" {
		t.Fatalf("recovered fill lost the link to its intent: %+v", fill)
	}
	if fill.Confidence == nil || *fill.Confidence != 0.72 {
		t.Fatalf("recovered fill lost the entry score: %+v", fill.Confidence)
	}
	// And it is visible to the FIFO machinery the halts and ledger use.
	fills, err := j.Fills(time.Time{})
	if err != nil {
		t.Fatalf("fills: %v", err)
	}
	if len(fills) != 1 {
		t.Fatalf("journal should now carry the fill for P/L math, got %d", len(fills))
	}
}

// Re-running must add nothing. A pass that appends every tick would turn
// one fill into hundreds of phantom trades and trip the drawdown halts.
func TestReconcileIsIdempotent(t *testing.T) {
	sub := time.Now().UTC().Add(-2 * time.Hour)
	l, j, _ := newReconcileLoop(t, []map[string]any{
		fillOrder("o-2", "alp-entry-TSLA-1", "TSLA", "buy", "1", "436.31", "filled", sub),
	})
	want := []int{1, 0, 0, 0}
	for i := 0; i < 4; i++ {
		n, err := l.reconcileFills(context.Background())
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if n != want[i] {
			t.Fatalf("pass %d wrote %d rows, want %d", i, n, want[i])
		}
	}
	fills, _ := j.Fills(time.Time{})
	if len(fills) != 1 {
		t.Fatalf("fill duplicated across passes: %d rows", len(fills))
	}
}

// Partial fills must accumulate rather than be judged "already seen".
func TestReconcileHandlesPartialThenRest(t *testing.T) {
	sub := time.Now().UTC().Add(-time.Hour)
	partial := fillOrder("o-3", "alp-entry-NVDA-1", "NVDA", "buy", "10", "900.00", "partially_filled", sub)
	partial["filled_qty"] = "3"
	l, j, stub := newReconcileLoop(t, []map[string]any{partial})

	n, err := l.reconcileFills(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("first pass n=%d err=%v", n, err)
	}
	// The rest arrives.
	full := fillOrder("o-3", "alp-entry-NVDA-1", "NVDA", "buy", "10", "900.00", "filled", sub)
	full["filled_qty"] = "10"
	stub.mu.Lock()
	stub.orders = []map[string]any{full}
	stub.mu.Unlock()

	n, err = l.reconcileFills(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n != 1 {
		t.Fatalf("second pass wrote %d, want the remaining 7 units as one row", n)
	}
	fills, _ := j.Fills(time.Time{})
	if len(fills) != 2 {
		t.Fatalf("want 2 partial rows, got %d", len(fills))
	}
	if fills[0].Qty != "3" || fills[1].Qty != "7" {
		t.Fatalf("deltas wrong: %s then %s", fills[0].Qty, fills[1].Qty)
	}
	// Third pass: settled, nothing owed.
	if n, _ := l.reconcileFills(context.Background()); n != 0 {
		t.Fatalf("a fully accounted order was journalled again: %d rows", n)
	}
}

// Outside the lookback: not this pass's job, and re-reading old orders
// every tick would grow the tick's cost forever.
func TestReconcileRespectsLookback(t *testing.T) {
	old := fillOrder("o-4", "alp-entry-OLD-1", "AMD", "buy", "4", "200.0", "filled",
		time.Now().UTC().Add(-(fillLookback + 24*time.Hour)))
	l, j, _ := newReconcileLoop(t, []map[string]any{old})
	// Steady state: the first, wide bootstrap window has already been spent.
	l.fillBackfilled = true
	n, err := l.reconcileFills(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 0 {
		t.Fatalf("recovered a fill outside the lookback: %d", n)
	}
	if f, _ := j.Fills(time.Time{}); len(f) != 0 {
		t.Fatal("out-of-window fill written")
	}
}

// A broker order that reports a fill but no average price is a contradiction
// the pass must NOT resolve by inventing one: a made-up price silently
// rewrites realized P/L.
func TestReconcileRefusesPricelessFill(t *testing.T) {
	odd := fillOrder("o-5", "alp-entry-X-1", "XOM", "buy", "3", "", "filled", time.Now().UTC().Add(-time.Hour))
	odd["filled_avg_price"] = nil
	l, j, _ := newReconcileLoop(t, []map[string]any{odd})
	n, err := l.reconcileFills(context.Background())
	if err != nil {
		t.Fatalf("the pass must survive one bad row: %v", err)
	}
	if n != 0 {
		t.Fatalf("invented a price for a fill without one: %d", n)
	}
	if f, _ := j.Fills(time.Time{}); len(f) != 0 {
		t.Fatal("a price-less fill reached the ledger")
	}
}

// Unfilled and cancelled orders are not trades.
func TestReconcileIgnoresNothingFilled(t *testing.T) {
	accepted := fillOrder("o-6", "alp-entry-Y-1", "MSFT", "buy", "2", "", "accepted", time.Now().UTC().Add(-time.Minute))
	accepted["filled_qty"] = "0"
	cancelled := fillOrder("o-7", "alp-entry-Z-1", "SPY", "sell", "1", "", "canceled", time.Now().UTC().Add(-time.Minute))
	cancelled["filled_qty"] = "0"
	l, j, _ := newReconcileLoop(t, []map[string]any{accepted, cancelled})
	n, err := l.reconcileFills(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if f, _ := j.Fills(time.Time{}); len(f) != 0 {
		t.Fatalf("non-fills journalled: %+v", f)
	}
}

// A broker outage is an error, not an empty result — the caller must be
// able to tell "nothing to recover" from "could not look".
func TestReconcileReportsLookupFailure(t *testing.T) {
	dir := t.TempDir()
	j, err := pnl.Open(filepath.Join(dir, "trades.jsonl"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	defer j.Close()
	stub := &brokerStub{notFound: true}
	l := &autoLoop{cfg: &config.Config{}, journal: j, client: stub.client(t)}
	if _, err := l.reconcileFills(context.Background()); err == nil {
		t.Fatal("a failed order lookup reported success")
	}
}

// An unwired client must not panic the tick.
func TestReconcileWithoutClient(t *testing.T) {
	l := &autoLoop{}
	n, err := l.reconcileFills(context.Background())
	if n != 0 || err != nil {
		t.Fatalf("no-client pass should no-op, got n=%d err=%v", n, err)
	}
}

// The recovery must happen inside the tick, and BEFORE the drawdown check:
// the halts read realized P/L out of the journal, so a fill recovered after
// the halt computation would be a halt that fired (or failed to fire) on
// numbers the broker had already corrected.
//
// safeTick is used deliberately: the loop is only wired far enough to reach
// the reconcile step, so the tick is expected to fail afterwards on the
// missing engine. What is being proven is the ORDER, and the rows the pass
// wrote.
func TestTickRunsReconciliationBeforeTheTradingPass(t *testing.T) {
	sub := time.Now().UTC().Add(-time.Hour)
	l, j, stub := newReconcileLoop(t, []map[string]any{
		fillOrder("o-8", "alp-entry-Q-1", "COIN", "buy", "2", "250.5", "filled", sub),
	})
	l.cfg = &config.Config{PollSeconds: 1, FactorMinScore: 0.5}
	_ = l.safeTick(context.Background())

	if stub.lists == 0 {
		t.Fatal("the tick never asked the broker for its order list")
	}
	fills, _ := j.Fills(time.Time{})
	if len(fills) != 1 {
		t.Fatalf("the tick did not recover the lost fill before trading: %d rows", len(fills))
	}
}

// The gap the pass exists to close is usually historical: the fills that
// were missing on the live account had been accepted weeks before the fix
// shipped, so a 72-hour window recovered nothing on its own.
func TestBootstrapPassRecoversOlderFills(t *testing.T) {
	older := fillOrder("o-9", "alp-entry-OLD-2", "AMD", "buy", "4", "200.0", "filled",
		time.Now().UTC().Add(-10*24*time.Hour))
	l, j, _ := newReconcileLoop(t, []map[string]any{older})
	if n, err := l.reconcileFills(context.Background()); err != nil || n != 1 {
		t.Fatalf("bootstrap pass recovered n=%d err=%v", n, err)
	}
	if !l.fillBackfilled {
		t.Fatal("a successful pass did not spend the bootstrap window")
	}
	if n, _ := l.reconcileFills(context.Background()); n != 0 {
		t.Fatalf("the wide window was reused: %d", n)
	}
	if f, _ := j.Fills(time.Time{}); len(f) != 1 {
		t.Fatalf("want one recovered fill, got %d", len(f))
	}
}
