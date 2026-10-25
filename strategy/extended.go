package strategy

// extended.go: trading the rest of the day.
//
// The US equity session Alpaca offers outside regular hours is not the
// session the strategy was calibrated in, and the broker will not let you
// bring your protection with you. Measured against the paper API on
// 2026-10-25:
//
//	extended_hours + order_class=bracket -> HTTP 403 40310000
//	    "bracket orders do not support extended hours trading"
//	extended_hours + order_class=oco     -> HTTP 403 40310000
//	    "oco orders do not support extended hours trading"
//	extended_hours + type=market         -> HTTP 422 42210000
//	    "extended hours order must be DAY or GTC limit orders"
//	extended_hours + type=stop_limit     -> HTTP 422 42210000
//	    "stop_limit orders are not eligible for extended hours trading"
//	extended_hours + limit + day|gtc     -> accepted; the response echoes
//	    extended_hours=true
//
// So an extended-hours position is entered with a marketable limit and has no
// broker-side stop while it sits there. Two consequences drive everything
// below. First, entry needs a live quote, not a stale print: a snapshot keeps
// serving its last one after the session ends (measured on a Sunday, AAPL's
// "spread" was Friday's 20:00:03Z close and 10% wide), so price and age are
// one condition. Second, the exit has to be a limit too — a market exit is
// refused, and a stop is refused — so the bot crosses the spread itself
// instead of asking the exchange to trigger on its behalf.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// extendedCrossBufferBps is how far through the touch a marketable limit
// reaches. Crossing exactly to the ask leaves the order resting the instant
// the ask moves by a cent, which in a thin book is the difference between an
// entry and a hanging order that fills hours later at a price nobody chose.
// Half a basis point of buffer buys immediacy without paying a full tick.
const extendedCrossBufferBps = 5.0

// extendedMaxPriceDeviation bounds how far the live extended-session mark may
// sit from the price the plan was sized at. The plan's mark and this quote are
// read seconds apart, so a large gap is not a move to chase — it is evidence
// that one of the two reads is not describing the same market.
const extendedMaxPriceDeviation = 0.01

// extendedQuoteSkewTolerance is how far ahead of this host's clock a feed
// stamp may sit and still be believed. A second or two is ordinary skew; a
// minute is a broken stamp, and treating a future print as "0 seconds old"
// would turn a bad timestamp into a licence to trade.
const extendedQuoteSkewTolerance = 15 * time.Second

// ExtendedQuote is a vetted extended-session mark: the touch, and the time it
// was printed. A quote without its age is a price without evidence.
type ExtendedQuote struct {
	Symbol string
	Bid    float64
	Ask    float64
	At     time.Time
}

// Mid is the touch midpoint — the fair value a two-sided market implies.
func (q ExtendedQuote) Mid() float64 { return (q.Bid + q.Ask) / 2 }

// SpreadBps is the quoted cost of trading through the touch right now.
func (q ExtendedQuote) SpreadBps() float64 {
	if q.Bid <= 0 || q.Ask <= 0 || q.Ask < q.Bid {
		return 0
	}
	return (q.Ask - q.Bid) / q.Mid() * 10000
}

// Valid reports whether the quote is two-sided and uncrossed.
func (q ExtendedQuote) Valid() bool {
	return q.Bid > 0 && q.Ask > 0 && q.Ask >= q.Bid && !q.At.IsZero()
}

// ExtendedMark reads the live touch for an equity symbol and refuses
// anything it would not act on: a missing or crossed market, and any print
// older than maxAge. It is deliberately not a "best effort" price: outside
// the regular session the last print can be hours or days old, and sizing an
// entry off it is the one failure mode the whole feature has to survive.
func (x *Executor) ExtendedMark(ctx context.Context, symbol string, maxAge time.Duration) (ExtendedQuote, error) {
	if x.Client == nil {
		return ExtendedQuote{}, fmt.Errorf("no broker client wired")
	}
	if IsCrypto(symbol) {
		return ExtendedQuote{}, fmt.Errorf("%s is crypto: it trades 24/7 and has no extended session", symbol)
	}
	if tools.IsOCCSymbol(symbol) {
		return ExtendedQuote{}, fmt.Errorf("%s is an option: Alpaca refuses extended_hours orders on options", symbol)
	}
	m, err := x.Client.GetSnapshots(ctx, []string{symbol})
	if err != nil {
		return ExtendedQuote{}, fmt.Errorf("extended-session quote %s: %w", symbol, err)
	}
	snap, ok := m[symbol]
	if !ok {
		return ExtendedQuote{}, fmt.Errorf("extended-session quote %s: no snapshot returned", symbol)
	}
	q := ExtendedQuote{
		Symbol: symbol,
		Bid:    snap.LatestQuoteBid,
		Ask:    snap.LatestQuoteAsk,
		At:     time.Unix(snap.QuoteTS, 0).UTC(),
	}
	if snap.QuoteTS <= 0 || !q.Valid() {
		return ExtendedQuote{}, fmt.Errorf("extended-session quote %s: no usable two-sided print (bid=%.4f ask=%.4f ts=%d)",
			symbol, q.Bid, q.Ask, snap.QuoteTS)
	}
	// A print from the future is not a fresh quote. Clock skew of a second or
	// two between this host and the feed is normal; anything larger means the
	// stamp is not describing this moment, and an age comparison against a
	// negative number would otherwise wave it through as brand new.
	age := time.Since(q.At)
	if age < -extendedQuoteSkewTolerance {
		return ExtendedQuote{}, fmt.Errorf("extended-session quote %s is stamped %s in the future — not a live print",
			symbol, (-age).Round(time.Second))
	}
	if age < 0 {
		age = 0
	}
	if age > maxAge {
		return ExtendedQuote{}, fmt.Errorf("extended-session quote %s is %s old, past the %s limit",
			symbol, age.Round(time.Second), maxAge)
	}
	return q, nil
}

// crossPrice returns the marketable limit price for one side: a buy reaches
// through the ask, a sell through the bid.
func crossPrice(q ExtendedQuote, side string) (float64, error) {
	buffer := q.Mid() * extendedCrossBufferBps / 10000
	switch strings.ToLower(side) {
	case "buy":
		return q.Ask + buffer, nil
	case "sell":
		price := q.Bid - buffer
		if price <= 0 {
			return 0, fmt.Errorf("%s: crossing the bid leaves a non-positive limit (bid=%.4f)", q.Symbol, q.Bid)
		}
		return price, nil
	}
	return 0, fmt.Errorf("invalid side %q", side)
}

// ExecuteExtendedEntry opens a position in the pre-market or after-hours
// session with a marketable limit/day carrying extended_hours=true.
//
// It does NOT build a bracket: the broker refuses the pair outright, and an
// order that is accepted without its protection is worse than one that is
// refused, because the position now exists and nothing is watching it except
// this process. Protection is the monitor's job on the next pass — the OCO
// legs it places are regular-session orders that rest and become live at the
// bell — and until then the exit path below is the only stop.
//
// The quote is a parameter, not re-fetched inside: the caller has already
// measured the spread against the liquidity cap, and paying a price from a
// second read would make that measurement a description of a different
// moment than the trade.
func (x *Executor) ExecuteExtendedEntry(ctx context.Context, p EntryPlan, q ExtendedQuote, conf float64) error {
	if !q.Valid() {
		return fmt.Errorf("extended entry %s: quote is not two-sided (bid=%.4f ask=%.4f)", p.Symbol, q.Bid, q.Ask)
	}
	if p.Crypto || IsCrypto(p.Symbol) {
		return fmt.Errorf("extended entry %s: crypto has no extended session", p.Symbol)
	}
	if tools.IsOCCSymbol(p.Symbol) {
		return fmt.Errorf("extended entry %s: options do not trade extended hours", p.Symbol)
	}
	limit, err := crossPrice(q, "buy")
	if err != nil {
		return fmt.Errorf("extended entry %s: %w", p.Symbol, err)
	}
	// The plan was sized at its own mark. If the session has moved more than
	// a percent away, the plan no longer describes this market and the entry
	// is not the one the factors approved.
	if p.Price > 0 && math.Abs(limit-p.Price)/p.Price > extendedMaxPriceDeviation {
		return fmt.Errorf("extended entry %s: marketable limit %.4f is more than %.0f%% from the priced mark %.4f — refusing",
			p.Symbol, limit, extendedMaxPriceDeviation*100, p.Price)
	}
	// Re-size against the price actually being paid, so the dollar budget the
	// gate approved is the dollar budget spent. A plan sized at 100 shares of
	// a $10 mark is not 100 shares at $11.
	qty := p.Qty
	if p.Budget > 0 {
		sized := int(math.Floor(p.Budget / limit))
		if sized != qty {
			qty = sized
		}
	}
	if qty < 1 {
		return fmt.Errorf("extended entry %s: budget %.2f cannot buy a whole share at %.4f", p.Symbol, p.Budget, limit)
	}
	notional := float64(qty) * limit
	acct, err := x.Client.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("balance check: account unavailable: %w", err)
	}
	if avail := parseFloatSafe(acct.BuyingPower); notional > avail {
		return fmt.Errorf("balance check: %s extended entry $%.2f exceeds buying power $%.2f", p.Symbol, notional, avail)
	}

	req := tools.OrderRequest{
		Symbol:        p.Symbol,
		Side:          "buy",
		Type:          "limit",
		TimeInForce:   "day",
		LimitPrice:    strconv.FormatFloat(limit, 'f', 4, 64),
		Qty:           strconv.Itoa(qty),
		ExtendedHours: true,
	}
	verdict := x.Validator.Validate(risk.Proposal{
		Symbol: p.Symbol, Side: "buy", Qty: req.Qty,
		Notional:      strategyNotional("", notional),
		Confidence:    &conf,
		OrderType:     req.Type,
		TimeInForce:   req.TimeInForce,
		OrderClass:    "simple",
		ExtendedHours: req.ExtendedHours,
	})
	if !verdict.Approved {
		return fmt.Errorf("risk gate rejected %s extended entry: %s", p.Symbol, strings.Join(verdict.Reasons, "; "))
	}

	score := entryScoreOf(p.Score, conf)
	req.ClientOrderID = x.newIntentID(p, score)
	x.journalIntent(req, limit, score)
	o, err := x.Client.PlaceOrder(ctx, req)
	if err != nil {
		if found, ok := x.resolveSubmission(ctx, req.ClientOrderID, err); ok {
			x.log().Printf("[strategy] %s: extended submission resolved to order %s (status %s) — not retrying",
				p.Symbol, found.ID, found.Status)
			o = found
		} else {
			return fmt.Errorf("place %s extended entry: %w", p.Symbol, err)
		}
	}
	x.log().Printf("[strategy] EXTENDED ENTRY %s qty=%s limit=%s @bid=%.4f @ask=%.4f quote_age=%s tp=%.4f sl=%.4f order=%s",
		p.Symbol, req.Qty, req.LimitPrice, q.Bid, q.Ask, time.Since(q.At).Round(time.Second),
		p.Brackets.TakeProfit, p.Brackets.StopLoss, o.ID)
	if x.Journal != nil {
		rec := pnl.Record{
			Kind: pnl.KindDecision, Source: "strategy:auto-extended", Risk: "APPROVED",
			ClientOrderID: req.ClientOrderID,
			Detail: fmt.Sprintf("extended-entry %s qty=%s limit=%s bid=%.4f ask=%.4f spread_bps=%.1f quote_at=%s tp=%.4f sl=%.4f order=%s",
				p.Symbol, req.Qty, req.LimitPrice, q.Bid, q.Ask, q.SpreadBps(),
				q.At.UTC().Format(time.RFC3339), p.Brackets.TakeProfit, p.Brackets.StopLoss, o.ID),
		}
		if score > 0 {
			sc := score
			rec.Confidence = &sc
		}
		_ = x.Journal.Append(rec)
		x.journalFill(o, score)
	}
	// Record the levels the strategy chose. The monitor's protect pass reads
	// them, sizes the pair off the LIVE position, and rests the regular-session
	// OCO legs — the same path that adopts a position the bot did not open.
	if x.State != nil && p.Brackets.TakeProfit > 0 && p.Brackets.StopLoss > 0 {
		if err := x.State.SetLevel(PositionLevels{
			Symbol: p.Symbol, EntryPrice: limit,
			TakeProfit: p.Brackets.TakeProfit, StopLoss: p.Brackets.StopLoss,
			Qty: req.Qty, Since: time.Now().UTC(),
		}); err != nil {
			x.log().Printf("[strategy] WARNING: %s extended entry placed but level persist failed: %v", p.Symbol, err)
		}
	}
	return nil
}

// ExtendedExit decides how a risk-reducing order must be expressed for the
// session it is being placed in. The auto loop wires it when EXTENDED_HOURS
// is on; without it every exit stays a market order, which is correct for the
// regular session and refused everywhere else.
type ExtendedExit struct {
	// Sessions resolves the current session from the broker's calendar.
	Sessions *tools.Sessions
	// MaxQuoteAge bounds the staleness of the exit print, same reasoning as
	// an entry: outside the session the snapshot serves its last trade.
	MaxQuoteAge time.Duration
}

// request shapes one exit order. It returns extended=false when the exit must
// stay a plain market order: the regular session (market works and is
// immediate), or no session at all (nothing can execute, and the position's
// resting protection is what carries it to the next bell — see below).
//
// Between sessions the answer is deliberately NOT "rest a market order until
// the open". ExecuteReduce cancels the position's protection before it
// exits, so a resting sell means the pair is gone and the position faces the
// open with a market order and no floor. Refusing leaves the protection in
// place and lets the breach re-trigger inside a session that can act.
func (e *ExtendedExit) request(ctx context.Context, x *Executor, symbol, side, qty string) (tools.OrderRequest, bool, error) {
	tif := "gtc"
	if tools.IsOCCSymbol(symbol) {
		tif = "day" // options reject every other value
	}
	out := tools.OrderRequest{
		Symbol: symbol, Side: side, Type: "market", TimeInForce: tif, Qty: qty,
	}
	if e == nil || e.Sessions == nil || IsCrypto(symbol) || tools.IsOCCSymbol(symbol) {
		return out, false, nil
	}
	sess, _, err := e.Sessions.Current(ctx)
	if err != nil {
		// Unknown session keeps the regular-session shape. The risk gate
		// still refuses an order it cannot place, so this is not a bypass:
		// it is the difference between "we could not read the calendar" and
		// "the calendar says nothing can execute".
		x.log().Printf("[exit] %s: session unknown (%v); using the regular-session order shape", symbol, err)
		return out, false, nil
	}
	if !sess.Extended() {
		return out, false, nil
	}
	q, err := x.ExtendedMark(ctx, symbol, e.MaxQuoteAge)
	if err != nil {
		return tools.OrderRequest{}, false, fmt.Errorf("extended exit %s: %w", symbol, err)
	}
	price, err := crossPrice(q, side)
	if err != nil {
		return tools.OrderRequest{}, false, fmt.Errorf("extended exit %s: %w", symbol, err)
	}
	return tools.OrderRequest{
		Symbol: symbol, Side: side, Type: "limit", TimeInForce: "day",
		LimitPrice: strconv.FormatFloat(price, 'f', 4, 64), Qty: qty,
		ExtendedHours: true,
	}, true, nil
}

// exitShape is the executor's view of the same decision: the order request a
// reducing order should use, and whether it is an extended-hours order (which
// the risk gate then checks for shape rather than assuming a market order).
func (x *Executor) exitShape(ctx context.Context, symbol, side, qty string) (tools.OrderRequest, bool, error) {
	return x.ExtendedExits.request(ctx, x, symbol, side, qty)
}
