package strategy

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// This file makes TP/SL POSITION-aware rather than entry-aware.
//
// The rest of the monitor is driven by stored levels, and levels were
// only ever written by ExecuteEntry — so protection existed exactly and
// only for positions this process opened itself. Every other position in
// the account was invisible to it: one opened by hand on the dashboard,
// one placed by the agent path, one that survived a redeploy of the data
// directory, or one that was already there when the bot started. The
// broker reports the position, the monitor looks up a level, finds none,
// and skips it. A position with no level has no take-profit and no stop,
// and nothing says so.
//
// Reconcile closes that gap by driving from the broker's positions
// instead of from the level book, and anchors every level to the
// position's REAL average entry price (avg_entry_price) rather than the
// price the entry was planned at. Those two are not the same number: a
// market order prints where the tape is, and averaging into a position
// moves the entry — so levels computed from the plan-time estimate sit
// at the wrong distance from the actual cost basis.

// levelEntryTolerance is how far a stored entry may drift from the
// broker's average entry before the levels are re-derived. Half a cent
// on a two-decimal price is below any meaningful move.
const levelEntryTolerance = 0.005

// Reconcile adopts every open broker position into the protection book
// and makes sure an equity position is actually protected at the broker,
// not just in local state. It runs before the enforcement passes so
// those passes see a complete book.
//
// track scopes it to one book ("" = swing, "intraday"): a swing position
// and an intraday position in the same symbol are separate exposures and
// get their own levels.
func (m *PositionMonitor) Reconcile(ctx context.Context, track string) (ReconcileResult, error) {
	b := m.broker()
	if b == nil {
		return ReconcileResult{}, nil
	}
	positions, err := b.Positions(ctx)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("positions: %w", err)
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i].Symbol < positions[j].Symbol })

	// One open-order snapshot for the whole pass: the equity branch needs
	// to know what already protects each position, and asking per symbol
	// would multiply the calls by the position count.
	var openOrders []tools.Order
	ordersKnown := false
	orders, err := b.OpenOrders(ctx)
	if err == nil {
		openOrders = orders
		ordersKnown = true
	} else {
		m.log().Printf("[protect] open-order snapshot failed: %v — equity protection cannot be verified this tick", err)
	}

	var res ReconcileResult
	// Positions the broker no longer reports must not keep levels: a
	// level with no position behind it is a stale trigger waiting to
	// fire against a flat account.
	if dropped, err := m.dropOrphanedLevels(ctx, positions, track); err != nil {
		m.log().Printf("[protect] orphan sweep failed: %v", err)
	} else if len(dropped) > 0 {
		m.log().Printf("[protect] dropped levels for %d closed position(s): %v", len(dropped), dropped)
	}

	for _, p := range positions {
		qty, _ := strconv.ParseFloat(strings.TrimSpace(p.Qty), 64)
		if qty == 0 {
			continue
		}
		sym := CanonicalSymbol(p.Symbol)
		short := isShortPosition(p.Side, qty)
		avgEntry, _ := strconv.ParseFloat(strings.TrimSpace(p.AvgEntry), 64)
		if avgEntry <= 0 {
			// No cost basis means there is no honest stop distance to
			// derive. An existing level is kept rather than churned — it
			// was written from a real price — and still used to verify
			// broker protection.
			if held := m.State.Level(sym, track); held != nil {
				if !held.LocallyEnforced() && ordersKnown {
					m.ensureBrokerProtection(held, openOrders)
				}
				continue
			}
			m.log().Printf("[protect] %s: broker reported no average entry; position is UNPROTECTED", sym)
			m.journalProtect(sym, "REJECTED", "no average entry from the broker; position left unprotected")
			continue
		}

		// Ownership first. The broker reports ONE aggregated position per
		// symbol, but this bot runs two books on the same symbols. If the
		// other book already carries the levels, that book owns the
		// exposure — adopting it here too would size, protect and close
		// the same shares twice, and place a second protective pair.
		if _, owner := m.State.LevelAnyTrack(sym); owner != "" && owner != track {
			continue
		}
		// When adoption is scoped to the traded universe, leave anything
		// outside it alone: the bot has no factor data on it and no
		// opinion about where its stop belongs.
		if !m.Settings.ProtectAllPositions && !m.inUniverse(sym, track) {
			m.log().Printf("[protect] %s: outside the traded universe; left alone (TPSL_PROTECT_ALL=false)", sym)
			continue
		}

		existing := m.State.Level(sym, track)
		if existing != nil && !levelStale(existing, avgEntry, qty, short) {
			// Levels are current. An equity still needs its broker-side
			// pair verified (a child can be cancelled upstream).
			if !existing.LocallyEnforced() && ordersKnown {
				if m.ensureBrokerProtection(existing, openOrders) {
					res.Protected = append(res.Protected, existing.Symbol)
				}
			}
			continue
		}

		lv, err := m.deriveLevel(sym, avgEntry, qty, short, p.AssetClass, track, ctx)
		if err != nil {
			m.log().Printf("[protect] %s: could not derive levels: %v — position is UNPROTECTED", sym, err)
			m.journalProtect(sym, "REJECTED", "levels could not be derived: "+err.Error())
			continue
		}
		if err := m.State.SetLevel(*lv); err != nil {
			m.log().Printf("[protect] %s: level persist failed: %v", sym, err)
			continue
		}
		action := "ADOPTED"
		if existing != nil {
			action = "RE-ANCHORED"
		}
		m.log().Printf("[protect] %s %s qty=%s avg_entry=%.4f tp=%.4f sl=%.4f (%s)",
			lv.Symbol, action, trimFloat(math.Abs(qty)), lv.EntryPrice, lv.TakeProfit, lv.StopLoss, classOf(lv))
		m.journalProtect(lv.Symbol, "APPROVED", fmt.Sprintf("%s %s qty=%s entry=%.4f tp=%.4f sl=%.4f",
			strings.ToLower(action), sideName(short), trimFloat(math.Abs(qty)), lv.EntryPrice, lv.TakeProfit, lv.StopLoss))
		res.Adopted = append(res.Adopted, lv.Symbol)

		if !lv.LocallyEnforced() && ordersKnown {
			if m.ensureBrokerProtection(lv, openOrders) {
				res.Protected = append(res.Protected, lv.Symbol)
			}
		}
	}
	return res, nil
}

// ReconcileResult reports what the position pass changed.
type ReconcileResult struct {
	// Adopted lists positions newly in the book (or re-anchored to the
	// broker's real average entry).
	Adopted []string
	// Protected lists equities whose protective pair the pass placed.
	Protected []string
}

// dropOrphanedLevels clears stored levels in this track whose position
// the broker no longer reports. It is driven by the position list, so an
// equity is cleaned up exactly like a crypto pair — the previous pass
// only swept levels it happened to iterate, and equities entered by the
// bracket path were never revisited once closed.
func (m *PositionMonitor) dropOrphanedLevels(ctx context.Context, positions []tools.Position, track string) ([]string, error) {
	b := m.broker()
	if b == nil {
		return nil, nil
	}
	st, err := m.State.Load()
	if err != nil {
		return nil, err
	}
	held := make([]string, 0, len(positions))
	for _, p := range positions {
		q, _ := strconv.ParseFloat(strings.TrimSpace(p.Qty), 64)
		if q == 0 {
			continue
		}
		held = append(held, CanonicalSymbol(p.Symbol))
	}
	var dropped []string
	for key, lv := range st.Levels {
		if lv == nil || lv.Track != track {
			continue
		}
		sym := CanonicalSymbol(lv.Symbol)
		found := false
		for _, h := range held {
			if h == sym {
				found = true
				break
			}
		}
		if found {
			continue
		}
		delete(st.Levels, key)
		dropped = append(dropped, lv.Symbol)
	}
	if len(dropped) == 0 {
		return nil, nil
	}
	sort.Strings(dropped)
	if err := m.State.Save(st); err != nil {
		return dropped, err
	}
	return dropped, nil
}

// inUniverse reports whether the symbol belongs to the track's own
// configured universe.
func (m *PositionMonitor) inUniverse(sym, track string) bool {
	list := m.Settings.EquitySymbols
	if track == "intraday" {
		list = m.Settings.IntradaySymbols
	}
	if IsCrypto(sym) {
		list = append(list, m.Settings.CryptoSymbols...)
	}
	for _, s := range list {
		if tools.SameSymbol(s, sym) {
			return true
		}
	}
	return false
}

// deriveLevel computes TP/SL for a position from its ACTUAL average
// entry, honouring the configured bracket mode and the track's own
// percentages (the intraday book scalps with tighter brackets).
func (m *PositionMonitor) deriveLevel(sym string, avgEntry, qty float64, short bool, assetClass, track string, ctx context.Context) (*PositionLevels, error) {
	tpPct, slPct := m.Settings.TPPct, m.Settings.SLPct
	if track == "intraday" {
		tpPct, slPct = m.Settings.IntradayTPPct, m.Settings.IntradaySLPct
	}

	lv := PositionLevels{
		Symbol:     sym,
		EntryPrice: round2(avgEntry),
		Qty:        trimFloat(math.Abs(qty)),
		Side:       sideName(short),
		AssetClass: assetClass,
		Track:      track,
		Crypto:     IsCrypto(sym),
	}
	if lv.AssetClass == "" {
		lv.AssetClass = inferAssetClass(sym)
	}

	// ATR scaling needs a bar series. Options have no bars in this client
	// and crypto ATR is available, so only those two attempt it; a
	// failure falls back to percentage brackets rather than leaving the
	// position with nothing.
	if m.Settings.BracketMode == "atr" && m.Bars != nil && lv.AssetClass != tools.AssetClassOption {
		if atr, ok := m.atrFor(ctx, sym); ok {
			bl, err := ATRDirectionalBrackets(avgEntry, atr, m.Settings.ATRMultTP, m.Settings.ATRMultSL, short)
			if err == nil {
				lv.TakeProfit, lv.StopLoss = bl.TakeProfit, bl.StopLoss
				return &lv, nil
			}
			m.log().Printf("[protect] %s: ATR brackets rejected (%v); using percentage brackets", sym, err)
		}
	}
	bl, err := DirectionalBrackets(avgEntry, tpPct, slPct, short)
	if err != nil {
		return nil, err
	}
	lv.TakeProfit, lv.StopLoss = bl.TakeProfit, bl.StopLoss
	return &lv, nil
}

// atrFor computes the 14-bar ATR used for volatility-scaled levels.
func (m *PositionMonitor) atrFor(ctx context.Context, sym string) (float64, bool) {
	bars, err := m.Bars.GetBars(ctx, []string{sym}, "1Day", "", "", 30)
	if err != nil {
		m.log().Printf("[protect] %s: ATR bars unavailable: %v", sym, err)
		return 0, false
	}
	atr, ok := ATR14(bars[CanonicalSymbol(sym)])
	if !ok {
		atr, ok = ATR14(bars[sym])
	}
	if !ok || atr <= 0 {
		return 0, false
	}
	return atr, true
}

// ensureBrokerProtection places the protective pair on an equity
// position that the broker is not already guarding. Crypto and options
// are enforced locally (Alpaca has no order class for either), so this
// only acts on levels the broker can hold for us.
func (m *PositionMonitor) ensureBrokerProtection(lv *PositionLevels, openOrders []tools.Order) bool {
	b := m.broker()
	if b == nil {
		return false
	}
	existing := inspectProtection(openOrders, lv.Symbol)
	if existing.State == protectionComplete && sameProtection(existing, lv) && existing.Duplicates == 0 {
		return false
	}
	// Something is resting but it is wrong: stale prices, only one leg,
	// or more than one pair on the same position. Clear it first, then
	// place exactly one correct pair. The order matters — placing before
	// cancelling would leave two protective pairs live simultaneously.
	if existing.State != protectionNone {
		n, err := b.CancelSymbolOrders(context.Background(), lv.Symbol)
		if err != nil {
			m.log().Printf("[protect] %s: could not clear mismatched protection (%v); leaving the resting pair alone rather than stacking a second", lv.Symbol, err)
			return false
		}
		m.log().Printf("[protect] %s: cleared %d stale/duplicate order(s) before re-placing tp=%.4f sl=%.4f",
			lv.Symbol, n, lv.TakeProfit, lv.StopLoss)
		m.journalProtect(lv.Symbol, "APPROVED", fmt.Sprintf(
			"replaced stale protective pair: cleared %d order(s), new tp=%.4f sl=%.4f (entry %.4f)",
			n, lv.TakeProfit, lv.StopLoss, lv.EntryPrice))
	}
	qty, _ := strconv.ParseFloat(lv.Qty, 64)
	if qty <= 0 {
		return false
	}
	side := "sell"
	if lv.IsShort() {
		side = "buy"
	}
	req := tools.OCOClose{
		Symbol:      lv.Symbol,
		Qty:         lv.Qty,
		Side:        side,
		TimeInForce: "gtc",
		TakeProfit:  lv.TakeProfit,
		StopLoss:    lv.StopLoss,
		// Deterministic per (symbol, levels, qty): a retry after a
		// timeout dedupes at the broker instead of stacking a second
		// protective pair on the same position.
		ClientOrderID: fmt.Sprintf("strategy-protect-%s-%d-%d-%d",
			sanitize(lv.Symbol), int64(lv.TakeProfit*100), int64(lv.StopLoss*100), int64(qty*10000)),
	}
	o, err := b.PlaceOCO(context.Background(), req)
	if err != nil {
		m.log().Printf("[protect] %s: protective pair FAILED — position is UNPROTECTED: %v", lv.Symbol, err)
		m.journalProtect(lv.Symbol, "REJECTED", "protective pair failed: "+err.Error())
		return false
	}
	m.log().Printf("[protect] %s: protective pair placed (oco order=%s) tp=%.4f sl=%.4f qty=%s side=%s",
		lv.Symbol, o.ID, lv.TakeProfit, lv.StopLoss, lv.Qty, side)
	m.journalProtect(lv.Symbol, "APPROVED", fmt.Sprintf("protective pair placed order=%s tp=%.4f sl=%.4f qty=%s exit=%s",
		o.ID, lv.TakeProfit, lv.StopLoss, lv.Qty, side))
	return true
}

// protectionClass is what the broker currently holds for one position.
type protectionClass int

const (
	protectionNone protectionClass = iota
	// protectionPartial means a lone stop or a lone take-profit is
	// resting. Not a pair: if only the stop exists, upside is never
	// banked; if only the take-profit exists, there is no floor.
	protectionPartial
	protectionComplete
)

// restingProtection is what the broker currently holds for one position.
type restingProtection struct {
	State      protectionClass
	TakeProfit float64
	StopLoss   float64
	// OrderIDs are the resting orders on the symbol that make up the
	// protection.
	OrderIDs []string
	// Duplicates counts protective parents beyond the first. Two pairs on
	// a five-share position can sell ten shares, so they must not coexist.
	Duplicates int
}

// inspectProtection reads the open orders for a protective pair on the
// symbol. An oco or bracket parent covers both sides on its own.
func inspectProtection(orders []tools.Order, symbol string) restingProtection {
	var out restingProtection
	var parents int
	var hasStop, hasTP bool
	for _, o := range orders {
		if !tools.SameSymbol(o.Symbol, symbol) {
			continue
		}
		out.OrderIDs = append(out.OrderIDs, o.ID)
		if tp, err := strconv.ParseFloat(strings.TrimSpace(o.LimitPrice), 64); err == nil && tp > 0 {
			out.TakeProfit = tp
		}
		if sp, err := strconv.ParseFloat(strings.TrimSpace(o.StopPrice), 64); err == nil && sp > 0 {
			out.StopLoss = sp
		}
		switch strings.ToLower(o.OrderClass) {
		case "oco", "bracket":
			parents++
			continue
		}
		switch strings.ToLower(o.Type) {
		case "stop", "stop_limit", "trailing_stop":
			hasStop = true
		case "limit":
			// A resting limit in the direction that CLOSES the position is
			// the take-profit leg.
			hasTP = true
		}
	}
	if parents > 1 {
		out.Duplicates = parents - 1
	}
	switch {
	case parents > 0 || (hasStop && hasTP):
		out.State = protectionComplete
	case hasStop || hasTP:
		out.State = protectionPartial
	default:
		out.State = protectionNone
	}
	return out
}

// sameProtection reports whether a resting pair still matches the level.
//
// A KNOWN mismatch means the broker is guarding at the old cost basis
// after a re-anchor, so the pair is replaced. Missing price data is
// treated as a match rather than a mismatch: cancelling and re-placing on
// every tick because a payload omitted a field would churn a perfectly
// good protective pair, and the position is safer left protected at
// approximately the right level than repeatedly stripped and rebuilt.
func sameProtection(r restingProtection, lv *PositionLevels) bool {
	const tol = 0.011
	if r.TakeProfit <= 0 || r.StopLoss <= 0 {
		return true
	}
	return math.Abs(r.TakeProfit-lv.TakeProfit) <= tol && math.Abs(r.StopLoss-lv.StopLoss) <= tol
}

// levelStale reports whether a stored level no longer describes the
// position the broker reports. Any of these changes the honest stop
// distance, so the level must be rebuilt from the position:
//
//   - the average entry moved (a partial fill printed elsewhere, or the
//     position was added to),
//   - the quantity changed,
//   - the side flipped (a long that was sold down and bought back short),
//   - the level carries no entry price at all (written by the old
//     plan-time path).
func levelStale(lv *PositionLevels, avgEntry, qty float64, short bool) bool {
	if lv.EntryPrice <= 0 {
		return true
	}
	if math.Abs(lv.EntryPrice-avgEntry) > levelEntryTolerance {
		return true
	}
	storedQty, _ := strconv.ParseFloat(lv.Qty, 64)
	if math.Abs(storedQty-math.Abs(qty)) > 1e-9 {
		return true
	}
	if lv.IsShort() != short {
		return true
	}
	if lv.TakeProfit <= 0 || lv.StopLoss <= 0 {
		return true
	}
	return false
}

// isShortPosition reads the broker's side, falling back to the signed
// quantity when the side field is absent.
func isShortPosition(side string, qty float64) bool {
	s := strings.ToLower(strings.TrimSpace(side))
	if s == "short" {
		return true
	}
	if s == "long" {
		return false
	}
	return qty < 0
}

// sideName renders the side the way levels store it.
func sideName(short bool) string {
	if short {
		return "short"
	}
	return "long"
}

// inferAssetClass is the fallback when the broker payload carries no
// asset_class: OCC symbols are options, slash pairs are crypto, and
// everything else is treated as an equity.
func inferAssetClass(sym string) string {
	switch {
	case tools.IsOCCSymbol(sym):
		return tools.AssetClassOption
	case IsCrypto(sym):
		return tools.AssetClassCrypto
	default:
		return tools.AssetClassEquity
	}
}

func classOf(lv *PositionLevels) string {
	if lv.AssetClass != "" {
		return lv.AssetClass
	}
	return inferAssetClass(lv.Symbol)
}

// journalProtect records the protection decisions in the trade log, so
// "why is this position guarded at this price" is answerable from the
// audit trail rather than from the log files.
func (m *PositionMonitor) journalProtect(symbol, riskLabel, detail string) {
	if m.Journal == nil {
		return
	}
	_ = m.Journal.Append(pnl.Record{
		Kind:   pnl.KindDecision,
		Source: "strategy:protect",
		Symbol: symbol,
		Risk:   riskLabel,
		Detail: detail,
	})
}
