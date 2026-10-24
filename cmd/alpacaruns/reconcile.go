package main

// reconcile.go closes the gap between what the broker filled and what the
// journal recorded.
//
// The journal is not a log for humans only: the drawdown halts compute
// realized P/L from it, the stats ledger rebuilds itself from it, and the
// learner calibrates from it. Before this pass, a fill reached the journal
// only if it was already present in the POST /v2/orders response. Orders
// that Alpaca accepts and fills a moment later — the normal case for a
// market order near the close, and the only case for a resting pre-order —
// never appeared. Measured on the live paper account: two filled entry
// orders at the broker, zero fill rows in the journal, so every safety
// number derived from it read zero.
//
// The pass is a diff, not an append: it journals only the quantity the
// broker reports as filled ABOVE what is already recorded for that order
// id. That makes it idempotent across restarts and correct across partial
// fills, with no extra state to keep in sync.

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// fillLookback bounds how far the reconciliation reach goes back on a
// normal tick. Broker history is unlimited; a trading day's worth of orders
// is not the point of a per-tick pass, and an unbounded read would grow the
// tick's cost forever as the account ages.
const fillLookback = 72 * time.Hour

// firstFillBackfillWindow is the order-history span the FIRST pass scans.
//
// It is "everything the broker will return in one page" (zero cutoff), not a
// date arithmetic guess: the fills that were missing on the live account had
// been accepted 47 days earlier, so a 30-day window recovered nothing. The
// pass runs once per start, and the order limit bounds it; later ticks fall
// back to fillLookback.
const firstFillBackfillLimit = 500

// fillScanLimit caps the orders inspected per pass.
const fillScanLimit = 200

// qtyEpsilon absorbs float noise in cumulative quantity comparisons. It is
// far below any tradable size (a crypto fill is meaningful at 1e-8), so a
// residue this small means "already accounted for", never "still owed".
const qtyEpsilon = 1e-9

// reconcileFills journals any broker fill the ledger has not recorded yet.
// It returns the number of fill rows written.
func (l *autoLoop) reconcileFills(ctx context.Context) (int, error) {
	if l.client == nil || l.journal == nil {
		return 0, nil
	}
	limit := fillScanLimit
	var cutoff time.Time // zero cutoff = no age filter (bootstrap pass)
	if l.fillBackfilled {
		cutoff = time.Now().UTC().Add(-fillLookback)
	} else {
		limit = firstFillBackfillLimit
	}
	orders, err := l.client.ListRecentOrders(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("list broker orders: %w", err)
	}
	filled, err := l.journal.FilledByOrder()
	if err != nil {
		return 0, fmt.Errorf("read journalled fills: %w", err)
	}
	scores, err := l.journal.IntentsByClientOrderID()
	if err != nil {
		return 0, fmt.Errorf("read intents: %w", err)
	}

	written := 0
	for _, o := range orders {
		oc, err := l.fillDeltaFor(o, filled, cutoff)
		if err != nil {
			// A single unparsable order must not abort the pass: the
			// other rows are what protects the account.
			log.Printf("[reconcile] %s order %s skipped: %v", o.Symbol, o.ID, err)
			continue
		}
		if oc.delta <= 0 {
			continue
		}
		rec := oc.record
		if sc, ok := scores[strings.TrimSpace(o.ClientOrderID)]; ok {
			score := sc
			rec.Confidence = &score
		}
		if err := l.journal.Append(rec); err != nil {
			log.Printf("[reconcile] fill row for %s not written: %v", o.Symbol, err)
			continue
		}
		written++
		filled[o.ID] += oc.delta // so a second row for the same order is right too
		l.journalReconciled(o, oc.delta)
	}
	// The wide window is spent only by a pass that actually read the broker.
	// A failed lookup returns above, so the bootstrap is retried next tick
	// rather than silently downgraded to the narrow window.
	if !l.fillBackfilled {
		l.fillBackfilled = true
		log.Printf("[reconcile] bootstrap pass scanned %d broker order(s) and recovered %d fill(s)", len(orders), written)
	}
	return written, nil
}

// openFill is one recovered fill delta, ready to journal.
type openFill struct {
	delta  float64
	record pnl.Record
}

// fillDeltaFor computes what this order still owes the journal.
func (l *autoLoop) fillDeltaFor(o tools.Order, filled map[string]float64, cutoff time.Time) (openFill, error) {
	if strings.TrimSpace(o.ID) == "" {
		return openFill{}, nil
	}
	q, err := strconv.ParseFloat(strings.TrimSpace(o.FilledQty), 64)
	if err != nil || q <= 0 {
		return openFill{}, nil // nothing filled: nothing to recover
	}
	// Bracket and OCO CHILD orders are separate ids that carry no fill of
	// their own until they trigger; the parent's group_role marks them, and
	// journalling a 0-qty child would be noise. Children WITH fills are
	// real executions (a stop that fired) and must be recorded.
	created := o.SubmittedAt
	if created.IsZero() {
		created = o.CreatedAt
	}
	if !cutoff.IsZero() && !created.IsZero() && created.Before(cutoff) {
		return openFill{}, nil // outside the lookback: not this pass's job
	}
	price := strings.TrimSpace(o.FilledAvgPrice)
	if price == "" || price == "0" {
		// The broker says it filled but gives no average price. Inventing a
		// price would corrupt P/L, so leave it for the next pass and say so
		// only when it is otherwise invisible.
		if !terminalStatus(o.Status) {
			return openFill{}, nil
		}
		return openFill{}, fmt.Errorf("filled_qty=%s but no filled_avg_price", o.FilledQty)
	}
	pf, err := strconv.ParseFloat(price, 64)
	if err != nil || pf <= 0 || math.IsNaN(pf) || math.IsInf(pf, 0) {
		return openFill{}, fmt.Errorf("unusable average price %q", price)
	}
	side := strings.ToLower(strings.TrimSpace(o.Side))
	if side != "buy" && side != "sell" {
		return openFill{}, fmt.Errorf("unusable side %q", o.Side)
	}
	delta := q - filled[o.ID]
	if delta <= qtyEpsilon {
		return openFill{}, nil // already journalled (this is the idempotence)
	}
	// Never journal more than the broker says exists, even if local state
	// is somehow ahead.
	if delta > q {
		delta = q
	}
	return openFill{
		delta: delta,
		record: pnl.Record{
			Kind:          pnl.KindFill,
			OrderID:       o.ID,
			ClientOrderID: strings.TrimSpace(o.ClientOrderID),
			Symbol:        strings.TrimSpace(o.Symbol),
			Side:          side,
			Qty:           strconv.FormatFloat(delta, 'f', -1, 64),
			Price:         price,
			Status:        o.Status,
		},
	}, nil
}

// terminalStatus reports whether an order can no longer fill.
func terminalStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "filled", "canceled", "expired", "rejected", "failed":
		return true
	}
	return false
}

// journalReconciled writes the audit row for one recovered fill, so the
// dashboard shows that the number arrived from reconciliation rather than
// from a live response — the two must be distinguishable when P/L is being
// argued about.
func (l *autoLoop) journalReconciled(o tools.Order, delta float64) {
	if l.journal == nil {
		return
	}
	_ = l.journal.Append(pnl.Record{
		Kind:   pnl.KindDecision,
		Source: "reconcile:fills",
		Risk:   "APPROVED",
		Symbol: strings.TrimSpace(o.Symbol),
		Side:   strings.ToLower(strings.TrimSpace(o.Side)),
		Detail: fmt.Sprintf("recovered fill order=%s %s %s qty=%s avg=%s status=%s coid=%s",
			o.ID, strings.TrimSpace(o.Symbol), strings.ToLower(o.Side),
			strconv.FormatFloat(delta, 'f', -1, 64), o.FilledAvgPrice, o.Status, o.ClientOrderID),
	})
}
