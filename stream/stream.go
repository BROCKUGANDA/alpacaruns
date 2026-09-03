package stream

// Package stream provides live websocket clients for Alpaca: a market
// data feed (trades, quotes, bars) and the account trade_updates stream.
//
// Both clients share one lifecycle: dial -> authenticate -> subscribe ->
// read loop. Every reconnect re-runs authentication and (re-)subscribes,
// waiting a capped exponential backoff between attempts. Malformed
// messages are logged and dropped; they never crash or stall the stream.
// Cancelling the context shuts the stream down cleanly and closes the
// output channel.
//
// Wire protocol (per Alpaca's published streaming docs): see the
// handshake frames documented on each client below.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	// Transport: github.com/gorilla/websocket. Chosen over
	// github.com/coder/websocket because gorilla is already in this
	// module's dependency graph (indirect via google.golang.org/adk),
	// so adopting it adds zero new transitive dependencies, and its
	// blocking ReadMessage API maps directly onto the
	// read-loop-plus-reconnect design below.
	"github.com/gorilla/websocket"
)

const (
	// backoffBase and backoffMax bound the exponential reconnect delay.
	backoffBase = time.Second
	backoffMax  = 30 * time.Second

	// dialTimeout bounds the websocket handshake when opening a
	// connection. Without it a stalled dial (e.g. "handshake i/o
	// timeout" against the paper endpoint) can hang far longer than
	// the reconnect loop expects before surfacing as an error.
	dialTimeout = 10 * time.Second

	// handshakeTimeout bounds each leg of the auth/subscribe exchange.
	handshakeTimeout = 10 * time.Second

	// readIdleTimeout is how long a data-session read may sit silent
	// before the connection is presumed dead and redialed. Control
	// traffic from Alpaca (heartbeats, quotes) refreshes it constantly
	// during market hours; outside hours a redial is harmless.
	readIdleTimeout = 90 * time.Second
)

// conn is the minimal websocket surface the streams depend on. It matches
// *websocket.Conn; tests substitute fakes so nothing here touches a real
// socket.
type conn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetReadDeadline(t time.Time) error
	Close() error
}

// dialFunc opens a websocket connection to urlStr. Injected so tests can
// supply fake connections.
type dialFunc func(ctx context.Context, urlStr string) (conn, error)

// defaultDial dials with a bounded websocket handshake (pong replies to
// server pings are handled automatically by the library).
func defaultDial(ctx context.Context, urlStr string) (conn, error) {
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = dialTimeout
	c, resp, err := dialer.DialContext(ctx, urlStr, nil)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	return c, nil
}

// streamCore abstracts what differs between the market-data and
// trade-updates clients so the reconnect loop below is written once.
type streamCore interface {
	endpoint() string // dial URL, for logs
	logger() *slog.Logger
	connect(ctx context.Context) (conn, error)
	retryDelay(attempt int) time.Duration
	handshake(c conn) error // auth (+ subscribe/listen) after dial
	dispatch(ctx context.Context, typ int, data []byte)
}

// runStream owns the shared reconnect loop. It blocks until ctx is
// cancelled; the owning client closes its output channel on return.
func runStream(ctx context.Context, sc streamCore) error {
	log := sc.logger()
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			d := sc.retryDelay(attempt - 1)
			log.Warn("stream: reconnecting", "endpoint", sc.endpoint(), "attempt", attempt, "backoff", d.String())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		}
		c, err := sc.connect(ctx)
		if err != nil {
			log.Error("stream: dial failed", "endpoint", sc.endpoint(), "err", err)
			attempt++
			continue
		}
		err = runSession(ctx, sc, c)
		_ = c.Close()
		if isShutdown(err) {
			return err
		}
		if errors.Is(err, errHandshake) {
			// Never got healthy: keep compounding the backoff.
			attempt++
		} else {
			// The session ran past the handshake (data flowed), so
			// the next fault backs off from base again.
			attempt = 0
		}
		log.Error("stream: session ended; will redial", "endpoint", sc.endpoint(), "err", err)
	}
}

// errHandshake marks a session failure during the auth/subscribe
// exchange. Sessions that never got healthy keep their backoff;
// anything that survived the handshake resets it.
var errHandshake = errors.New("handshake")

// runSession runs one dial's worth of handshake plus data loop.
func runSession(ctx context.Context, sc streamCore, c conn) error {
	stop := watchCtx(ctx, c)
	defer stop()

	if err := sc.handshake(c); err != nil {
		return fmt.Errorf("%w: %w", errHandshake, err)
	}
	for {
		typ, data, err := readWithTimeout(c, readIdleTimeout)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		sc.dispatch(ctx, typ, data)
	}
}

// watchCtx force-unblocks a pending read by expiring the read deadline as
// soon as ctx is cancelled, so shutdown is prompt even mid-read.
func watchCtx(ctx context.Context, c conn) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// A past deadline aborts any in-flight read.
			_ = c.SetReadDeadline(time.Now().Add(-time.Hour))
		case <-done:
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// ---- wire helpers ----

// authRequest is the shared {"action":"auth"} frame.
type authRequest struct {
	Action string `json:"action"`
	Key    string `json:"key"`
	Secret string `json:"secret"`
}

// authenticate authenticates the session and waits for the ack.
//
// Alpaca's docs describe a "connected" greeting before auth, but the
// paper endpoint has been observed skipping it entirely: the websocket
// upgrade completes and the first frame is only sent after the client
// authenticates (verified by raw-frame probes in Aug 2025). Waiting for
// the greeting therefore deadlocks every dial, so the greeting is not
// awaited at all; if a future endpoint sends one, awaitControl below
// simply consumes it while looking for "authenticated".
func authenticate(c conn, keyID, secret string) error {
	req, err := json.Marshal(authRequest{Action: "auth", Key: keyID, Secret: secret})
	if err != nil {
		return err
	}
	if err := c.WriteMessage(websocket.TextMessage, req); err != nil {
		return fmt.Errorf("auth write: %w", err)
	}
	if err := awaitControl(c, "success", "authenticated"); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	return nil
}

// envelope mirrors the market-data wire format: a JSON object keyed by
// "T". Field names cover every payload the package parses; irrelevant
// fields are simply zero.
type envelope struct {
	T   string    `json:"T"`
	S   string    `json:"S"` // symbol
	P   float64   `json:"p"` // trade price
	Sz  float64   `json:"s"` // trade size
	BP  float64   `json:"bp"`
	BS  float64   `json:"bs"`
	AP  float64   `json:"ap"`
	AS  float64   `json:"as"`
	O   float64   `json:"o"`
	H   float64   `json:"h"`
	L   float64   `json:"l"`
	C   float64   `json:"c"`
	V   float64   `json:"v"`
	N   int       `json:"n"`
	VW  float64   `json:"vw"`
	TS  time.Time `json:"t"`
	Msg string    `json:"msg"`
}

// decodeEnvelopes decodes one wire message payload. Alpaca sends arrays
// of envelope objects; a bare object is tolerated defensively. Array
// frames are decoded element-wise so a single malformed element (or a
// non-object like a bare number) is skipped instead of dropping the
// whole batch — Alpaca batches control and data frames together, and
// one bad element must not discard the good ones.
func decodeEnvelopes(data []byte) ([]envelope, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var raws []json.RawMessage
		if err := json.Unmarshal(trimmed, &raws); err != nil {
			return nil, err
		}
		envs := make([]envelope, 0, len(raws))
		for _, raw := range raws {
			var e envelope
			if err := json.Unmarshal(raw, &e); err != nil || e.T == "" {
				continue // non-object, malformed, or typeless element: skip it
			}
			envs = append(envs, e)
		}
		return envs, nil
	}
	var one envelope
	if err := json.Unmarshal(data, &one); err != nil {
		return nil, err
	}
	return []envelope{one}, nil
}

// readWithTimeout bounds a single read attempt.
func readWithTimeout(c conn, d time.Duration) (int, []byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(d))
	return c.ReadMessage()
}

// awaitControl reads frames until the wanted control type arrives.
// wantMsg optionally narrows "success" frames by their msg field.
// Server "error" frames abort immediately.
func awaitControl(c conn, wantT, wantMsg string) error {
	for {
		_, data, err := readWithTimeout(c, handshakeTimeout)
		if err != nil {
			return err
		}
		envs, decErr := decodeEnvelopes(data)
		if decErr != nil {
			continue // malformed control frame: skip
		}
		for _, e := range envs {
			switch {
			case e.T == "error":
				return fmt.Errorf("server error: %s", e.Msg)
			case e.T == wantT && (wantMsg == "" || e.Msg == wantMsg):
				return nil
			}
		}
	}
}

// nextBackoff returns the delay before the n-th retry (0-based): 1s,
// 2s, 4s, .. capped at backoffMax, with ±25% jitter so a fleet of
// reconnecting clients does not synchronize. The caller resets n to
// base after any healthy session (see runStream).
func nextBackoff(n int) time.Duration {
	if n < 0 {
		n = 0
	}
	d := backoffBase << uint(n) // overflow-safe via the guards below
	if n >= 32 || d <= 0 || d > backoffMax {
		return backoffMax + jitter(backoffMax)
	}
	return d + jitter(d)
}

// jitter returns a value in [-d/4, +d/4], deterministic per call.
func jitter(d time.Duration) time.Duration {
	q := d / 4
	if q <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(2*q)+1)) - q
}

// isShutdown reports whether err merely reflects context cancellation —
// a normal, user-initiated stream shutdown rather than a fault.
func isShutdown(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// mustJSON marshals v or panics; it is only called with literal map
// values that always marshal successfully.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("stream: mustJSON: %v", err))
	}
	return b
}
