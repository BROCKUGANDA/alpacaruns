package stream

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type MarketConfig struct {
	KeyID  string
	Secret string
	// Feed is "iex" (free) or "sip" (paid). Empty defaults to "iex".
	Feed string
	// Symbols subscribed for trades, quotes, and bars.
	Symbols []string
}

// Trade is one tape trade (wire type "t").
type Trade struct {
	Symbol string
	Price  float64
	Size   float64
	TS     int64 // unix nanos
}

// Quote is one NBBO-style quote (wire type "q").
type Quote struct {
	Symbol string
	BidPx  float64
	BidSz  float64
	AskPx  float64
	AskSz  float64
	TS     int64 // unix nanos
}

// Bar is one OHLCV aggregate (wire type "b").
type Bar struct {
	Symbol string
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
	TS     int64 // unix nanos, bar open time
	N      int
	VWAP   float64
}

// MarketEvent is exactly one of the pointer fields, non-nil.
type MarketEvent struct {
	Trade *Trade
	Quote *Quote
	Bar   *Bar
}

// MarketStream is a live Alpaca market-data stream. Create with
// NewMarketStream, then Run in its own goroutine and consume Events.
// Events is closed when Run returns.
type MarketStream struct {
	cfg    MarketConfig
	events chan MarketEvent
	log    *slog.Logger
	dialer dialFunc
}

// NewMarketStream creates a market stream. Feed defaults to "iex"; an
// unknown feed value is rejected here so misconfiguration fails at setup
// rather than as a silent empty stream.
func NewMarketStream(cfg MarketConfig, log *slog.Logger) (*MarketStream, error) {
	if cfg.KeyID == "" || cfg.Secret == "" {
		return nil, fmt.Errorf("stream: market: key ID and secret are required")
	}
	if len(cfg.Symbols) == 0 {
		return nil, fmt.Errorf("stream: market: at least one symbol required")
	}
	switch strings.ToLower(cfg.Feed) {
	case "", "iex":
		cfg.Feed = "iex"
	case "sip":
		cfg.Feed = "sip"
	default:
		return nil, fmt.Errorf("stream: market: unknown feed %q (want iex or sip)", cfg.Feed)
	}
	if log == nil {
		log = slog.Default()
	}
	return &MarketStream{
		cfg:    cfg,
		events: make(chan MarketEvent, 256),
		log:    log,
		dialer: defaultDial,
	}, nil
}

// Events yields parsed trades, quotes, and bars. Closed when Run returns.
func (m *MarketStream) Events() <-chan MarketEvent { return m.events }

// Close releases the event channel; call after Run has returned.
func (m *MarketStream) Close() { close(m.events) }

// Run blocks until ctx is cancelled, maintaining the connection across
// faults with capped exponential backoff. Every (re)connect re-runs the
// full auth + subscribe handshake.
func (m *MarketStream) Run(ctx context.Context) error {
	err := runStream(ctx, m)
	return err
}

// ---- streamCore plumbing ----

func (m *MarketStream) endpoint() string {
	return fmt.Sprintf("wss://stream.data.alpaca.markets/v2/%s", m.cfg.Feed)
}

func (m *MarketStream) logger() *slog.Logger { return m.log }

func (m *MarketStream) connect(ctx context.Context) (conn, error) { return m.dialer(ctx, m.endpoint()) }

func (m *MarketStream) retryDelay(attempt int) time.Duration { return nextBackoff(attempt) }

func (m *MarketStream) handshake(c conn) error {
	if err := authenticate(c, m.cfg.KeyID, m.cfg.Secret); err != nil {
		return err
	}
	sub := map[string]any{"action": "subscribe"}
	for _, k := range []string{"trades", "quotes", "bars"} {
		sub[k] = m.cfg.Symbols
	}
	req := mustJSON(sub)
	if err := c.WriteMessage(websocket.TextMessage, req); err != nil {
		return fmt.Errorf("subscribe write: %w", err)
	}
	// Ack is a subscription confirmation frame; exact contents vary by
	// feed version, so only require it to be well-formed.
	if _, data, err := readWithTimeout(c, handshakeTimeout); err != nil {
		return fmt.Errorf("subscribe ack: %w", err)
	} else if envs, derr := decodeEnvelopes(data); derr != nil {
		return fmt.Errorf("subscribe ack: malformed: %w", derr)
	} else {
		for _, e := range envs {
			if e.T == "error" {
				return fmt.Errorf("subscribe rejected: %s", e.Msg)
			}
		}
	}
	m.log.Info("stream: market data subscribed",
		"feed", m.cfg.Feed, "symbols", len(m.cfg.Symbols))
	return nil
}

// dispatch parses one wire message into zero or more events and delivers
// them. Malformed payloads are logged and dropped — never fatal.
func (m *MarketStream) dispatch(ctx context.Context, typ int, data []byte) {
	envs, err := decodeEnvelopes(data)
	if err != nil {
		m.log.Warn("stream: market: dropping malformed frame", "err", err)
		return
	}
	for _, e := range envs {
		ev, ok := parseMarketEnvelope(e)
		if !ok {
			continue // control frames (success/error/subscription) and unknown types
		}
		select {
		case m.events <- ev:
		case <-ctx.Done():
			return
		}
	}
}

// parseMarketEnvelope converts one envelope to a typed event. ok=false
// means control/unknown frame.
func parseMarketEnvelope(e envelope) (ev MarketEvent, ok bool) {
	ts := e.TS.UnixNano()
	switch e.T {
	case "t":
		return MarketEvent{Trade: &Trade{Symbol: e.S, Price: e.P, Size: e.Sz, TS: ts}}, true
	case "q":
		return MarketEvent{Quote: &Quote{Symbol: e.S, BidPx: e.BP, BidSz: e.BS, AskPx: e.AP, AskSz: e.AS, TS: ts}}, true
	case "b":
		return MarketEvent{Bar: &Bar{Symbol: e.S, Open: e.O, High: e.H, Low: e.L, Close: e.C, Volume: e.V, TS: ts, N: e.N, VWAP: e.VW}}, true
	default:
		return MarketEvent{}, false
	}
}
