package strategy

import (
	"context"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// These tests pin the position-aware behaviour: protection is driven by
// what the BROKER says is open, not by what this process remembers
// placing. Every one of them fails against the previous monitor, which
// iterated stored levels only and therefore never looked at a position
// it had not opened itself.

func posMonitor(t *testing.T, kill *agents.KillSwitch, set Settings, positions []tools.Position, open []tools.Order) (*PositionMonitor, *fakeBroker) {
	t.Helper()
	dir := t.TempDir()
	jPath := filepath.Join(dir, "trades.jsonl")
	j, err := pnl.Open(jPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	fb := &fakeBroker{openStops: map[string]bool{}, positions: positions}
	if open != nil {
		fb.openOrders = open
	}
	m := &PositionMonitor{
		Kill: kill, State: NewStateStore(jPath), Journal: j,
		Settings: set, Log: log.New(os.Stderr, "", 0), Broker: fb,
	}
	return m, fb
}

// TestReconcileAdoptsForeignPosition is the core case: a position that
// exists on the broker but that this bot never opened must still get a
// stop and a target, anchored to the broker's average entry price.
func TestReconcileAdoptsForeignPosition(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "AAPL", Qty: "5", Side: "long",
			AvgEntry: "319.69", CurrentPrice: "347.72", AssetClass: tools.AssetClassEquity,
		}}, nil)

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}

	lv := m.State.Level("AAPL", "")
	if lv == nil {
		t.Fatal("a broker position with no levels must be adopted — it is currently unprotected")
	}
	if math.Abs(lv.EntryPrice-319.69) > 0.01 {
		t.Fatalf("entry must be the broker's avg_entry_price, got %v", lv.EntryPrice)
	}
	set := defSettings()
	wantTP := round2(319.69 * (1 + set.TPPct))
	wantSL := round2(319.69 * (1 - set.SLPct))
	if math.Abs(lv.TakeProfit-wantTP) > 0.01 || math.Abs(lv.StopLoss-wantSL) > 0.01 {
		t.Fatalf("levels off the real entry: tp=%v want %v, sl=%v want %v", lv.TakeProfit, wantTP, lv.StopLoss, wantSL)
	}
	// An equity is protected AT THE BROKER, not only in local state.
	if len(fb.oco) != 1 {
		t.Fatalf("expected one protective oco pair placed, got %d", len(fb.oco))
	}
	got := fb.oco[0]
	if got.Symbol != "AAPL" || got.Side != "sell" || got.Qty != "5" {
		t.Fatalf("protective pair shape wrong: %+v", got)
	}
	if math.Abs(got.TakeProfit-wantTP) > 0.01 || math.Abs(got.StopLoss-wantSL) > 0.01 {
		t.Fatalf("protective pair prices wrong: %+v", got)
	}
}

// TestReconcileShortPosition proves a short gets INVERTED levels and a
// buy to close. Long-style levels on a short put the take-profit above
// the entry, which fires on the next tick and buys the position back at
// a loss immediately.
func TestReconcileShortPosition(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "TSLA", Qty: "-3", Side: "short",
			AvgEntry: "400", CurrentPrice: "390", AssetClass: tools.AssetClassEquity,
		}}, nil)

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	lv := m.State.Level("TSLA", "")
	if lv == nil {
		t.Fatal("short position was not adopted")
	}
	if !lv.IsShort() {
		t.Fatalf("side must be recorded as short: %+v", lv)
	}
	if lv.TakeProfit >= lv.EntryPrice || lv.StopLoss <= lv.EntryPrice {
		t.Fatalf("short levels must invert: entry=%v tp=%v sl=%v", lv.EntryPrice, lv.TakeProfit, lv.StopLoss)
	}
	if len(fb.oco) != 1 {
		t.Fatalf("expected a protective pair, got %d", len(fb.oco))
	}
	if fb.oco[0].Side != "buy" {
		t.Fatalf("closing a short is a buy, got %q", fb.oco[0].Side)
	}
	if fb.oco[0].Qty != "3" {
		t.Fatalf("qty must be absolute, got %q", fb.oco[0].Qty)
	}
}

// TestReconcileReanchorsToRealEntry proves a level anchored to a stale
// plan-time price is rebuilt from the broker's actual average entry —
// which is the difference between a stop at the intended distance and a
// stop at the right one.
func TestReconcileReanchorsToRealEntry(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "MSFT", Qty: "10", Side: "long",
			AvgEntry: "520", CurrentPrice: "521", AssetClass: tools.AssetClassEquity,
		}}, nil)
	// A level written from a plan-time estimate that no longer matches.
	if err := m.State.SetLevel(PositionLevels{
		Symbol: "MSFT", EntryPrice: 480, TakeProfit: 518.4, StopLoss: 460.8,
		Qty: "10", Side: "long", AssetClass: tools.AssetClassEquity,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	lv := m.State.Level("MSFT", "")
	if lv == nil {
		t.Fatal("level vanished")
	}
	if math.Abs(lv.EntryPrice-520) > 0.01 {
		t.Fatalf("level must follow the broker's average entry, got %v", lv.EntryPrice)
	}
	if len(fb.oco) != 1 {
		t.Fatalf("a re-anchored equity must get a fresh protective pair, got %d", len(fb.oco))
	}
}

// TestReconcileSkipsAlreadyProtected stops the pass from stacking a
// second pair on a position the broker already guards.
func TestReconcileSkipsAlreadyProtected(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "NVDA", Qty: "20", Side: "long",
			AvgEntry: "210", CurrentPrice: "211", AssetClass: tools.AssetClassEquity,
		}},
		[]tools.Order{{Symbol: "NVDA", Type: "limit", Side: "sell", OrderClass: "oco"}})

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	if len(fb.oco) != 0 {
		t.Fatalf("a position with a resting oco pair must not be re-protected, placed %d", len(fb.oco))
	}
	// The level is still adopted: local bookkeeping drives the rest.
	if m.State.Level("NVDA", "") == nil {
		t.Fatal("levels should still be recorded for an already-protected position")
	}
}

// TestStaleProtectivePairIsReplaced proves the point of comparing
// prices: after a re-anchor the broker is still holding a pair at the OLD
// cost basis, so the level the dashboard shows is not the stop actually
// resting. A mismatched pair must be replaced, not merely noticed.
func TestStaleProtectivePairIsReplaced(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "AAPL", Qty: "5", Side: "long",
			AvgEntry: "320", CurrentPrice: "321", AssetClass: tools.AssetClassEquity,
		}},
		[]tools.Order{{
			Symbol: "AAPL", Side: "sell", Type: "limit", OrderClass: "oco",
			LimitPrice: "200.00", StopPrice: "150.00", ID: "stale-pair",
		}})

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	if len(fb.cancelled) == 0 {
		t.Fatal("the stale-priced pair must be cleared before a correct one is placed")
	}
	if len(fb.oco) != 1 {
		t.Fatalf("expected one replacement pair, got %d", len(fb.oco))
	}
	got := fb.oco[0]
	if math.Abs(got.TakeProfit-345.60) > 0.05 || math.Abs(got.StopLoss-307.20) > 0.05 {
		t.Fatalf("replacement must sit at the NEW entry's levels, got %+v", got)
	}
}

// TestUnreadablePairPricesAreLeftAlone proves the conservative half of
// the rule: a resting pair whose prices cannot be read is NOT churned.
// Cancelling and re-placing every tick over a missing payload field
// would leave the position repeatedly unprotected.
func TestUnreadablePairPricesAreLeftAlone(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "AAPL", Qty: "5", Side: "long",
			AvgEntry: "320", CurrentPrice: "321", AssetClass: tools.AssetClassEquity,
		}},
		[]tools.Order{{Symbol: "AAPL", Side: "sell", Type: "limit", OrderClass: "oco", ID: "opaque-pair"}})

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	if len(fb.oco) != 0 || len(fb.cancelled) != 0 {
		t.Fatalf("an unreadable but present pair must be left alone: oco=%d cancelled=%v", len(fb.oco), fb.cancelled)
	}
}

// TestReconcileCryptoIsLocalOnly proves crypto pairs get levels and NO
// broker order class — Alpaca rejects bracket/oco on crypto, and a
// placement attempt would fail the position open.
func TestReconcileCryptoIsLocalOnly(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "BTCUSD", Qty: "0.5", Side: "long",
			AvgEntry: "80000", CurrentPrice: "80100", AssetClass: tools.AssetClassCrypto,
		}}, nil)

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	lv := m.State.Level("BTC/USD", "")
	if lv == nil {
		t.Fatal("crypto position not adopted")
	}
	if !lv.LocallyEnforced() {
		t.Fatalf("crypto must be locally enforced: %+v", lv)
	}
	if len(fb.oco) != 0 {
		t.Fatalf("crypto must not get a broker order class, placed %d", len(fb.oco))
	}
}

// TestReconcileOptionPositionIsLocalOnly is the same for contracts: the
// broker has no order class for options, so the monitor must carry the
// levels and the enforce pass must act on them.
func TestReconcileOptionPositionIsLocalOnly(t *testing.T) {
	// Mark sits BELOW the take-profit so the position is still open when
	// the levels are inspected; a mark through the target is closed by the
	// very same tick (see TestOptionPositionActuallyClosesAtTarget).
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "AAPL260116C00150000", Qty: "2", Side: "long",
			AvgEntry: "10", CurrentPrice: "10.20", AssetClass: tools.AssetClassOption,
		}}, nil)

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	lv := m.State.Level("AAPL260116C00150000", "")
	if lv == nil {
		t.Fatal("option position not adopted")
	}
	if !lv.LocallyEnforced() {
		t.Fatalf("options must be locally enforced (no broker order class): %+v", lv)
	}
	if len(fb.oco) != 0 {
		t.Fatalf("options must not get a broker order class, placed %d", len(fb.oco))
	}
}

// TestOptionPositionActuallyClosesAtTarget proves the enforcement pass
// now sees options at all. The old filter returned crypto levels only,
// so an option position reached its take-profit and nothing happened.
func TestOptionPositionActuallyClosesAtTarget(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "AAPL260116C00150000", Qty: "2", Side: "long",
			AvgEntry: "10", CurrentPrice: "12", AssetClass: tools.AssetClassOption,
		}}, nil)

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	// entry 10, TP_PCT .08 -> 10.80; mark 12 is through it.
	var closed bool
	for _, p := range fb.placed {
		if p.Symbol == "AAPL260116C00150000" && p.Side == "sell" && p.Type == "market" {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("an option through its take-profit must be closed, placed=%+v", fb.placed)
	}
}

// TestShortPositionClosesWithBuy proves a short is protected the right
// way round. Equities are guarded at the broker, so the assertion is on
// the resting pair: buying to close, stop ABOVE the entry, target BELOW
// it. Selling a short at its stop would double the exposure instead of
// flattening it, which is what a direction-blind path does.
func TestShortPositionClosesWithBuy(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "TSLA", Qty: "-3", Side: "short",
			AvgEntry: "400", CurrentPrice: "500", AssetClass: tools.AssetClassEquity,
		}}, nil)

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	if len(fb.oco) != 1 {
		t.Fatalf("the short must be protected by one broker pair, got %d", len(fb.oco))
	}
	got := fb.oco[0]
	if got.Side != "buy" {
		t.Fatalf("closing a short is a buy, got %q", got.Side)
	}
	if got.StopLoss <= 400 || got.TakeProfit >= 400 {
		t.Fatalf("short pair must invert: entry 400, tp=%v sl=%v", got.TakeProfit, got.StopLoss)
	}
	for _, p := range fb.placed {
		if p.Symbol == "TSLA" && p.Side == "sell" {
			t.Fatalf("a short must never be sold to close, placed=%+v", fb.placed)
		}
	}
}

// TestRestingOrdersCancelledBeforeLocalClose proves a protective leg
// cannot survive a local exit and reopen the position on the wrong side.
func TestRestingOrdersCancelledBeforeLocalClose(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "BTCUSD", Qty: "1", Side: "long",
			AvgEntry: "80000", CurrentPrice: "99999", AssetClass: tools.AssetClassCrypto,
		}}, nil)

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	var cancelled bool
	for _, s := range fb.cancelled {
		if s == "BTC/USD" {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatalf("resting orders must be cleared before a local close, cancelled=%v", fb.cancelled)
	}
}

// TestReconcileRespectsTrackOwnership pins the bug the first version of
// this pass had: the swing book adopting a position the intraday book
// already owns, because the broker reports one aggregated position per
// symbol while the bot runs two books on it.
func TestReconcileRespectsTrackOwnership(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "BTCUSD", Qty: "0.5", Side: "long",
			AvgEntry: "50000", CurrentPrice: "56000", AssetClass: tools.AssetClassCrypto,
		}}, nil)
	if err := m.State.SetLevel(PositionLevels{
		Symbol: "BTC/USD", EntryPrice: 50000, TakeProfit: 51000, StopLoss: 49000,
		Qty: "0.5", Side: "long", Crypto: true, AssetClass: tools.AssetClassCrypto,
		Track: "intraday",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.TickTrack(context.Background(), map[string]float64{}, 200000, ""); err != nil {
		t.Fatal(err)
	}
	if m.State.Level("BTC/USD", "") != nil {
		t.Fatal("the swing pass must not adopt a symbol the intraday book owns")
	}
	if len(fb.placed) != 0 {
		t.Fatalf("the swing pass must not act on the intraday book's position, placed=%+v", fb.placed)
	}
	// The intraday owner still enforces its own levels on its own pass.
	if _, err := m.TickTrack(context.Background(), map[string]float64{}, 200000, "intraday"); err != nil {
		t.Fatal(err)
	}
	var closed bool
	for _, p := range fb.placed {
		if p.Symbol == "BTC/USD" {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("the owning book must still enforce, placed=%+v", fb.placed)
	}
}

// TestReconcileScopedToUniverse proves TPSL_PROTECT_ALL=false leaves
// holdings outside the traded universe alone.
func TestReconcileScopedToUniverse(t *testing.T) {
	set := defSettings()
	set.ProtectAllPositions = false
	set.EquitySymbols = []string{"AAPL"}
	m, fb := posMonitor(t, agents.NewKillSwitch(), set,
		[]tools.Position{{
			Symbol: "ZZZZ", Qty: "100", Side: "long",
			AvgEntry: "10", CurrentPrice: "10", AssetClass: tools.AssetClassEquity,
		}}, nil)

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	if m.State.Level("ZZZZ", "") != nil {
		t.Fatal("a symbol outside the universe must not be adopted when the scope is narrowed")
	}
	if len(fb.oco) != 0 {
		t.Fatalf("no protective order may be placed either, got %d", len(fb.oco))
	}
}

// TestProtectionIsJournaled proves the audit trail answers "why is this
// position guarded at this price" without the log files.
func TestProtectionIsJournaled(t *testing.T) {
	m, fb := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{
			Symbol: "AAPL", Qty: "5", Side: "long",
			AvgEntry: "319.69", CurrentPrice: "347.72", AssetClass: tools.AssetClassEquity,
		}}, nil)

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	recs, err := m.Journal.Records()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range recs {
		if r.Source == "strategy:protect" && strings.Contains(r.Detail, "adopted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("adoption must be journaled, records=%d", len(recs))
	}
	if len(fb.oco) != 1 {
		t.Fatalf("the journal should match a real placement, got %d", len(fb.oco))
	}
}

// TestMissingAverageEntryIsReported proves the pass refuses to invent a
// stop distance rather than silently leaving the position naked.
func TestMissingAverageEntryIsReported(t *testing.T) {
	m, _ := posMonitor(t, agents.NewKillSwitch(), defSettings(),
		[]tools.Position{{Symbol: "AAPL", Qty: "5", Side: "long", AvgEntry: ""}}, nil)

	if _, err := m.Tick(context.Background(), map[string]float64{}, 200000); err != nil {
		t.Fatal(err)
	}
	if m.State.Level("AAPL", "") != nil {
		t.Fatal("no cost basis means no honest level — nothing may be invented")
	}
	recs, _ := m.Journal.Records()
	var reported bool
	for _, r := range recs {
		if r.Source == "strategy:protect" && strings.Contains(r.Detail, "no average entry") {
			reported = true
		}
	}
	if !reported {
		t.Fatal("an unprotected position must be reported, not skipped quietly")
	}
}
