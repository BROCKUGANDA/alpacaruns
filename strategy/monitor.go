package strategy

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// PositionMonitor enforces TP/SL and drawdown halts every tick:
//
//   - Crypto: brackets are NOT supported by Alpaca on crypto orders, so
//     levels persisted in strategy-state.json are enforced here — a close
//     through either level places a reducing market order.
//   - Equities: server-side bracket children are authoritative. Each tick
//     verifies via ListOrders that an open protective order still exists;
//     a missing one (cancelled/expired/rejected upstream) is rebuilt as a
//     plain stop order at the stored SL price.
//   - Drawdown: daily / weekly / total P/L computed from the pnl journal
//     plus live account equity; any breach engages the shared kill switch
//     and logs loudly.
type PositionMonitor struct {
	Client   *tools.Client
	Kill     *agents.KillSwitch
	State    *StateStore
	Journal  *pnl.Journal
	Settings Settings
	Log      *log.Logger

	// Bars supplies the daily series for ATR-scaled levels. Required only
	// when BRACKET_MODE=atr; the percentage brackets need no bars.
	Bars barSource

	// Broker abstracts the Alpaca calls so tests can inject fakes.
	// When nil it is built lazily from Client; when both are nil the
	// enforcement passes degrade to no-ops (nothing to enforce against).
	Broker BrokerOps

	brokerOnce   sync.Once
	alpacaBroker *alpacaBroker
}

// BrokerOps is the broker surface the monitor needs.
type BrokerOps interface {
	Positions(ctx context.Context) ([]tools.Position, error)
	OpenOrders(ctx context.Context) ([]tools.Order, error)
	Place(ctx context.Context, req tools.OrderRequest) (*tools.Order, error)
	// PlaceOCO protects a position the broker already holds with a
	// take-profit / stop-loss pair (a bracket with no entry leg).
	PlaceOCO(ctx context.Context, req tools.OCOClose) (*tools.Order, error)
	// CancelSymbolOrders pulls every resting order on a symbol, used
	// before a locally-enforced exit so a stale protective leg cannot
	// fire afterwards and open the position back up as a short.
	CancelSymbolOrders(ctx context.Context, symbol string) (int, error)
}

func (m *PositionMonitor) broker() BrokerOps {
	if m.Broker != nil {
		return m.Broker
	}
	if m.Client == nil {
		return nil
	}
	m.brokerOnce.Do(func() { m.alpacaBroker = &alpacaBroker{c: m.Client} })
	return m.alpacaBroker
}

// MonitorResult summarizes one monitor tick for tests and logging.
type MonitorResult struct {
	// Adopted lists positions the reconciliation pass newly protected
	// (or re-anchored to the broker's real average entry).
	Adopted []string
	Closed  []string // symbols closed via local TP/SL (crypto, options)
	Rebuilt []string // equity protective orders re-placed
	HaltEngaged bool
	HaltReasons []string
}

// Tick runs one enforcement pass. prices resolves current marks; clock is
// injectable for tests (time.Now in production).
func (m *PositionMonitor) Tick(ctx context.Context, prices map[string]float64, equity float64) (*MonitorResult, error) {
	return m.TickTrack(ctx, prices, equity, "")
}

// TickTrack runs one enforcement pass restricted to one book: "" = swing
// (the default pass the auto loop has always run), "intraday" = the
// intraday book. Each pass enforces only its own TP/SL levels and equity
// bracket rebuilds so a swing exit never closes an intraday position
// through an intraday bracket, and vice versa. Drawdown halts stay
// global (shared kill switch) — they are evaluated on the swing pass.
func (m *PositionMonitor) TickTrack(ctx context.Context, prices map[string]float64, equity float64, track string) (*MonitorResult, error) {
	res := &MonitorResult{}
	// Position-aware first: adopt every broker position into the level
	// book and make sure the broker actually protects the equities among
	// them. The enforcement passes below only see what this book holds,
	// so running them first would skip anything the bot did not open.
	rec, err := m.Reconcile(ctx, track)
	if err != nil {
		m.log().Printf("[monitor] position reconciliation failed: %v", err)
	}
	res.Adopted = rec.Adopted
	res.Rebuilt = rec.Protected
	closed, err := m.enforceTPSL(ctx, prices, track)
	if err != nil {
		return res, err
	}
	res.Closed = closed
	if track != "intraday" {
		// Drawdown halts engage the SHARED kill switch; running them on
		// every track pass would double-log the same breach.
		halts := m.checkDrawdown(equity)
		res.HaltReasons = halts
		res.HaltEngaged = len(halts) > 0
		if res.HaltEngaged {
			m.Kill.Halt()
			for _, h := range halts {
				m.log().Printf("[monitor] KILL SWITCH ENGAGED: %s", h)
			}
		}
	}
	return res, nil
}

// enforceTPSL compares open crypto positions against stored levels for
// the given track.
func (m *PositionMonitor) enforceTPSL(ctx context.Context, prices map[string]float64, track string) ([]string, error) {
	b := m.broker()
	var closed []string
	if b == nil {
		return closed, nil // test/disabled mode: no broker to close with
	}
	levels, err := m.State.CryptoLevelsForTrack(track)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	for _, lv := range levels {
		// Resolve the position first — it carries an authoritative,
		// always-present current mark (/positions.current_price) and the
		// live qty. This removes the silent-failure window where a
		// Data-API price fetch for a crypto symbol came back empty and
		// the TP/SL check was skipped even though price was above TP.
		pos, err := m.findPosition(ctx, lv.Symbol)
		if err != nil {
			m.log().Printf("[monitor] %s: position lookup failed: %v", lv.Symbol, err)
			continue
		}
		if pos == nil || pos.Qty <= 0 {
			// Position already gone; drop stale levels (of THIS track —
			// the other book may still hold the symbol).
			_ = m.State.ClearLevelTrack(lv.Symbol, lv.Track)
			continue
		}
		price := pos.Price
		if price <= 0 {
			// Broker mark unavailable; fall back to the passed Data-API
			// price map.
			price = prices[lv.Symbol]
		}
		if price <= 0 {
			continue // no reliable mark this tick: nothing to do
		}
		// The trigger direction depends on the POSITION side. A long
		// profits above entry and dies below it; a short is the mirror,
		// so its take-profit sits below the entry and its stop above.
		// Reading a short's levels as a long's closes it immediately:
		// price >= TP is true on the tick after entry when TP is below
		// the market.
		short := lv.IsShort()
		exitSide := "sell"
		if short {
			exitSide = "buy"
		}
		triggered := ""
		switch {
		case lv.TakeProfit <= 0 && lv.StopLoss <= 0:
			// No protective levels were ever stored (a bracket that failed
			// to compute leaves zeros behind). Treating zero as a trigger
			// would market-sell the position on the very next tick, so
			// zero levels mean "no local enforcement".
			continue
		case lv.TakeProfit > 0 && ((short && price <= lv.TakeProfit) || (!short && price >= lv.TakeProfit)):
			triggered = "take-profit"
		case lv.StopLoss > 0 && ((short && price >= lv.StopLoss) || (!short && price <= lv.StopLoss)):
			triggered = "stop-loss"
		default:
			continue
		}
		side, reason := exitSide, triggered
		// Pull any resting protective orders on this symbol BEFORE the
		// close goes out. Otherwise a leg that survives the fill (an
		// adopted OCO pair, or a stop rebuilt by an earlier tick) fires
		// later against a flat account and opens the position back up on
		// the wrong side.
		if n, err := b.CancelSymbolOrders(ctx, lv.Symbol); err != nil {
			m.log().Printf("[monitor] %s: could not clear resting orders before the %s close: %v", lv.Symbol, reason, err)
		} else if n > 0 {
			m.log().Printf("[monitor] %s: cancelled %d resting order(s) before the %s close", lv.Symbol, n, reason)
		}
		req := tools.OrderRequest{
			Symbol:      lv.Symbol,
			Qty:         trimFloat(pos.Qty),
			Side:        side,
			Type:        "market",
			TimeInForce: "gtc",
			// Deterministic per (symbol, reason, levels, qty): a retry
			// after a timeout carries the SAME id so Alpaca rejects the
			// duplicate instead of double-selling into an accidental
			// short. New levels/qty produce a new id for legitimate
			// follow-up closes.
			ClientOrderID: fmt.Sprintf("strategy-tpsl-%s-%s-%d-%d-%d",
				sanitize(lv.Symbol), reason,
				int64(lv.TakeProfit*100), int64(lv.StopLoss*100), int64(pos.Qty*10000)),
		}
		o, err := b.Place(ctx, req)
		if err != nil {
			m.log().Printf("[monitor] %s: %s close failed: %v", lv.Symbol, reason, err)
			continue
		}
		m.log().Printf("[monitor] %s hit %s at %.2f (levels tp=%.2f sl=%.2f): closing order %s",
			lv.Symbol, reason, price, lv.TakeProfit, lv.StopLoss, o.ID)
		closed = append(closed, lv.Symbol)
		// Clear with the level's OWN track so an intraday close removes
		// the intraday bracket (and a swing close the swing bracket)
		// even when both books hold the same symbol.
		_ = m.State.ClearLevelTrack(lv.Symbol, lv.Track)
	}
	sort.Strings(closed)
	return closed, nil
}

// checkDrawdown computes daily / weekly / total drawdown vs peak equity
// from the journal + current equity, returning breach reasons (empty =
// healthy).
func (m *PositionMonitor) checkDrawdown(equity float64) []string {
	if equity <= 0 {
		return nil
	}
	st, err := m.State.Load()
	if err != nil {
		return []string{fmt.Sprintf("state unreadable for drawdown check: %v", err)}
	}
	if equity > st.PeakEquity {
		st.PeakEquity = equity
		_ = m.State.Save(st)
	}
	fills, err := m.Journal.Fills(time.Time{})
	if err != nil {
		return []string{fmt.Sprintf("journal unreadable for drawdown check: %v", err)}
	}
	now := time.Now().UTC()
	dayAgo := now.AddDate(0, 0, -1)
	weekStart := now.AddDate(0, 0, -int(now.Weekday())) // Sunday 00:00 UTC anchor

	realizedDay, realizedWeek := 0.0, 0.0
	for _, f := range fills {
		if f.TS.After(dayAgo) {
			realizedDay += fillPL(f)
		}
		if f.TS.After(weekStart) {
			realizedWeek += fillPL(f)
		}
	}

	var reasons []string
	check := func(label string, dd, limit float64) {
		if dd >= limit {
			reasons = append(reasons, fmt.Sprintf("%s drawdown %.1f%% >= halt %.1f%%", label, 100*dd, 100*limit))
		}
	}
	// Drawdown = loss relative to the period's starting equity proxy:
	// peak minus current, expressed against peak. Daily/weekly realized
	// losses count toward their periods even when equity is propped up
	// by unrealized gains elsewhere.
	totalDD := max(0, (st.PeakEquity-equity)/st.PeakEquity)
	dailyDD := max(0, -realizedDay/st.PeakEquity)
	weeklyDD := max(0, -realizedWeek/st.PeakEquity)
	check("daily", dailyDD, m.Settings.DailyDD)
	check("weekly", weeklyDD, m.Settings.WeeklyDD)
	check("total", totalDD, m.Settings.TotalDD)
	return reasons
}

// findPosition returns the live quantity for symbol, or nil when flat.
func (m *PositionMonitor) findPosition(ctx context.Context, symbol string) (*positionQty, error) {
	b := m.broker()
	if b == nil {
		return nil, nil
	}
	ps, err := b.Positions(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		// Broker position symbols are venue-formatted: crypto comes back
		// as "BTCUSD" while strategy levels use "BTC/USD" (BASE/QUOTE).
		// Match either form (slash-stripped, case-insensitive) so a
		// crypto TP/SL level reliably finds its position instead of
		// silently failing and dropping the level.
		want := strings.ToLower(strings.ReplaceAll(symbol, "/", ""))
		got := strings.ToLower(strings.ReplaceAll(p.Symbol, "/", ""))
		if want != got {
			continue
		}
		q, _ := strconv.ParseFloat(p.Qty, 64)
		if q == 0 {
			continue // a zero-quantity row is not a position
		}
		// Capture the broker's own current mark too. The broker
		// /positions response carries current_price for every open
		// position, which is an authoritative, always-present quote
		// for the TP/SL check — far more reliable than re-fetching
		// Data-API snapshots in a separate loop (those can be empty
		// or rate-limited, which is exactly how a crypto TP silently
		// failed to fire while price was above the bracket).
		var px float64
		if cp := strings.TrimSpace(p.CurrentPrice); cp != "" {
			px, _ = strconv.ParseFloat(cp, 64)
		}
		return &positionQty{Qty: math.Abs(q), Short: isShortPosition(p.Side, q), Price: px}, nil
	}
	return nil, nil
}

// alpacaBroker adapts *tools.Client to BrokerOps (production wiring).
type alpacaBroker struct{ c *tools.Client }

func (a *alpacaBroker) Positions(ctx context.Context) ([]tools.Position, error) {
	return a.c.GetPositions(ctx)
}

func (a *alpacaBroker) OpenOrders(ctx context.Context) ([]tools.Order, error) {
	return a.c.ListOrders(ctx, "open", 500)
}

func (a *alpacaBroker) Place(ctx context.Context, req tools.OrderRequest) (*tools.Order, error) {
	return a.c.PlaceOrder(ctx, req)
}

func (a *alpacaBroker) PlaceOCO(ctx context.Context, req tools.OCOClose) (*tools.Order, error) {
	return a.c.PlaceOCOClose(ctx, req)
}

func (a *alpacaBroker) CancelSymbolOrders(ctx context.Context, symbol string) (int, error) {
	return a.c.CancelOrdersForSymbol(ctx, symbol)
}

type positionQty struct {
	// Qty is the ABSOLUTE position quantity. Closing either side trades
	// that many contracts/shares, and the direction comes from Short.
	Qty float64
	// Short reports the position side, so the exit is a buy rather
	// than a sell.
	Short bool
	// Price is the position's current mark as reported by the broker
	// (/positions.current_price). Zero when unavailable; callers fall
	// back to the Data-API price map in that case.
	Price float64
}

func (m *PositionMonitor) log() *log.Logger {
	if m.Log == nil {
		return log.Default()
	}
	return m.Log
}

// fillPL approximates per-fill P/L contribution for drawdown math:
// negative buys reduce cash (risk outlay), positive sells return it.
// Realized accuracy lives in pnl.Compute; this only needs sign/magnitude.
func fillPL(r pnl.Record) float64 {
	q, _ := strconv.ParseFloat(r.Qty, 64)
	p, _ := strconv.ParseFloat(r.Price, 64)
	v := q * p
	if r.Side == "buy" {
		return -v
	}
	return v
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func sanitize(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "/", "-"), " ", "")
}
