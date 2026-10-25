package main

// livebook.go: a short-TTL view of the broker's positions, shared by the
// risk gate and the entry paths.
//
// Two defects this closes, both found by reading the code rather than the
// README:
//
//   - The auto path's validator was built without a Positions source, so
//     the additive-exposure check in risk/risk.go was dead on exactly the
//     path that trades unattended. Every cap was evaluated per order, never
//     against the book.
//   - The tick's position cache was frozen at tick start, so two BUYs for
//     the same symbol in one tick both saw the pre-tick exposure and both
//     passed the per-symbol cap.
//
// The answer is not a longer cache. Exposure limits are only meaningful
// against a read that is current enough to include what this process just
// bought, so the book is invalidated on every mutation the loop makes and
// falls back to a short TTL for everything else.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/strategy"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// Entry submission wrappers. Every path that can change the book goes
// through one of these so the gate's cached view is dropped afterwards: a
// second entry in the same tick must be sized against what the broker
// holds NOW, not against the list fetched before the first fill landed.
// The invalidation is unconditional — a rejected order may still have
// cancelled or replaced something on its way to being rejected.

func (l *autoLoop) submitEntry(ctx context.Context, plan strategy.EntryPlan, conf float64) error {
	err := l.exec.ExecuteEntry(ctx, plan, conf)
	l.book.Invalidate()
	return err
}

func (l *autoLoop) submitOptionEntry(ctx context.Context, leg *strategy.OptionLeg, conf float64) error {
	err := l.exec.ExecuteOptionEntry(ctx, leg, conf)
	l.book.Invalidate()
	return err
}

func (l *autoLoop) submitPreOrder(ctx context.Context, plan strategy.EntryPlan, conf float64) error {
	err := l.exec.ExecutePreOrder(ctx, plan, conf)
	l.book.Invalidate()
	return err
}

// bookTTL bounds how stale a position snapshot may be when the gate asks.
// It is deliberately shorter than one tick: a protective close performed by
// the monitor (which cannot reach this cache) must not leave the gate
// sizing against an account that no longer looks like that.
const bookTTL = 15 * time.Second

type liveBook struct {
	mu      sync.Mutex
	client  *tools.Client
	ttl     time.Duration
	at      time.Time
	entries []tools.Position
}

func newLiveBook(c *tools.Client) *liveBook {
	return &liveBook{client: c, ttl: bookTTL}
}

// snapshot returns the positions list, refetching when it is missing or
// older than the TTL.
func (b *liveBook) snapshot(ctx context.Context) ([]tools.Position, error) {
	if b == nil || b.client == nil {
		return nil, context.Canceled
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries != nil && time.Since(b.at) < b.ttl {
		return b.entries, nil
	}
	pos, err := b.client.GetPositions(ctx)
	if err != nil {
		// A failed read is NOT reported as an empty book. Returning the
		// stale list would let the gate size against a position set the
		// broker no longer agrees with; returning nothing makes the caller
		// fail closed, which is the only safe reading of "unknown".
		return nil, err
	}
	b.entries = pos
	b.at = time.Now()
	return pos, nil
}

// Invalidate drops the cached view. Call it after any order this process
// places or cancels, so the next exposure question is answered from the
// broker rather than from a book that already changed.
func (b *liveBook) Invalidate() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = nil
	b.at = time.Time{}
}

// exposureUSD returns the market value held in a symbol, or an error when
// the book cannot be read. The gate's own Positions hook takes a symbol and
// returns a plain float64, so the caller adapts it — see wireGateToBook.
func (b *liveBook) exposureUSD(ctx context.Context, symbol string) (float64, error) {
	pos, err := b.snapshot(ctx)
	if err != nil {
		return 0, err
	}
	var sum float64
	for _, p := range pos {
		if !tools.SameSymbol(p.Symbol, symbol) {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(p.Qty), 64)
		if err != nil {
			return 0, fmt.Errorf("unreadable qty for %s: %q", p.Symbol, p.Qty)
		}
		px, err := strconv.ParseFloat(strings.TrimSpace(p.CurrentPrice), 64)
		if err != nil {
			return 0, fmt.Errorf("unreadable price for %s: %q", p.Symbol, p.CurrentPrice)
		}
		sum += math.Abs(q * px)
	}
	return sum, nil
}

// signedQty returns the live signed quantity (+long, -short) for a symbol,
// and 0 when flat. This is what makes a "reducing" assertion provable.
func (b *liveBook) signedQty(ctx context.Context, symbol string) (float64, error) {
	pos, err := b.snapshot(ctx)
	if err != nil {
		return 0, err
	}
	for _, p := range pos {
		if !tools.SameSymbol(p.Symbol, symbol) {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(p.Qty), 64)
		if err != nil {
			return 0, fmt.Errorf("unreadable qty for %s: %q", p.Symbol, p.Qty)
		}
		return q, nil
	}
	return 0, nil
}

// grossUSD returns the book's total absolute market value, the number a margin
// ceiling applies to. Longs and shorts are added by absolute value: a long and
// a short of the same size still use buying power twice.
func (b *liveBook) grossUSD(ctx context.Context) (float64, error) {
	pos, err := b.snapshot(ctx)
	if err != nil {
		return 0, err
	}
	var sum float64
	for _, p := range pos {
		q, err := strconv.ParseFloat(strings.TrimSpace(p.Qty), 64)
		if err != nil {
			continue
		}
		px, err := strconv.ParseFloat(strings.TrimSpace(p.CurrentPrice), 64)
		if err != nil || px <= 0 {
			continue
		}
		sum += math.Abs(q * px)
	}
	return sum, nil
}

// quotedSpreadBps returns the spread at the touch, in basis points. It is a
// liquidity measurement from the live quote, not a cost model: the caller
// uses it to refuse entries it cannot plausibly pay for.
func (l *autoLoop) quotedSpreadBps(ctx context.Context, symbol string) (float64, bool) {
	if l == nil || l.client == nil {
		return 0, false
	}
	if strategy.IsCrypto(symbol) {
		m, err := l.client.GetCryptoSnapshots(ctx, []string{tools.CryptoDataSymbol(symbol)})
		if err != nil || len(m) == 0 {
			return 0, false
		}
		for _, snap := range m { // the response is keyed by the requested form
			return spreadBps(snap.BidPrice, snap.AskPrice)
		}
		return 0, false
	}
	m, err := l.client.GetSnapshots(ctx, []string{symbol})
	if err != nil {
		return 0, false
	}
	snap, ok := m[symbol]
	if !ok {
		return 0, false
	}
	return spreadBps(snap.LatestQuoteBid, snap.LatestQuoteAsk)
}

func spreadBps(bid, ask float64) (float64, bool) {
	if bid <= 0 || ask <= 0 || ask < bid {
		return 0, false
	}
	mid := (bid + ask) / 2
	if mid <= 0 {
		return 0, false
	}
	return (ask - bid) / mid * 10000, true
}
