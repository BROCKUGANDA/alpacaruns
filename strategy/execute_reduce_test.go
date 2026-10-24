package strategy

// execute_reduce_test.go pins the exit authority: what it reads, in which
// order it acts, and what it refuses.

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

	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// exitBroker is a scripted Alpaca paper endpoint that records the request
// sequence — the ORDER is the contract here, not just the outcome.
type exitBroker struct {
	mu        sync.Mutex
	positions []map[string]any
	resting   []map[string]any // open orders on the symbol (protection)
	events    []string         // "GET /positions", "DELETE id", "POST side qty"
	placeErr  int              // when non-zero, POST returns this status
	placeBody string           // body to return with placeErr
	posted    int              // number of POSTs received
	onPost    func()           // observed at the moment the POST arrives
}

func (b *exitBroker) handler(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/positions":
		b.events = append(b.events, "GET /positions")
		_ = json.NewEncoder(w).Encode(b.positions)
	case r.Method == http.MethodGet && r.URL.Path == "/orders":
		b.events = append(b.events, "GET /orders?"+r.URL.RawQuery)
		if r.URL.Query().Get("status") == "open" {
			// The protective pair resting on this symbol, which the exit
			// must clear before selling into it.
			_ = json.NewEncoder(w).Encode(b.resting)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/orders/"):
		b.events = append(b.events, "DELETE "+strings.TrimPrefix(r.URL.Path, "/orders/"))
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPost && r.URL.Path == "/orders":
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		b.events = append(b.events, "POST "+stringOr(body["side"])+" "+stringOr(body["qty"]))
		b.posted++
		if b.onPost != nil {
			b.onPost()
		}
		if b.placeErr != 0 {
			w.WriteHeader(b.placeErr)
			msg := b.placeBody
			if msg == "" {
				msg = `{"code":42210000,"message":"client_order_id must be unique"}`
			}
			_, _ = w.Write([]byte(msg))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":              "ord-" + stringOr(body["client_order_id"]),
			"client_order_id": body["client_order_id"],
			"symbol":          body["symbol"], "side": body["side"], "qty": body["qty"],
			"status": "accepted", "filled_qty": "0", "filled_avg_price": nil,
		})
	default:
		b.events = append(b.events, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func stringOr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (b *exitBroker) log() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.events...)
}

type recGate struct {
	got  []risk.Proposal
	want []string // refusal reasons; empty = approve
}

func (g *recGate) Validate(p risk.Proposal) risk.Verdict {
	g.got = append(g.got, p)
	if len(g.want) > 0 {
		return risk.Verdict{Approved: false, Reasons: g.want}
	}
	return risk.Verdict{Approved: true}
}

func newExitSetup(t *testing.T, positions []map[string]any) (*Executor, *exitBroker, *recGate, *pnl.Journal, *StateStore) {
	t.Helper()
	dir := t.TempDir()
	jpath := filepath.Join(dir, "trades.jsonl")
	j, err := pnl.Open(jpath)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	broker := &exitBroker{positions: positions}
	srv := httptest.NewServer(http.HandlerFunc(broker.handler))
	t.Cleanup(srv.Close)
	st := NewStateStore(jpath)
	gate := &recGate{}
	x := &Executor{
		Client:    tools.NewClient("k", "s", srv.URL, srv.URL),
		Validator: gate,
		State:     st,
		Journal:   j,
	}
	return x, broker, gate, j, st
}

func pos(sym, qty, px string) map[string]any {
	return map[string]any{"symbol": sym, "qty": qty, "current_price": px, "avg_entry_price": px}
}

// A long is closed by selling, and the quantity is the WHOLE live position
// read fresh from the broker.
func TestExecuteReduceClosesLongFromLiveRead(t *testing.T) {
	x, broker, gate, j, st := newExitSetup(t, []map[string]any{pos("AAPL", "5", "320")})
	// A stale level claiming a different size must not influence the order.
	if err := st.SetLevel(PositionLevels{Symbol: "AAPL", Qty: "999", StopLoss: 300}); err != nil {
		t.Fatalf("level: %v", err)
	}
	done, err := x.ExecuteReduce(context.Background(), "AAPL", "max-hold")
	if err != nil {
		t.Fatalf("ExecuteReduce: %v", err)
	}
	if !done {
		t.Fatal("reported nothing to do while a position existed")
	}
	if len(gate.got) != 1 {
		t.Fatalf("gate called %d times, want exactly 1", len(gate.got))
	}
	p := gate.got[0]
	if !p.Reducing {
		t.Fatal("the exit was not asserted reducing — it would face entry policy")
	}
	if p.Side != "sell" || p.Qty != "5" {
		t.Fatalf("wrong order built: %+v", p)
	}
	if p.Confidence != nil {
		t.Fatal("an exit was given a confidence number; exits are not bets")
	}
	ev := broker.log()
	if !contains(ev, "POST sell 5") {
		t.Fatalf("no exit POST: %v", ev)
	}
	// Level cleared, so the monitor stops chasing a closed position.
	if got := st.Level("AAPL", ""); got != nil {
		t.Fatalf("stale level survived the close: %+v", got)
	}
	// An intent row exists for the submission.
	recs, _ := j.Records()
	if countKind(recs, pnl.KindIntent) != 1 {
		t.Fatalf("no durable intent written: %+v", recs)
	}
}

// A short is closed by BUYING. The path this replaced returned early on a
// negative quantity, so a short could never be exited on a signal.
func TestExecuteReduceClosesShort(t *testing.T) {
	x, broker, gate, _, _ := newExitSetup(t, []map[string]any{pos("TSLA", "-2", "400")})
	done, err := x.ExecuteReduce(context.Background(), "TSLA", "signal")
	if err != nil || !done {
		t.Fatalf("short close failed: done=%v err=%v", done, err)
	}
	if gate.got[0].Side != "buy" || gate.got[0].Qty != "2" {
		t.Fatalf("short was not covered: %+v", gate.got[0])
	}
	if !contains(broker.log(), "POST buy 2") {
		t.Fatalf("events: %v", broker.log())
	}
}

// Crypto position symbols come back as BTCUSD while levels and the engine
// use BTC/USD; the exit must trade the broker's own form.
func TestExecuteReduceUsesBrokerSymbolForm(t *testing.T) {
	x, broker, gate, _, _ := newExitSetup(t, []map[string]any{pos("BTCUSD", "0.5", "60000")})
	if _, err := x.ExecuteReduce(context.Background(), "BTC/USD", "tp"); err != nil {
		t.Fatalf("crypto close: %v", err)
	}
	if !strings.EqualFold(gate.got[0].Symbol, "BTCUSD") {
		t.Fatalf("gate saw the strategy form, not the broker's: %q", gate.got[0].Symbol)
	}
	if !contains(broker.log(), "POST sell 0.5") {
		t.Fatalf("events: %v", broker.log())
	}
}

// Protection is cancelled BEFORE the market exit. Cancel after, or not at
// all, and the surviving leg fires against a flat account — a short the bot
// never intended.
func TestExecuteReduceClearsProtectionFirst(t *testing.T) {
	x, broker, _, _, _ := newExitSetup(t, []map[string]any{pos("NVDA", "3", "900")})
	// A resting OCO pair on the same symbol: exactly what must be cleared
	// before a market exit, or it fires against a flat account afterwards.
	broker.mu.Lock()
	broker.resting = []map[string]any{{
		"id": "protect-1", "symbol": "NVDA", "side": "sell", "type": "limit",
		"order_class": "oco", "status": "accepted", "qty": "3", "filled_qty": "0",
	}}
	broker.mu.Unlock()
	if _, err := x.ExecuteReduce(context.Background(), "NVDA", "exit"); err != nil {
		t.Fatalf("ExecuteReduce: %v", err)
	}
	ev := broker.log()
	postAt, cancelAt := indexOfPrefix(ev, "POST "), -1
	for i, e := range ev {
		if strings.HasPrefix(e, "DELETE ") {
			cancelAt = i
			break
		}
	}
	if cancelAt < 0 {
		t.Fatalf("no protective order was cancelled before the exit: %v", ev)
	}
	if postAt < 0 || cancelAt > postAt {
		t.Fatalf("exit placed before protection was cleared (cancel=%d post=%d): %v", cancelAt, postAt, ev)
	}
}

// A refused exit places nothing. The gate's word is final even though the
// caller asserted reducing — the assertion is checked, not trusted.
func TestExecuteReduceHonoursGateRefusal(t *testing.T) {
	x, broker, _, j, _ := newExitSetup(t, []map[string]any{pos("AMD", "10", "200")})
	broker.mu.Lock()
	// Rebuild the executor's gate with a refusal.
	broker.mu.Unlock()
	refusing := &recGate{want: []string{"reducing order oversells"}}
	x.Validator = refusing
	_, err := x.ExecuteReduce(context.Background(), "AMD", "exit")
	if err == nil {
		t.Fatal("a refused exit returned no error")
	}
	if !strings.Contains(err.Error(), "oversells") {
		t.Fatalf("refusal reason lost: %v", err)
	}
	for _, e := range broker.log() {
		if strings.HasPrefix(e, "POST ") {
			t.Fatalf("an order was placed after the gate refused: %v", broker.log())
		}
	}
	// No intent may be written for an order that was never authorised.
	recs, _ := j.Records()
	if countKind(recs, pnl.KindIntent) != 0 {
		t.Fatal("intent written for a rejected exit")
	}
}

// Flat account: no order, and the stale level is dropped rather than left
// to be re-chased every tick.
func TestExecuteReduceOnFlatPositionClearsLevel(t *testing.T) {
	x, broker, gate, _, st := newExitSetup(t, nil)
	if err := st.SetLevel(PositionLevels{Symbol: "MSFT", Qty: "4"}); err != nil {
		t.Fatalf("level: %v", err)
	}
	done, err := x.ExecuteReduce(context.Background(), "MSFT", "exit")
	if err != nil {
		t.Fatalf("flat close should not error: %v", err)
	}
	if done {
		t.Fatal("reported a close with nothing held")
	}
	if len(gate.got) != 0 {
		t.Fatal("the gate was consulted for a position that does not exist")
	}
	for _, e := range broker.log() {
		if strings.HasPrefix(e, "POST ") {
			t.Fatalf("ordered a sell against a flat account: %v", broker.log())
		}
	}
	if got := st.Level("MSFT", ""); got != nil {
		t.Fatal("stale level survived")
	}
}

// The exit id is deterministic on (symbol, side, qty, UTC day), which is
// what lets the broker refuse a retry instead of accepting a second sell.
func TestExecuteReduceExitIntentIDDedupes(t *testing.T) {
	day := time.Now().UTC().Format("20060102")
	a := exitIntentID("AAPL", "sell", "5", day)
	b := exitIntentID("AAPL", "sell", "5", day)
	if a != b {
		t.Fatalf("the same close produced two ids: %q vs %q", a, b)
	}
	if c := exitIntentID("AAPL", "sell", "6", day); c == a {
		t.Fatal("a different quantity reused the same id")
	}
	if d := exitIntentID("AAPL", "buy", "5", day); d == a {
		t.Fatal("the opposite side reused the same id")
	}
	if e := exitIntentID("AAPL", "sell", "5", "20000101"); e == a {
		t.Fatal("a different day reused the same id")
	}
	// Broker-safe character set only.
	if f := exitIntentID("BTC/USD", "sell", "0.5", day); strings.ContainsAny(f, "/ ") {
		t.Fatalf("id carries characters the broker rejects: %q", f)
	}
}

// A duplicate-id rejection means the order is ALREADY at the broker. Treating
// it as a failure invites a retry under a fresh id, which is how the second
// sell and the unintended short happen.
func TestExecuteReduceTreatsDuplicateAsSubmitted(t *testing.T) {
	x, broker, _, j, _ := newExitSetup(t, []map[string]any{pos("SPY", "2", "500")})
	broker.mu.Lock()
	broker.placeErr = http.StatusUnprocessableEntity
	broker.mu.Unlock()

	done, err := x.ExecuteReduce(context.Background(), "SPY", "exit")
	if err != nil {
		t.Fatalf("a duplicate id must not read as a failure: %v", err)
	}
	if !done {
		t.Fatal("a duplicate id must read as already submitted")
	}
	recs, _ := j.Records()
	var noted bool
	for _, r := range recs {
		if strings.Contains(r.Detail, "already at the broker") || strings.Contains(r.Detail, "already submitted") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the idempotent outcome was not journalled: %+v", recs)
	}
}

func countKind(recs []pnl.Record, kind string) int {
	n := 0
	for _, r := range recs {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func indexOfPrefix(xs []string, prefix string) int {
	for i, x := range xs {
		if strings.HasPrefix(x, prefix) {
			return i
		}
	}
	return -1
}

// The intent must reach the journal BEFORE the order is submitted, and the
// only way to prove that ordering is to look at the journal AT THE MOMENT
// the broker is called: if the row exists there, it was written before the
// request left. Asserting only that the row exists afterwards is satisfied
// by writing it after a failed POST, which is exactly the ordering that
// loses a trade when the response never arrives.
func TestExecuteReduceWritesIntentBeforeSubmitting(t *testing.T) {
	x, broker, _, j, _ := newExitSetup(t, []map[string]any{pos("SPY", "3", "500")})
	broker.mu.Lock()
	broker.placeErr = http.StatusInternalServerError
	broker.placeBody = `{"message":"boom"}`
	broker.mu.Unlock()

	intentAtCall := -1
	broker.onPost = func() {
		recs, err := j.Records()
		if err != nil {
			t.Errorf("journal unreadable at POST time: %v", err)
			return
		}
		intentAtCall = countKind(recs, pnl.KindIntent)
	}

	done, err := x.ExecuteReduce(context.Background(), "SPY", "exit")
	if err == nil || done {
		t.Fatalf("a 500 must be an error, got done=%v err=%v", done, err)
	}
	if broker.posted == 0 {
		t.Fatal("the POST never happened, so the ordering assertion proved nothing")
	}
	if intentAtCall != 1 {
		t.Fatalf("at the moment the order was submitted the journal held %d intent rows; a lost response would leave this trade unexplainable", intentAtCall)
	}
}
