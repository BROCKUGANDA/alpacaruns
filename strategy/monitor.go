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
	Closed      []string // symbols closed via local TP/SL (crypto)
	Rebuilt     []string // equity SL orders re-placed
	HaltEngaged bool
	HaltReasons []string
}

// Tick runs one enforcement pass. prices resolves current marks; clock is
// injectable for tests (time.Now in production).
func (m *PositionMonitor) Tick(ctx context.Context, prices map[string]float64, equity float64) (*MonitorResult, error) {
	res := &MonitorResult{}
	closed, err := m.enforceTPSL(ctx, prices)
	if err != nil {
		return res, err
	}
	res.Closed = closed
	if err := m.verifyEquityBrackets(ctx); err != nil {
		m.log().Printf("[monitor] bracket verification warning: %v", err)
	}
	halts := m.checkDrawdown(equity)
	res.HaltReasons = halts
	res.HaltEngaged = len(halts) > 0
	if res.HaltEngaged {
		m.Kill.Halt()
		for _, h := range halts {
			m.log().Printf("[monitor] KILL SWITCH ENGAGED: %s", h)
		}
	}
	return res, nil
}

// enforceTPSL compares open crypto positions against stored levels.
func (m *PositionMonitor) enforceTPSL(ctx context.Context, prices map[string]float64) ([]string, error) {
	b := m.broker()
	var closed []string
	if b == nil {
		return closed, nil // test/disabled mode: no broker to close with
	}
	levels, err := m.State.CryptoLevels()
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
			// Position already gone; drop stale levels.
			_ = m.State.ClearLevel(lv.Symbol)
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
		side := ""
		reason := ""
		switch {
		case price >= lv.TakeProfit:
			side, reason = "sell", "take-profit"
		case price <= lv.StopLoss:
			side, reason = "sell", "stop-loss"
		default:
			continue
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
		_ = m.State.ClearLevel(lv.Symbol)
	}
	sort.Strings(closed)
	return closed, nil
}

// verifyEquityBrackets confirms each open equity position with stored
// levels still has a protective (stop/stop_limit) order open; rebuild a
// missing one like the reference architecture does after restarts.
func (m *PositionMonitor) verifyEquityBrackets(ctx context.Context) error {
	b := m.broker()
	if b == nil {
		return nil
	}
	orders, err := b.OpenOrders(ctx)
	if err != nil {
		return fmt.Errorf("list open orders: %w", err)
	}
	protected := map[string]bool{}
	for _, o := range orders {
		t := strings.ToLower(o.Type)
		if t == "stop" || t == "stop_limit" || t == "trailing_stop" {
			protected[o.Symbol] = true
		}
	}
	st, err := m.State.Load()
	if err != nil {
		return err
	}
	syms := make([]string, 0, len(st.Levels))
	for sym, lv := range st.Levels {
		if !lv.Crypto && lv.StopLoss > 0 {
			syms = append(syms, sym)
		}
	}
	sort.Strings(syms)
	for _, sym := range syms {
		if protected[sym] {
			continue
		}
		lv := st.Levels[sym]
		pos, err := m.findPosition(ctx, sym)
		if err != nil {
			m.log().Printf("[monitor] %s: position lookup failed: %v", sym, err)
			continue
		}
		if pos == nil || pos.Qty <= 0 {
			_ = m.State.ClearLevel(sym)
			continue
		}
		stopPrice := round2(lv.StopLoss)
		req := tools.OrderRequest{
			Symbol:      sym,
			Qty:         trimFloat(pos.Qty),
			Side:        "sell",
			Type:        "stop",
			TimeInForce: "gtc",
			StopPrice:   strconv.FormatFloat(stopPrice, 'f', 2, 64),
			// Deterministic per (symbol, price, qty): retries after a
			// timeout dedupe broker-side; a rebuilt stop at a new level
			// gets a fresh id.
			ClientOrderID: fmt.Sprintf("strategy-sl-rebuild-%s-%d-%d",
				sanitize(sym), int64(stopPrice*100), int64(pos.Qty*10000)),
		}
		if _, err := b.Place(ctx, req); err != nil {
			m.log().Printf("[monitor] %s: SL rebuild failed: %v", sym, err)
			continue
		}
		m.log().Printf("[monitor] %s: missing protective order REBUILT as stop @ %.2f", sym, stopPrice)
	}
	return nil
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
		return &positionQty{Qty: q, Price: px}, nil
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

type positionQty struct {
	Qty float64
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
