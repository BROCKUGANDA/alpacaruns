package stream

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeConn is a scripted conn: it replays readScript frames in order and
// records every written frame.
type fakeConn struct {
	mu         sync.Mutex
	readScript [][]byte
	writeLog   [][]byte
	closed     bool
	// deadlineSet counts SetReadDeadline calls (watchCtx exercises this).
	deadlineSet int
}

func (f *fakeConn) ReadMessage() (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.readScript) == 0 {
		return 0, nil, errors.New("script exhausted")
	}
	p := f.readScript[0]
	f.readScript = f.readScript[1:]
	return websocket.TextMessage, p, nil
}

func (f *fakeConn) WriteMessage(_ int, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeLog = append(f.writeLog, append([]byte(nil), data...))
	return nil
}

func (f *fakeConn) SetReadDeadline(time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadlineSet++
	return nil
}

func (f *fakeConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeConn) writes() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.writeLog))
	copy(out, f.writeLog)
	return out
}

// ---- parsing tests ----

func TestParseMarketEnvelopes(t *testing.T) {
	ts := "2026-08-25T14:30:00.123456789Z"
	wantTS, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("bad test timestamp: %v", err)
	}
	tests := []struct {
		name    string
		payload string
		wantNil bool // control/unknown frame => no event
		check   func(t *testing.T, ev MarketEvent)
	}{
		{
			name:    "trade",
			payload: `[{"T":"t","S":"AAPL","p":227.5,"s":100,"t":"` + ts + `"}]`,
			check: func(t *testing.T, ev MarketEvent) {
				if ev.Trade == nil {
					t.Fatalf("want trade event")
				}
				if ev.Trade.Symbol != "AAPL" || ev.Trade.Price != 227.5 || ev.Trade.Size != 100 {
					t.Fatalf("trade = %+v", ev.Trade)
				}
				if got := time.Unix(0, ev.Trade.TS).UTC(); !got.Equal(wantTS) {
					t.Fatalf("ts = %v, want %v", got, wantTS)
				}
			},
		},
		{
			name:    "quote",
			payload: `[{"T":"q","S":"MSFT","bp":410.1,"bs":5,"ap":410.3,"as":7,"t":"` + ts + `"}]`,
			check: func(t *testing.T, ev MarketEvent) {
				if ev.Quote == nil {
					t.Fatalf("want quote event")
				}
				q := ev.Quote
				if q.Symbol != "MSFT" || q.BidPx != 410.1 || q.BidSz != 5 || q.AskPx != 410.3 || q.AskSz != 7 {
					t.Fatalf("quote = %+v", q)
				}
			},
		},
		{
			name:    "bar",
			payload: `[{"T":"b","S":"TSLA","o":250,"h":255,"l":249,"c":252.25,"v":1200,"n":42,"vw":251.9,"t":"` + ts + `"}]`,
			check: func(t *testing.T, ev MarketEvent) {
				if ev.Bar == nil {
					t.Fatalf("want bar event")
				}
				b := ev.Bar
				if b.Symbol != "TSLA" || b.Open != 250 || b.High != 255 || b.Low != 249 ||
					b.Close != 252.25 || b.Volume != 1200 || b.N != 42 || b.VWAP != 251.9 {
					t.Fatalf("bar = %+v", b)
				}
			},
		},
		{
			name:    "subscription ack ignored",
			payload: `[{"T":"subscription","trades":["AAPL"],"bars":["AAPL"]}]`,
			wantNil: true,
		},
		{
			name:    "success control ignored",
			payload: `[{"T":"success","msg":"connected"}]`,
			wantNil: true,
		},
		{
			name:    "unknown type ignored",
			payload: `[{"T":"x","S":"AAPL"}]`,
			wantNil: true,
		},
		{
			name:    "array mixing data and control",
			payload: `[{"T":"success","msg":"authenticated"},{"T":"t","S":"AAPL","p":10,"s":1,"t":"` + ts + `"}]`,
			check: func(t *testing.T, ev MarketEvent) {
				if ev.Trade == nil || ev.Trade.Symbol != "AAPL" {
					t.Fatalf("want AAPL trade, got %+v", ev)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envs, err := decodeEnvelopes([]byte(tt.payload))
			if err != nil {
				t.Fatalf("decodeEnvelopes: %v", err)
			}
			var got []MarketEvent
			for _, e := range envs {
				if ev, ok := parseMarketEnvelope(e); ok {
					got = append(got, ev)
				}
			}
			if tt.wantNil {
				if len(got) != 0 {
					t.Fatalf("expected no events, got %d", len(got))
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			tt.check(t, got[0])
		})
	}
}

func TestDecodeEnvelopesBareObjectTolerated(t *testing.T) {
	envs, err := decodeEnvelopes([]byte(`{"T":"success","msg":"connected"}`))
	if err != nil || len(envs) != 1 || envs[0].T != "success" {
		t.Fatalf("bare object decode = %v, %v", envs, err)
	}
}

func TestDecodeEnvelopesArrayWithMalformedElements(t *testing.T) {
	// One good envelope, one non-object (bare number), one null, and
	// one unknown-typed object: the good ones must survive and the
	// non-object elements be skipped without an error.
	payload := `[{"T":"success","msg":"authenticated"},42,null,{"T":"x","S":"AAPL"},{"T":"t","S":"MSFT","p":410.5,"s":3,"t":"2026-08-25T14:30:00Z"}]`
	envs, err := decodeEnvelopes([]byte(payload))
	if err != nil {
		t.Fatalf("decodeEnvelopes: %v", err)
	}
	if len(envs) != 3 {
		t.Fatalf("got %d envelopes, want 3 (non-object elements skipped): %+v", len(envs), envs)
	}
	if envs[0].T != "success" || envs[0].Msg != "authenticated" {
		t.Fatalf("envs[0] = %+v", envs[0])
	}
	if envs[1].T != "x" {
		t.Fatalf("envs[1] = %+v", envs[1])
	}
	if envs[2].T != "t" || envs[2].S != "MSFT" || envs[2].P != 410.5 || envs[2].Sz != 3 {
		t.Fatalf("envs[2] = %+v", envs[2])
	}
}

func TestDecodeEnvelopesEmptyArray(t *testing.T) {
	envs, err := decodeEnvelopes([]byte(`[]`))
	if err != nil || len(envs) != 0 {
		t.Fatalf("empty array decode = %v, %v", envs, err)
	}
}

func TestMarketDispatchArrayFrameDeliversEvents(t *testing.T) {
	m := &MarketStream{events: make(chan MarketEvent, 8), log: slog.New(slog.DiscardHandler)}
	m.dispatch(context.Background(), websocket.TextMessage,
		[]byte(`[{"T":"success","msg":"authenticated"},{"T":"t","S":"AAPL","p":227.5,"s":100,"t":"2026-08-25T14:30:00Z"}]`))
	select {
	case ev := <-m.events:
		if ev.Trade == nil || ev.Trade.Symbol != "AAPL" || ev.Trade.Price != 227.5 {
			t.Fatalf("trade from array frame = %+v", ev.Trade)
		}
	default:
		t.Fatalf("no event delivered from array frame")
	}
}

func TestParseFillEvents(t *testing.T) {
	mkPayload := func(event string, order map[string]any) []byte {
		b, _ := json.Marshal(map[string]any{
			"stream": "trade_updates",
			"data": map[string]any{
				"event": event,
				"price": "436.31",
				"qty":   "1",
				"order": order,
			},
		})
		return b
	}
	tests := []struct {
		name      string
		payload   []byte
		wantEvent bool
		want      FillEvent
	}{
		{
			name: "fill uses top-level price and qty",
			payload: mkPayload("fill", map[string]any{
				"id": "abc", "symbol": "TSLA", "side": "BUY",
				"filled_qty": "3", "filled_avg_price": "999",
			}),
			wantEvent: true,
			want:      FillEvent{OrderID: "abc", Symbol: "TSLA", Side: "buy", Qty: 1, Price: 436.31},
		},
		{
			name: "partial_fill same shape",
			payload: mkPayload("partial_fill", map[string]any{
				"id": "def", "symbol": "AAPL", "side": "sell",
			}),
			wantEvent: true,
			want:      FillEvent{OrderID: "def", Symbol: "AAPL", Side: "sell", Qty: 1, Price: 436.31},
		},
		{
			name:      "new event skipped",
			payload:   mkPayload("new", map[string]any{"id": "ghi", "symbol": "AAPL", "side": "buy"}),
			wantEvent: false,
		},
		{
			name:      "canceled event skipped",
			payload:   mkPayload("canceled", map[string]any{"id": "jkl", "symbol": "AAPL", "side": "buy"}),
			wantEvent: false,
		},
		{
			name: "missing top-level qty falls back to filled_qty",
			payload: func() []byte {
				b, _ := json.Marshal(map[string]any{
					"stream": "trade_updates",
					"data": map[string]any{
						"event": "fill",
						"price": "",
						"qty":   "",
						"order": map[string]any{
							"id":               "mno",
							"symbol":           "NVDA",
							"side":             "buy",
							"filled_qty":       "12",
							"filled_avg_price": "101.5",
						},
					},
				})
				return b
			}(),
			wantEvent: true,
			want:      FillEvent{OrderID: "mno", Symbol: "NVDA", Side: "buy", Qty: 12, Price: 101.5},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tu tradeUpdate
			if err := json.Unmarshal(tt.payload, &tu); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			got, ok := parseFillEvent(tu)
			if ok != tt.wantEvent {
				t.Fatalf("ok = %v, want %v", ok, tt.wantEvent)
			}
			if !tt.wantEvent {
				return
			}
			if got != tt.want {
				t.Fatalf("fill = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDispatchMalformedFrameDoesNotPanicOrDeliver(t *testing.T) {
	m := &MarketStream{events: make(chan MarketEvent, 8), log: slog.New(slog.DiscardHandler)}
	m.dispatch(context.Background(), websocket.TextMessage, []byte(`not json at all`))
	m.dispatch(context.Background(), websocket.TextMessage, []byte(`[{"T":"t"`))
	select {
	case ev := <-m.events:
		t.Fatalf("unexpected event from malformed frames: %+v", ev)
	default:
	}
}

// ---- binary-frame decode ----

// binaryJSON wraps a JSON payload as a binary websocket frame body.
// gorilla delivers binary and text identically to ReadMessage; the point
// of this test is that our decoder never inspects the message type.
func TestBinaryFrameCarryingTradeUpdate(t *testing.T) {
	payload := []byte(`{"stream":"trade_updates","data":{"event":"fill","price":"250.75","qty":"4","timestamp":1756127400000000000,"order":{"id":"bin-1","symbol":"AMD","side":"sell"}}}`)
	bin := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(bin[:4], uint32(len(payload)))
	copy(bin[4:], payload)

	ts := &TradeStream{fills: make(chan FillEvent, 8), log: slog.New(slog.DiscardHandler)}
	ts.dispatch(context.Background(), websocket.BinaryMessage, bin[4:]) // body only; framing header is transport-level
	select {
	case f := <-ts.fills:
		if f.OrderID != "bin-1" || f.Symbol != "AMD" || f.Side != "sell" || f.Qty != 4 || f.Price != 250.75 {
			t.Fatalf("fill = %+v", f)
		}
		if f.TS != 1756127400000000000 {
			t.Fatalf("ts = %d", f.TS)
		}
	default:
		t.Fatalf("no fill delivered from binary frame")
	}
}

// ---- handshake / lifecycle with fake conns ----

func newTestMarket(cfg MarketConfig) *MarketStream {
	m, err := NewMarketStream(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		panic(err)
	}
	return m
}

func TestMarketHandshakeAuthAndSubscribe(t *testing.T) {
	fake := &fakeConn{}
	fake.readScript = [][]byte{
		[]byte(`[{"T":"success","msg":"connected"}]`),
		[]byte(`[{"T":"success","msg":"authenticated"}]`),
		[]byte(`[{"T":"subscription","trades":["AAPL"],"quotes":["AAPL"],"bars":["AAPL"]}]`),
	}
	m := newTestMarket(MarketConfig{KeyID: "k", Secret: "s", Feed: "iex", Symbols: []string{"AAPL"}})
	if err := m.handshake(fake); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	writes := fake.writes()
	if len(writes) != 2 {
		t.Fatalf("wrote %d frames, want 2 (auth + subscribe)", len(writes))
	}
	var auth map[string]any
	if err := json.Unmarshal(writes[0], &auth); err != nil || auth["action"] != "auth" ||
		auth["key"] != "k" || auth["secret"] != "s" {
		t.Fatalf("auth frame = %s (%v)", writes[0], err)
	}
	var sub map[string]any
	if err := json.Unmarshal(writes[1], &sub); err != nil || sub["action"] != "subscribe" {
		t.Fatalf("subscribe frame = %s (%v)", writes[1], err)
	}
	for _, k := range []string{"trades", "quotes", "bars"} {
		syms, ok := sub[k].([]any)
		if !ok || len(syms) != 1 || syms[0] != "AAPL" {
			t.Fatalf("subscribe %s = %v, want [AAPL]", k, sub[k])
		}
	}
}

func TestHandshakeRejectsServerErrorFrame(t *testing.T) {
	fake := &fakeConn{}
	fake.readScript = [][]byte{[]byte(`[{"T":"error","msg":"auth failed"}]`)}
	m := newTestMarket(MarketConfig{KeyID: "k", Secret: "s", Symbols: []string{"AAPL"}})
	if err := m.handshake(fake); err == nil {
		t.Fatalf("expected handshake failure on error greeting")
	}
}

func TestReconnectReAuthsAndResubscribes(t *testing.T) {
	var mu sync.Mutex
	dials := 0
	first := &fakeConn{}
	first.readScript = [][]byte{
		[]byte(`[{"T":"success","msg":"connected"}]`),
		[]byte(`[{"T":"success","msg":"authenticated"}]`),
		[]byte(`[{"T":"subscription"}]`),
		[]byte(`[{"T":"t","S":"AAPL","p":1,"s":1,"t":"2026-08-25T14:30:00Z"}]`),
	}
	second := &fakeConn{}
	second.readScript = [][]byte{
		[]byte(`[{"T":"success","msg":"connected"}]`),
		[]byte(`[{"T":"success","msg":"authenticated"}]`),
		[]byte(`[{"T":"subscription"}]`),
		[]byte(`[{"T":"b","S":"AAPL","o":1,"h":2,"l":0.5,"c":1.5,"v":10,"n":3,"vw":1.4,"t":"2026-08-25T14:30:01Z"}]`),
	}
	conns := []*fakeConn{first, second}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newTestMarket(MarketConfig{KeyID: "k", Secret: "s", Symbols: []string{"AAPL"}})
	m.dialer = func(context.Context, string) (conn, error) {
		mu.Lock()
		defer mu.Unlock()
		i := dials
		dials++
		if i >= len(conns) {
			cancel() // stop after second session dies
			return nil, errors.New("done dialing")
		}
		return conns[i], nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runStream(ctx, m) // reuse core loop directly so we can inject conns
	}()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := dials
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for reconnect (dials = %d)", n)
		case <-time.After(5 * time.Millisecond):
		}
	}
	<-done

	mu.Lock()
	defer mu.Unlock()

	// The real assertions: two dials happened, each conn saw an auth write
	// followed by a subscribe write.
	if dials < 2 {
		t.Fatalf("dials = %d, want >= 2 (reconnect must re-dial)", dials)
	}
	for i, c := range conns[:min(dials, len(conns))] {
		w := c.writes()
		if len(w) < 2 {
			t.Fatalf("conn %d wrote %d frames, want auth+subscribe", i, len(w))
		}
		if !containsAction(w[0], "auth") || !containsAction(w[1], "subscribe") {
			t.Fatalf("conn %d frames not auth->subscribe: %s | %s", i, w[0], w[1])
		}
	}
}

func containsAction(b []byte, action string) bool {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	return m["action"] == action
}

func TestNextBackoffCappedExponential(t *testing.T) {
	// Jitter is ±25%, so every delay must land in [d*3/4, d*5/4].
	tests := []struct {
		attempt int
		nominal time.Duration
	}{
		{0, time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, backoffMax}, // 32s base would exceed the cap; clamp to max
		{6, backoffMax},
		{20, backoffMax},
		{-1, time.Second},
	}
	for _, tt := range tests {
		for i := 0; i < 50; i++ { // sample repeatedly to cover jitter range
			got := nextBackoff(tt.attempt)
			lo := tt.nominal * 3 / 4
			hi := tt.nominal * 5 / 4
			if got < lo || got > hi {
				t.Errorf("nextBackoff(%d) = %v, want within [%v, %v]", tt.attempt, got, lo, hi)
				break
			}
		}
	}
}

func TestContextCancellationStopsRunPromptly(t *testing.T) {
	blocking := &blockingConn{}
	m := newTestMarket(MarketConfig{KeyID: "k", Secret: "s", Symbols: []string{"AAPL"}})
	m.dialer = func(context.Context, string) (conn, error) { return blocking, nil }

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- m.Run(ctx) }()

	// Wait until Run is blocked inside a read, then cancel.
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case <-errc:
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("shutdown took %v; ctx watch should unblock reads", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not return after context cancellation")
	}
}

// blockingConn blocks forever in ReadMessage, simulating a
// healthy-but-silent connection. When watchCtx expires the read deadline
// on shutdown (a past timestamp), the read unblocks immediately.
type blockingConn struct {
	unblocked chan struct{}
	once      sync.Once
}

var errReadAborted = errors.New("read aborted by deadline")

func (b *blockingConn) ReadMessage() (int, []byte, error) {
	b.ensureCh()
	<-b.unblocked
	return 0, nil, errReadAborted
}

func (b *blockingConn) ensureCh() {
	b.once.Do(func() { b.unblocked = make(chan struct{}) })
}

func (b *blockingConn) WriteMessage(int, []byte) error { return nil }
func (b *blockingConn) SetReadDeadline(t time.Time) error {
	if t.Before(time.Now()) {
		// watchCtx expires the deadline on shutdown: unblock the reader.
		b.ensureCh()
		close(b.unblocked)
	}
	return nil
}
func (b *blockingConn) Close() error { return nil }

// ---- constructor validation ----

func TestConstructorsValidateConfig(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if _, err := NewMarketStream(MarketConfig{Secret: "s", Symbols: []string{"A"}}, log); err == nil {
		t.Error("market: expected error for missing key ID")
	}
	if _, err := NewMarketStream(MarketConfig{KeyID: "k", Secret: "s"}, log); err == nil {
		t.Error("market: expected error for missing symbols")
	}
	if _, err := NewMarketStream(MarketConfig{KeyID: "k", Secret: "s", Feed: "polygon", Symbols: []string{"A"}}, log); err == nil {
		t.Error("market: expected error for unknown feed")
	}
	if _, err := NewMarketStream(MarketConfig{KeyID: "k", Secret: "s", Feed: "", Symbols: []string{"A"}}, log); err != nil {
		t.Errorf("market: empty feed should default to iex, got %v", err)
	}
	if _, err := NewTradeStream(TradeConfig{Secret: "s", Host: "wss://paper-api.alpaca.markets"}, log); err == nil {
		t.Error("trades: expected error for missing key ID")
	}
	if _, err := NewTradeStream(TradeConfig{KeyID: "k", Secret: "s", Host: ""}, log); err == nil {
		t.Error("trades: expected error for missing host")
	}
}

func TestTradeEndpointDerivedFromHost(t *testing.T) {
	tests := []struct{ host, want string }{
		{"wss://paper-api.alpaca.markets", "wss://paper-api.alpaca.markets/stream"},
		{"wss://api.alpaca.markets/", "wss://api.alpaca.markets/stream"},
	}
	for _, tt := range tests {
		ts, err := NewTradeStream(TradeConfig{KeyID: "k", Secret: "s", Host: tt.host}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("NewTradeStream(%q): %v", tt.host, err)
		}
		if got := ts.endpoint(); got != tt.want {
			t.Errorf("endpoint(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}
