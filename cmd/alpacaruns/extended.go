package main

// extended.go: routing an entry into the session it belongs to.
//
// The bot used to have two answers to "what is the market doing": open or
// closed. That is enough while the only thing you can do when closed is wait,
// but the pre-market and after-hours sessions are a third state — tradable,
// on terms no regular-hours order uses. Here those terms are enforced in one
// place, so an entry, its liquidity bar, and the order shape all come from the
// same reading of the session rather than three that can disagree.

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"

	"github.com/BROCKUGANDA/alpacaruns/strategy"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// entryKind is how a BUY reaches the broker, given the session.
type entryKind int

const (
	// routeLive is a regular-session order.
	routeLive entryKind = iota
	// routeExtended is a pre-market or after-hours marketable limit.
	routeExtended
	// routePreOrder queues a resting limit for the next open (PRE_ORDERS).
	routePreOrder
	// routeBlocked refuses the entry, with the reason in `why`.
	routeBlocked
)

// entryRoute carries the decision and, for an extended entry, the single
// quote every later judgement about this trade is made against.
type entryRoute struct {
	kind  entryKind
	why   string
	quote strategy.ExtendedQuote
}

// routeEntry resolves the session from the broker's calendar and maps it to
// an entry route.
//
// An unreadable calendar is a BLOCKED route, not a guess. The alternative —
// assuming the regular session — is what let the agent path place orders
// during an outage; assuming closed would turn a calendar blip into a bot
// that silently stops trading, which is the other half of the same bug. The
// answer here is to refuse the entry, say why, and let the next tick ask
// again.
func (l *autoLoop) routeEntry(ctx context.Context, symbol string) entryRoute {
	if strategy.IsCrypto(symbol) {
		return entryRoute{kind: routeLive}
	}
	if l.sessions == nil {
		// No session source (unit tests, or a loop built without one):
		// preserve the historical clock-only behaviour.
		if l.equityMarketOpen(ctx) {
			return entryRoute{kind: routeLive}
		}
		if l.cfg != nil && l.cfg.PreOrders {
			return entryRoute{kind: routePreOrder}
		}
		return entryRoute{kind: routeBlocked, why: "market closed"}
	}
	sess, day, err := l.sessions.Current(ctx)
	if err != nil {
		return entryRoute{kind: routeBlocked, why: fmt.Sprintf("market session unknown (%v)", err)}
	}
	switch sess {
	case tools.SessionRegular:
		return entryRoute{kind: routeLive}
	case tools.SessionPreMarket, tools.SessionAfterHours:
		if r := l.extendedEntry(ctx, symbol, sess, day); r.kind != routeBlocked {
			return r
		} else if l.cfg != nil && l.cfg.PreOrders {
			// The session is open but this entry is not eligible for it
			// (opt-out, outside the window, stale or wide quote). With
			// PRE_ORDERS the fallback is the resting order the bot used
			// before extended trading existed; without it the reason the
			// extended route refused is the answer the operator needs.
			return entryRoute{kind: routePreOrder}
		} else {
			return r
		}
	default:
		if l.cfg != nil && l.cfg.PreOrders {
			return entryRoute{kind: routePreOrder}
		}
		return entryRoute{kind: routeBlocked,
			why: fmt.Sprintf("market closed (%s, %s)", sess, day.Date)}
	}
}

// extendedEntry decides whether one equity may open in the extended session,
// and returns the quote it would trade against. Every refusal here is a
// reason the operator can act on: the session opt-out, the window, the age of
// the print, and the width of the market.
func (l *autoLoop) extendedEntry(ctx context.Context, symbol string, sess tools.Session, day tools.DayWindow) entryRoute {
	if tools.IsOCCSymbol(symbol) {
		return entryRoute{kind: routeBlocked,
			why: fmt.Sprintf("%s options do not trade outside the regular session (Alpaca refuses extended_hours on options)", sess)}
	}
	if l.cfg == nil || !l.cfg.ExtendedHours {
		return entryRoute{kind: routeBlocked, why: fmt.Sprintf("market closed (%s: EXTENDED_HOURS=false)", sess)}
	}
	if !l.inExtendedWindow(symbol) {
		spec := ""
		if l.cfg != nil {
			spec = l.cfg.ExtendedHoursWindows
		}
		return entryRoute{kind: routeBlocked,
			why: fmt.Sprintf("outside EXTENDED_HOURS_WINDOWS (%s) during %s", spec, sess)}
	}
	if l.exec == nil {
		return entryRoute{kind: routeBlocked, why: "no executor wired for extended entries"}
	}
	q, err := l.exec.ExtendedMark(ctx, symbol, l.cfg.MaxQuoteAge())
	if err != nil {
		return entryRoute{kind: routeBlocked, why: err.Error()}
	}
	if spreadCap := l.cfg.ExtendedSpreadCapBps(); spreadCap > 0 {
		if bps := q.SpreadBps(); bps > spreadCap {
			return entryRoute{kind: routeBlocked,
				why: fmt.Sprintf("%s spread %.1fbps exceeds the %.1fbps extended-session cap", symbol, bps, spreadCap)}
		}
	}
	log.Printf("[rest-of-hours] %s eligible in %s: bid=%.4f ask=%.4f spread %.1fbps, print %s old",
		symbol, sess, q.Bid, q.Ask, q.SpreadBps(), time.Since(q.At).Round(time.Second))
	return entryRoute{kind: routeExtended, quote: q}
}

// inExtendedWindow applies EXTENDED_HOURS_WINDOWS. A missing window set is
// "not tradable": an unset window is not a licence to trade the whole session,
// it is a configuration that was never completed.
func (l *autoLoop) inExtendedWindow(symbol string) bool {
	if l.extWin == nil {
		return false
	}
	return l.extWin.CanTrade(symbol)
}

// newExtendedWindows parses EXTENDED_HOURS_WINDOWS. It returns nil (never an
// error) when the session is off, so the trading loop and the `sessions`
// command read the same rule: no windows means no extended entries, and
// `sessions` can say why.
func newExtendedWindows(cfg *config.Config) (*strategy.TradingWindows, error) {
	if cfg == nil || !cfg.ExtendedHours {
		return nil, nil
	}
	return strategy.NewTradingWindows(cfg.ExtendedHoursWindows, cfg.ExtendedHoursWindows, nil, nil)
}

// submitExtendedEntry is the extended-hours entry wrapper, with the same
// book-invalidation discipline as every other mutation: the next cap check in
// this tick has to see what this order did to the account.
func (l *autoLoop) submitExtendedEntry(ctx context.Context, plan strategy.EntryPlan, q strategy.ExtendedQuote, conf float64) error {
	err := l.exec.ExecuteExtendedEntry(ctx, plan, q, conf)
	l.book.Invalidate()
	return err
}
