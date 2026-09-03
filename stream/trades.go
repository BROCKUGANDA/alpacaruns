package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// TradeConfig configures the account trade_updates stream.
type TradeConfig struct {
	KeyID  string
	Secret string
	// Host is derived by the caller from their Alpaca REST base URL:
	// "wss://paper-api.alpaca.markets" for paper keys,
	// "wss://api.alpaca.markets" for live. Trailing slashes are trimmed.
	Host string
}

// FillEvent is a parsed fill (or partial fill) of one order.
type FillEvent struct {
	OrderID string
	Symbol  string
	Side    string // "buy" | "sell"
	Qty     float64
	Price   float64
	TS      int64 // unix nanos, zero when the wire frame lacks a timestamp
}

// tradeUpdate mirrors the {"stream":"trade_updates","data":{...}} payload.
type tradeUpdate struct {
	Stream string `json:"stream"`
	Data   struct {
		Event string  `json:"event"`
		Price string  `json:"price"`
		Qty   string  `json:"qty"`
		TS    int64   `json:"timestamp"`
		Order *order_ `json:"order"`
	} `json:"data"`
}

// order_ is the subset of the order object fills carry.
type order_ struct {
	ID             string  `json:"id"`
	Symbol         string  `json:"symbol"`
	Side           string  `json:"side"`
	FilledQty      float64 `json:"filled_qty,string"`
	FilledAvgPrice float64 `json:"filled_avg_price,string"`
}

// TradeStream is a live account trade_updates stream. Create with
// NewTradeStream, then Run in its own goroutine and consume Fills.
// Fills is closed when Run returns.
type TradeStream struct {
	cfg    TradeConfig
	fills  chan FillEvent
	log    *slog.Logger
	dialer dialFunc
}

// NewTradeStream creates a trade-updates stream for the given host.
func NewTradeStream(cfg TradeConfig, log *slog.Logger) (*TradeStream, error) {
	if cfg.KeyID == "" || cfg.Secret == "" {
		return nil, fmt.Errorf("stream: trades: key ID and secret are required")
	}
	host := strings.TrimRight(cfg.Host, "/")
	if host == "" {
		return nil, fmt.Errorf("stream: trades: host is required (e.g. wss://paper-api.alpaca.markets)")
	}
	if log == nil {
		log = slog.Default()
	}
	return &TradeStream{
		cfg:    cfg,
		fills:  make(chan FillEvent, 128),
		log:    log,
		dialer: defaultDial,
	}, nil
}

// Fills yields parsed fill events. Closed when Run returns.
func (t *TradeStream) Fills() <-chan FillEvent { return t.fills }

// Close releases the fills channel; call after Run has returned.
func (t *TradeStream) Close() { close(t.fills) }

// Run blocks until ctx is cancelled, maintaining the connection across
// faults with capped exponential backoff. Every (re)connect re-runs the
// full auth + listen handshake.
func (t *TradeStream) Run(ctx context.Context) error {
	err := runStream(ctx, t)
	return err
}

// ---- streamCore plumbing ----

func (t *TradeStream) endpoint() string { return strings.TrimRight(t.cfg.Host, "/") + "/stream" }

func (t *TradeStream) logger() *slog.Logger { return t.log }

func (t *TradeStream) connect(ctx context.Context) (conn, error) { return t.dialer(ctx, t.endpoint()) }

func (t *TradeStream) retryDelay(attempt int) time.Duration { return nextBackoff(attempt) }

func (t *TradeStream) handshake(c conn) error {
	if err := authenticate(c, t.cfg.KeyID, t.cfg.Secret); err != nil {
		return err
	}
	req := mustJSON(map[string]any{
		"action": "listen",
		"data":   map[string]any{"streams": []string{"trade_updates"}},
	})
	if err := c.WriteMessage(websocket.TextMessage, req); err != nil {
		return fmt.Errorf("listen write: %w", err)
	}
	// The ack ("listening") arrives as a control frame; require it to be
	// well-formed but tolerate either spelling of the confirmation.
	if _, data, err := readWithTimeout(c, handshakeTimeout); err != nil {
		return fmt.Errorf("listen ack: %w", err)
	} else if envs, derr := decodeEnvelopes(data); derr != nil {
		t.log.Debug("stream: trades: non-envelope listen ack ignored")
	} else {
		for _, e := range envs {
			if e.T == "error" {
				return fmt.Errorf("listen rejected: %s", e.Msg)
			}
		}
	}
	t.log.Info("stream: trade updates listening", "host", t.cfg.Host)
	return nil
}

// dispatch parses one wire message and delivers any fill it carries.
// The paper endpoint sends BINARY frames wrapping the same JSON payloads;
// gorilla surfaces both text and binary through ReadMessage, so typ is
// deliberately not inspected here.
func (t *TradeStream) dispatch(ctx context.Context, typ int, data []byte) {
	var tu tradeUpdate
	if err := json.Unmarshal(data, &tu); err != nil {
		t.log.Warn("stream: trades: dropping malformed frame", "err", err)
		return
	}
	if tu.Stream != "trade_updates" || tu.Data.Order == nil {
		return // other streams / control payloads
	}
	fill, ok := parseFillEvent(tu)
	if !ok {
		return // new/canceled/rejected etc.: lifecycle events, not fills
	}
	select {
	case t.fills <- fill:
	case <-ctx.Done():
	}
}

// parseFillEvent converts fill/partial_fill updates into a FillEvent.
func parseFillEvent(tu tradeUpdate) (FillEvent, bool) {
	o := tu.Data.Order
	if o == nil {
		// A fill without an order object cannot be attributed; drop it.
		return FillEvent{}, false
	}
	switch tu.Data.Event {
	case "fill", "partial_fill":
	default:
		return FillEvent{}, false
	}
	var q float64
	if qty, err := strconv.ParseFloat(strings.TrimSpace(tu.Data.Qty), 64); err == nil && qty > 0 {
		q = qty
	} else if o.FilledQty > 0 {
		q = o.FilledQty
	}
	var p float64
	if price, err := strconv.ParseFloat(strings.TrimSpace(tu.Data.Price), 64); err == nil && price > 0 {
		p = price
	} else if o.FilledAvgPrice > 0 {
		p = o.FilledAvgPrice
	}
	ts := tu.Data.TS // zero when the wire frame omits it; callers journal with their own clock anyway
	return FillEvent{
		OrderID: o.ID,
		Symbol:  o.Symbol,
		Side:    strings.ToLower(o.Side),
		Qty:     q,
		Price:   p,
		TS:      ts,
	}, true
}
