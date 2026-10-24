package strategy

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// Executor turns EntryPlans into real (paper) orders through the SAME
// deterministic risk gate as every other path (agents.NewValidator), so
// all caps, kill-switch and session rules apply. Crypto entries never
// carry bracket children (unsupported by Alpaca on crypto); equities get
// order_class=bracket with TP/SL children.
type Executor struct {
	Client    *tools.Client
	Validator RiskGate
	Kill      *agents.KillSwitch
	State     *StateStore
	Journal   *pnl.Journal
	Log       *log.Logger
}

// RiskGate is the slice of *risk.Validator the executor consumes;
// *risk.Validator satisfies it directly and tests fake it.
type RiskGate interface {
	Validate(p risk.Proposal) risk.Verdict
}

// strategyNotional returns the USD exposure to hand the risk gate. The
// strategy already knows the dollar value of an entry (qty x reference
// price), so the gate sizes against that instead of re-fetching a price:
// a share count on its own is not a dollar amount and the caps would be
// meaningless.
func strategyNotional(explicit string, computed float64) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return s
	}
	if computed > 0 {
		return strconv.FormatFloat(computed, 'f', 2, 64)
	}
	return ""
}

func (x *Executor) ExecuteEntry(ctx context.Context, p EntryPlan, conf float64) error {
	isNotional := p.Notional > 0
	if isNotional && !p.Crypto {
		return fmt.Errorf("notional orders only supported for crypto (got %s)", p.Symbol)
	}
	// Balance-aware pre-trade guard: before touching the broker, verify
	// the account can actually fund this entry. Crypto on Alpaca requires
	// settled cash (no margin), so a negative/zero cash balance means
	// EVERY crypto buy will 403 — better to skip with a clear reason than
	// fire a guaranteed-fail order every tick. Equities are funded from
	// buying power (margin). This single check covers every entry path
	// (swing, intraday, ensemble, pre-order) because they all flow through
	// here.
	notionalUSD := p.Notional
	if notionalUSD <= 0 && p.Qty > 0 && p.Price > 0 {
		notionalUSD = float64(p.Qty) * p.Price
	}
	if notionalUSD > 0 {
		acct, err := x.Client.GetAccount(ctx)
		if err != nil {
			return fmt.Errorf("balance check: account unavailable: %w", err)
		}
		var avail float64
		if p.Crypto {
			avail = parseFloatSafe(acct.Cash)
			if avail <= 0 {
				return fmt.Errorf("balance check: no available USD for %s (cash=%.2f); skipping buy", p.Symbol, avail)
			}
		} else {
			avail = parseFloatSafe(acct.BuyingPower)
			if avail <= 0 {
				return fmt.Errorf("balance check: no buying power for %s (bp=%.2f); skipping buy", p.Symbol, avail)
			}
		}
		if notionalUSD > avail {
			return fmt.Errorf("balance check: %s buy $%.2f exceeds available $%.2f; skipping", p.Symbol, notionalUSD, avail)
		}
	}
	// Crypto notional orders must be day orders (Alpaca rejects
	// fractional notional with GTC). For qty-based crypto orders, the
	// default GTC is fine.
	// Crypto notional orders: Alpaca paper accepts GTC, not day. Verified
	// empirically (day returns 422 "invalid crypto time_in_force").
	tif := "day"
	if isNotional {
		tif = "gtc"
	}
	req := tools.OrderRequest{
		Symbol:      p.Symbol,
		Side:        "buy",
		Type:        "market",
		TimeInForce: tif,
	}
	if isNotional {
		req.Notional = strconv.FormatFloat(p.Notional, 'f', 2, 64)
	} else {
		req.Qty = strconv.Itoa(p.Qty)
	}
	verdict := x.Validator.Validate(risk.Proposal{
		Symbol: p.Symbol, Side: "buy", Qty: req.Qty,
		Notional:    strategyNotional(req.Notional, notionalUSD),
		Confidence:  &conf,
		OrderType:   req.Type,
		TimeInForce: req.TimeInForce,
	})
	if !verdict.Approved {
		return fmt.Errorf("risk gate rejected %s entry: %s", p.Symbol, strings.Join(verdict.Reasons, "; "))
	}
	// The intent is written BEFORE the order is submitted, and carries the
	// client_order_id the submission will use.
	//
	// Broker acceptance is not a fill, and a lost response is not a
	// failure: an order can be accepted and filled after the HTTP call
	// times out or the process dies. Without a durable intent the only
	// record of that trade is a position the bot cannot explain — no fill
	// row, so the drawdown halts see zero realized P/L and the learner has
	// no evidence. With one, the reconciliation pass can match the broker's
	// order back to this decision, including the score that justified it.
	score := entryScoreOf(p.Score, conf)
	req.ClientOrderID = x.newIntentID(p, score)
	x.journalIntent(req, p.Price, score)
	o, err := x.placeWithBrackets(ctx, req, p)
	if err != nil {
		// A failed POST is not proof the order was not accepted. Ask the
		// broker by the id we already journalled: if it is there, this was a
		// lost response, not a lost order — and treating it as failure would
		// invite a retry that opens a second position.
		if found, ok := x.resolveSubmission(ctx, req.ClientOrderID, err); ok {
			x.log().Printf("[strategy] %s: submission outcome resolved to order %s (status %s) — not retrying",
				p.Symbol, found.ID, found.Status)
			_ = x.journalAudit("strategy:resolved", p.Symbol, "buy",
				fmt.Sprintf("submission error resolved by client_order_id %s to order %s (status %s): %v",
					req.ClientOrderID, found.ID, found.Status, err))
			o = found
		} else {
			return fmt.Errorf("place %s entry: %w", p.Symbol, err)
		}
	}
	x.log().Printf("[strategy] ENTRY %s qty=%s notional=%.2f @~%.4f budget=%.2f crypto=%v tp=%.4f sl=%.4f order=%s",
		p.Symbol, req.Qty, p.Notional, p.Price, p.Budget, p.Crypto, p.Brackets.TakeProfit, p.Brackets.StopLoss, o.ID)
	if x.Journal != nil {
		rec := pnl.Record{
			Kind: pnl.KindDecision, Source: "strategy:auto", Risk: "APPROVED",
			ClientOrderID: req.ClientOrderID,
			Detail: fmt.Sprintf("entry %s qty=%s notional=%.2f price=%.4f tp=%.4f sl=%.4f crypto=%t order=%s",
				p.Symbol, req.Qty, p.Notional, p.Price, p.Brackets.TakeProfit, p.Brackets.StopLoss, p.Crypto, o.ID),
		}
		if score > 0 {
			sc := score
			rec.Confidence = &sc
		}
		_ = x.Journal.Append(rec)
		// Optimistic path: the response already carries the fill. When it
		// does not (accepted, or partially filled), reconciliation recovers
		// the rest — journalFill is deliberately not the only way a fill
		// reaches the ledger.
		x.journalFill(o, score)
	}
	if p.Crypto {
		if err := x.State.SetLevel(PositionLevels{
			Symbol: p.Symbol, EntryPrice: p.Price,
			TakeProfit: p.Brackets.TakeProfit, StopLoss: p.Brackets.StopLoss,
			Qty: req.Qty, Since: time.Now().UTC(),
		}); err != nil {
			x.log().Printf("[strategy] WARNING: %s filled but state persist failed: %v", p.Symbol, err)
		}
	}
	return nil
}

// entryScoreOf picks the number to journal as the decision's score. The
// plan's own composite is the honest answer; the gate confidence is the
// fallback for callers that have no factor score (an LLM proposal, whose
// confidence IS its score). Zero when neither exists, so an unattributed
// fill stays unattributed instead of silently counting as a 1.00 entry.
func entryScoreOf(planScore, conf float64) float64 {
	if planScore > 0 {
		return planScore
	}
	if conf > 0 && conf < 1 {
		return conf
	}
	return 0
}

// journalFill records a filled order in the trade log. The auto loop
// placed orders but never wrote fill records, so the drawdown halts saw
// a realized P/L of zero forever and the stats ledger never populated.
//
// entryScore rides on the record as its confidence. Without it the
// ledger knows a trade won or lost but not what number the bot acted
// on, so no score -> outcome relationship can ever be measured and the
// learner has nothing to calibrate against.
func (x *Executor) journalFill(o *tools.Order, entryScore float64) {
	if x.Journal == nil || o == nil {
		return
	}
	if o.FilledQty == "" || o.FilledQty == "0" {
		return
	}
	price := strings.TrimSpace(o.FilledAvgPrice)
	if price == "" || price == "0" {
		return
	}
	rec := pnl.Record{
		Kind:          pnl.KindFill,
		OrderID:       o.ID,
		ClientOrderID: o.ClientOrderID,
		Symbol:        o.Symbol,
		Side:          strings.ToLower(o.Side),
		Qty:           o.FilledQty,
		Price:         price,
		Status:        o.Status,
		TS:            time.Now().UTC(),
	}
	if entryScore > 0 {
		sc := entryScore
		rec.Confidence = &sc
	}
	_ = x.Journal.Append(rec)
}

// newIntentID returns the client_order_id for one intended submission.
//
// A plan that carries its own IntentID keeps it: the A2A path keys the
// intent to the peer's signal id, so a redelivery after a crash is refused
// by the broker as a duplicate instead of filling twice.
//
// Otherwise the id is unique per intent, not derived from the order's
// content: two identical entries on the same day are legitimate (the crypto
// book scales in), and a content hash would have the broker reject the
// second one. Stability across a single attempt is what matters — the id is
// journalled before the POST, so a lost response is still traceable, and
// Alpaca enforces uniqueness (measured: a reused id returns HTTP 422 code
// 42210000), so the same intent can never be submitted twice.
func (x *Executor) newIntentID(p EntryPlan, score float64) string {
	if id := strings.TrimSpace(p.IntentID); id != "" {
		return id
	}
	const kind = "entry"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A clock fallback is still unique enough in practice: it is
		// combined with the symbol, and the broker will refuse a collision
		// rather than double-fill.
		return fmt.Sprintf("alp-%s-%s-%d", kind, sanitizeForID(p.Symbol), time.Now().UnixNano())
	}
	return fmt.Sprintf("alp-%s-%s-%d-%x", kind, sanitizeForID(p.Symbol), time.Now().Unix(), b)
}

// sanitizeForID makes a symbol safe to embed in a client_order_id
// (Alpaca allows alphanumerics, dash and underscore, up to 128 chars).
func sanitizeForID(s string) string {
	var out strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out.WriteRune(r)
		default:
			out.WriteRune('-')
		}
	}
	id := out.String()
	if len(id) > 32 {
		id = id[:32]
	}
	return id
}

// journalIntent writes the durable record of an intended submission.
func (x *Executor) journalIntent(req tools.OrderRequest, markPrice, score float64) {
	if x.Journal == nil {
		return
	}
	rec := pnl.Record{
		Kind:          pnl.KindIntent,
		Source:        "strategy:auto",
		ClientOrderID: req.ClientOrderID,
		Symbol:        req.Symbol,
		Side:          strings.ToLower(req.Side),
		Qty:           req.Qty,
		Price:         strconv.FormatFloat(markPrice, 'f', 4, 64),
		Detail:        fmt.Sprintf("intent %s %s qty=%s notional=%s @~%.4f", req.Side, req.Symbol, req.Qty, req.Notional, markPrice),
	}
	if score > 0 {
		sc := score
		rec.Confidence = &sc
	}
	_ = x.Journal.Append(rec)
}

// ExecuteReduce places ONE risk-reducing order: the whole position (or the
// whole short) in a symbol, proven against a fresh broker read.
//
// This is the exit authority. Every path that wants a position gone — the
// signal-driven sell, the max-hold close, the profit-target rebalancer,
// the intraday flatten — calls here instead of assembling an order by hand,
// because the four things this function does in order are exactly what the
// hand-built versions kept getting wrong:
//
//  1. the quantity comes from a FRESH position read, never from a cached
//     tick snapshot or a stored level (a stale number here is an oversold
//     account and an unintended short);
//  2. resting protection is cancelled BEFORE the exit, because a surviving
//     bracket or OCO leg fires against a flat account and reopens exposure
//     on the other side;
//  3. the order is validated by the same gate as an entry, but asserted
//     Reducing — which proves it cannot oversell and exempts it from the
//     policies that exist to stop new risk, not to stop risk leaving;
//  4. the client_order_id is deterministic on (symbol, side, qty, UTC day),
//     so a retry after a lost response is refused by the broker as a
//     duplicate instead of becoming a second sell.
func (x *Executor) ExecuteReduce(ctx context.Context, symbol, reason string) (bool, error) {
	if x.Client == nil {
		return false, fmt.Errorf("no broker client wired")
	}
	positions, err := x.Client.GetPositions(ctx)
	if err != nil {
		return false, fmt.Errorf("live positions: %w", err)
	}
	var held float64
	var mark float64
	var found bool
	for _, p := range positions {
		if !tools.SameSymbol(p.Symbol, symbol) {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(p.Qty), 64)
		if err != nil {
			return false, fmt.Errorf("unreadable position qty for %s: %q", symbol, p.Qty)
		}
		if q == 0 {
			continue
		}
		held, found = q, true
		mark, _ = strconv.ParseFloat(strings.TrimSpace(p.CurrentPrice), 64)
		symbol = p.Symbol // trade the broker's own symbol form
		break
	}
	if !found {
		// Nothing to close. Clearing the level is the useful action: a
		// stored level with no position behind it is what makes the monitor
		// chase a ghost every tick.
		if x.State != nil {
			_ = x.State.ClearLevel(symbol)
		}
		return false, nil
	}
	side := "sell"
	if held < 0 {
		side = "buy"
	}
	qty := math.Abs(held)
	qtyStr := strconv.FormatFloat(qty, 'f', -1, 64)

	// Protection first. A failure here is logged and carried on past: the
	// exit is the risk-reducing action, and the reconcile pass re-places
	// protection for any position left exposed by a failed close.
	if n, err := x.Client.CancelOrdersForSymbol(ctx, symbol); err != nil {
		x.log().Printf("[exit] %s: resting protection could not be cleared (%v); exiting anyway — reconcile will re-protect if this close fails", symbol, err)
		_ = x.journalAudit("exit:protect", symbol, side, fmt.Sprintf(
			"protection not cancelled before %s close (%v); reconcile pass will re-derive", reason, err))
	} else if n > 0 {
		x.log().Printf("[exit] %s: cleared %d resting protective order(s) before the close", symbol, n)
		_ = x.journalAudit("exit:protect", symbol, side, fmt.Sprintf(
			"cancelled %d resting protective order(s) before close (%s)", n, reason))
	}

	tif := "gtc"
	if tools.IsOCCSymbol(symbol) {
		tif = "day" // options reject other values
	}
	req := tools.OrderRequest{
		Symbol: symbol, Side: side, Type: "market", TimeInForce: tif, Qty: qtyStr,
		ClientOrderID: exitIntentID(symbol, side, qtyStr, time.Now().UTC().Format("20060102")),
	}
	verdict := x.Validator.Validate(risk.Proposal{
		Symbol: symbol, Side: side, Qty: qtyStr,
		OrderType: req.Type, TimeInForce: req.TimeInForce,
		Reducing: true, // proven, not asserted: see risk.Validator.Holdings
	})
	if !verdict.Approved {
		return false, fmt.Errorf("risk gate refused the %s exit: %s", symbol, strings.Join(verdict.Reasons, "; "))
	}
	x.journalIntent(req, mark, 0)

	o, err := x.Client.PlaceOrder(ctx, req)
	if err != nil {
		if found, ok := x.resolveSubmission(ctx, req.ClientOrderID, err); ok {
			// The broker already holds this exact intent: either it rejected
			// the id as a duplicate, or the order is there under the id we
			// journalled. Reporting failure would invite a retry with a NEW
			// id, which is how a double-sell and an unintended short are born.
			x.log().Printf("[exit] %s: intent %s already at the broker (order %s, status %s); awaiting reconciliation",
				symbol, req.ClientOrderID, found.ID, found.Status)
			_ = x.journalAudit("exit:idempotent", symbol, side, fmt.Sprintf(
				"submission resolved to existing order %s (status %s) — the close is already submitted", found.ID, found.Status))
			return true, nil
		}
		return false, fmt.Errorf("place %s exit: %w", symbol, err)
	}
	x.log().Printf("[exit] %s %s qty=%s order=%s (%s)", symbol, side, qtyStr, o.ID, reason)
	_ = x.journalAudit("exit:approved", symbol, side, fmt.Sprintf(
		"%s qty=%s order=%s reason=%q", symbol, qtyStr, o.ID, reason))
	if x.State != nil {
		_ = x.State.ClearLevel(symbol)
	}
	x.journalFill(o, 0) // the synchronous fast path; reconciliation owns the late half
	return true, nil
}

// exitIntentID is deterministic on (symbol, side, qty, UTC day) so a repeat
// of the same close dedupes at the broker rather than becoming a second
// sell. The day is a parameter rather than a hidden clock read: the whole
// value of the id is its stability, and a test cannot assert stability
// against a clock it cannot move.
func exitIntentID(symbol, side, qty, day string) string {
	return fmt.Sprintf("alp-exit-%s-%s-%s-%s", sanitizeForID(symbol), side, sanitizeForID(qty), day)
}

// resolveSubmission decides whether a failed order POST was actually a
// submission that succeeded and lost its response.
//
// Both signals are consulted: the broker's duplicate-id rejection (which
// says "this id already exists") and a lookup by that id (which says "here
// it is"). Anything else — no id on the request, no match at the broker — is
// a real failure, and the caller should NOT retry blindly.
func (x *Executor) resolveSubmission(ctx context.Context, clientOrderID string, postErr error) (*tools.Order, bool) {
	if x.Client == nil || strings.TrimSpace(clientOrderID) == "" {
		return nil, false
	}
	o, err := x.Client.FindOrderByClientOrderID(ctx, clientOrderID)
	if err != nil || o == nil {
		return nil, false
	}
	if tools.IsDuplicateClientOrderID(postErr) {
		return o, true
	}
	// A submitted order exists under our id. The POST error may have been a
	// timeout after the broker had already accepted it.
	return o, true
}

// journalAudit writes one exit-path decision row under the given source.
func (x *Executor) journalAudit(source, symbol, side, detail string) error {
	if x.Journal == nil {
		return nil
	}
	return x.Journal.Append(pnl.Record{
		Kind: pnl.KindDecision, Source: source, Risk: "APPROVED",
		Symbol: symbol, Side: side, Detail: detail,
	})
}

// ExecuteOptionEntry places a deep-ITM call entry through the SAME risk
// gate + balance guard as equity entries. The validator prices OCC
// proposals with the 100-share multiplier, so MAX_POSITION_USD caps the
// premium outlay; the balance check confirms the account can fund it
// before the order goes out. Previously the overlay called PlaceOrder
// directly — bypassing the risk gate AND the balance check entirely.
func (x *Executor) ExecuteOptionEntry(ctx context.Context, leg *OptionLeg, conf float64) error {
	if leg == nil || leg.Contracts < 1 {
		return fmt.Errorf("option leg needs at least one contract, got %+v", leg)
	}
	// Balance guard first: contracts x per-contract premium is the cash
	// the account must have. Options are cash-settled against buying
	// power on Alpaca; a shortfall is a guaranteed broker reject — better
	// to skip with a clear reason than fire a doomed order every tick.
	outlay := float64(leg.Contracts) * leg.Premium
	acct, err := x.Client.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("balance check: account unavailable: %w", err)
	}
	avail := parseFloatSafe(acct.BuyingPower)
	if outlay > avail {
		return fmt.Errorf("balance check: %s premium outlay $%.2f exceeds buying power $%.2f; skipping", leg.OCC, outlay, avail)
	}
	req := tools.OrderRequest{
		Symbol:      leg.OCC,
		Qty:         strconv.Itoa(leg.Contracts),
		Side:        "buy",
		Type:        "market",
		TimeInForce: "day", // options require day|gtc
	}
	// Full deterministic risk gate — same validator, same caps, same
	// kill-switch. The gate's OCC branch prices via its own PriceSource
	// (quote mid x 100), which double-checks our outlay estimate.
	verdict := x.Validator.Validate(risk.Proposal{
		Symbol: leg.OCC, Side: "buy", Qty: req.Qty,
		Confidence: &conf, OrderType: req.Type, TimeInForce: req.TimeInForce,
	})
	if !verdict.Approved {
		return fmt.Errorf("risk gate rejected %s option entry: %s", leg.OCC, strings.Join(verdict.Reasons, "; "))
	}
	o, err := x.Client.PlaceOrder(ctx, req)
	if err != nil {
		if found, ok := x.resolveSubmission(ctx, req.ClientOrderID, err); ok {
			x.log().Printf("[strategy] %s: option submission resolved to order %s (status %s)", leg.OCC, found.ID, found.Status)
			o = found
		} else {
			return fmt.Errorf("place %s option entry: %w", leg.OCC, err)
		}
	}
	x.log().Printf("[strategy] OPTION ENTRY %s x%d premium=%.2f/contract outlay=%.2f order=%s",
		leg.OCC, leg.Contracts, leg.Premium, outlay, o.ID)
	if x.Journal != nil {
		score := entryScoreOf(leg.Score, conf)
		rec := pnl.Record{
			Kind: pnl.KindDecision, Source: "strategy:auto", Risk: "APPROVED",
			Detail: fmt.Sprintf("option-entry %s x%d premium=%.2f outlay=%.2f order=%s",
				leg.OCC, leg.Contracts, leg.Premium, outlay, o.ID),
		}
		if score > 0 {
			sc := score
			rec.Confidence = &sc
		}
		_ = x.Journal.Append(rec)
		// The option fill belongs in the ledger too: an option position is
		// a trade with an outcome, and excluding it would teach the
		// learner only about the classes it happened to record.
		x.journalFill(o, score)
	}
	return nil
}

// placeWithBrackets issues the order request. Equities get the bracket
// envelope merged into the JSON body via a typed extension of
// OrderRequest — Alpaca accepts unknown-order fields only through the
// raw JSON, so we marshal manually with the extra keys.
func (x *Executor) placeWithBrackets(ctx context.Context, req tools.OrderRequest, p EntryPlan) (*tools.Order, error) {
	if p.Crypto {
		return x.Client.PlaceOrder(ctx, req)
	}
	// order_class=bracket is EQUITIES ONLY (Alpaca rejects it on crypto
	// orders). Build the full JSON body including the bracket envelope;
	// tools.OrderRequest has no class field, so extend at the wire level.
	body := map[string]any{
		"symbol":        req.Symbol,
		"qty":           req.Qty,
		"side":          req.Side,
		"type":          req.Type,
		"time_in_force": req.TimeInForce,
		"order_class":   "bracket",
		"take_profit":   map[string]string{"limit_price": fmt.Sprintf("%.4f", p.Brackets.TakeProfit)},
		"stop_loss":     map[string]string{"stop_price": fmt.Sprintf("%.4f", p.Brackets.StopLoss)},
	}
	if req.LimitPrice != "" {
		body["limit_price"] = req.LimitPrice
	}
	if req.ClientOrderID != "" {
		body["client_order_id"] = req.ClientOrderID
	}
	var out tools.Order
	endpoint := x.Client.BaseURL + "/orders"
	if err := x.Client.Do(ctx, "POST", endpoint, nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExecutePreOrder queues an equity entry as a resting limit/GTC bracket
// while the market is closed (PRE_ORDERS mode). The entry limit is the
// last daily close; TP/SL children ride the same bracket envelope and
// only activate once the parent fills at/after the next open. The risk
// validator's session gate must have PRE_ORDERS enabled or this rejects.
//
// Balance-aware: the resting order consumes buying power the moment the
// market opens, so the account must be able to fund it NOW. Same guard
// as ExecuteEntry — cash-funded equities checked against buying power,
// fail closed when the account is unreadable.
func (x *Executor) ExecutePreOrder(ctx context.Context, p EntryPlan, conf float64) error {
	notionalUSD := float64(p.Qty) * p.Price
	if notionalUSD > 0 {
		acct, err := x.Client.GetAccount(ctx)
		if err != nil {
			return fmt.Errorf("balance check: account unavailable: %w", err)
		}
		avail := parseFloatSafe(acct.BuyingPower)
		if avail <= 0 {
			return fmt.Errorf("balance check: no buying power for %s (bp=%.2f); skipping pre-order", p.Symbol, avail)
		}
		if notionalUSD > avail {
			return fmt.Errorf("balance check: %s pre-order $%.2f exceeds available $%.2f; skipping", p.Symbol, notionalUSD, avail)
		}
	}
	req := tools.OrderRequest{
		Symbol:      p.Symbol,
		Side:        "buy",
		Type:        "limit",
		TimeInForce: "gtc",
		Qty:         strconv.Itoa(p.Qty),
		LimitPrice:  fmt.Sprintf("%.4f", p.Price),
	}
	verdict := x.Validator.Validate(risk.Proposal{
		Symbol: p.Symbol, Side: "buy", Qty: req.Qty,
		Notional:    strategyNotional("", notionalUSD),
		Confidence:  &conf,
		OrderType:   req.Type,
		TimeInForce: req.TimeInForce,
	})
	if !verdict.Approved {
		return fmt.Errorf("risk gate rejected %s pre-order: %s", p.Symbol, strings.Join(verdict.Reasons, "; "))
	}
	o, err := x.placeWithBrackets(ctx, req, p)
	if err != nil {
		if found, ok := x.resolveSubmission(ctx, req.ClientOrderID, err); ok {
			x.log().Printf("[strategy] %s: pre-order submission resolved to order %s (status %s)", p.Symbol, found.ID, found.Status)
			o = found
		} else {
			return fmt.Errorf("place %s pre-order: %w", p.Symbol, err)
		}
	}
	x.log().Printf("[strategy] PRE-ORDER %s qty=%d @limit %.4f budget=%.2f tp=%.4f sl=%.4f order=%s (resting until open)",
		p.Symbol, p.Qty, p.Price, p.Budget, p.Brackets.TakeProfit, p.Brackets.StopLoss, o.ID)
	if x.Journal != nil {
		rec := pnl.Record{
			Kind: pnl.KindDecision, Source: "strategy:auto", Risk: "APPROVED",
			Detail: fmt.Sprintf("pre-order %s qty=%d limit=%.4f tp=%.4f sl=%.4f order=%s",
				p.Symbol, p.Qty, p.Price, p.Brackets.TakeProfit, p.Brackets.StopLoss, o.ID),
		}
		if score := entryScoreOf(p.Score, conf); score > 0 {
			sc := score
			rec.Confidence = &sc
		}
		_ = x.Journal.Append(rec)
	}
	return nil
}

func (x *Executor) log() *log.Logger {
	if x.Log == nil {
		return log.Default()
	}
	return x.Log
}

// parseFloatSafe parses a numeric string from Alpaca (which returns
// prices/balances as strings), returning 0 for empty/invalid input so a
// missing balance is treated as "nothing available" — fail closed.
func parseFloatSafe(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}
