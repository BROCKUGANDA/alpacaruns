package selfheal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// fakeBroker is a scripted broker: fixed positions/orders, and a record of
// every cancellation the pass asked for.
type fakeBroker struct {
	positions []tools.Position
	orders    []tools.Order
	cancelErr error
	cancelled []string
}

func (f *fakeBroker) Positions(context.Context) ([]tools.Position, error) {
	return f.positions, nil
}

func (f *fakeBroker) OpenOrders(context.Context) ([]tools.Order, error) {
	return f.orders, nil
}

func (f *fakeBroker) Cancel(_ context.Context, id string) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelled = append(f.cancelled, id)
	return nil
}

func TestOrphanProtectiveOrderIsCancelled(t *testing.T) {
	// The dangerous state: the position was closed by hand, the
	// protective sell limit is still resting. If it fills, the account is
	// short five shares it never decided to hold.
	fb := &fakeBroker{
		positions: nil, // flat
		orders: []tools.Order{{
			ID: "orph-1", Symbol: "AAPL", Side: "sell", Type: "limit",
			OrderClass: "oco", LimitPrice: "400",
		}},
	}
	rep := Run(context.Background(), Deps{Broker: fb}, DefaultOptions())
	if len(fb.cancelled) != 1 || fb.cancelled[0] != "orph-1" {
		t.Fatalf("orphan must be cancelled, cancelled=%v report=%s", fb.cancelled, rep.Summary())
	}
	if len(rep.Actions) != 1 || rep.Actions[0].Check != "orphan-protective-order" || !rep.Actions[0].Repaired {
		t.Fatalf("unexpected actions: %+v", rep.Actions)
	}
}

func TestProtectiveOrderWithPositionIsLeftAlone(t *testing.T) {
	fb := &fakeBroker{
		positions: []tools.Position{{Symbol: "AAPL", Qty: "5"}},
		orders: []tools.Order{{
			ID: "keep-1", Symbol: "AAPL", Side: "sell", Type: "limit",
			OrderClass: "oco", LimitPrice: "400",
		}},
	}
	if rep := Run(context.Background(), Deps{Broker: fb}, DefaultOptions()); len(fb.cancelled) != 0 {
		t.Fatalf("live protection must not be touched: %v (%s)", fb.cancelled, rep.Summary())
	}
}

func TestCryptoSymbolFormsMatch(t *testing.T) {
	// The broker reports "BTCUSD" while an order may carry "BTC/USD".
	// A comparison that got this wrong would cancel the protection on a
	// position that is very much still open.
	fb := &fakeBroker{
		positions: []tools.Position{{Symbol: "BTCUSD", Qty: "0.5"}},
		orders: []tools.Order{{
			ID: "btc-1", Symbol: "BTC/USD", Side: "sell", Type: "stop", StopPrice: "40000",
		}},
	}
	if rep := Run(context.Background(), Deps{Broker: fb}, DefaultOptions()); len(fb.cancelled) != 0 {
		t.Fatalf("slash/no-slash must resolve to the same position: %v (%s)", fb.cancelled, rep.Summary())
	}
}

func TestDuplicateProtectivePairsCollapseToOne(t *testing.T) {
	newer := time.Now().UTC()
	older := newer.Add(-48 * time.Hour)
	fb := &fakeBroker{
		positions: []tools.Position{{Symbol: "AAPL", Qty: "5"}},
		orders: []tools.Order{
			{ID: "pair-old", Symbol: "AAPL", Side: "sell", Type: "limit", OrderClass: "oco", LimitPrice: "380", CreatedAt: older},
			{ID: "pair-new", Symbol: "AAPL", Side: "sell", Type: "limit", OrderClass: "oco", LimitPrice: "400", CreatedAt: newer},
		},
	}
	if rep := Run(context.Background(), Deps{Broker: fb}, DefaultOptions()); len(fb.cancelled) != 1 || fb.cancelled[0] != "pair-old" {
		t.Fatalf("must keep the newest pair and cancel the older, cancelled=%v (%s)", fb.cancelled, rep.Summary())
	}
}

func TestSinglePairLegsAreNotDuplicates(t *testing.T) {
	// A bracket shows up as a parent plus stop/limit legs. Counting those
	// as duplicates would strip a position of its protection entirely.
	now := time.Now().UTC()
	fb := &fakeBroker{
		positions: []tools.Position{{Symbol: "NVDA", Qty: "10"}},
		orders: []tools.Order{
			{ID: "parent", Symbol: "NVDA", Side: "buy", Type: "limit", OrderClass: "bracket", CreatedAt: now},
			{ID: "leg-stop", Symbol: "NVDA", Side: "sell", Type: "stop", StopPrice: "150", CreatedAt: now},
			{ID: "leg-tp", Symbol: "NVDA", Side: "sell", Type: "limit", LimitPrice: "200", CreatedAt: now},
		},
	}
	rep := Run(context.Background(), Deps{Broker: fb}, DefaultOptions())
	for _, a := range rep.Actions {
		if a.Check == "duplicate-protection" {
			t.Fatalf("one bracket is not a duplicate: %+v", a)
		}
	}
}

func TestStaleRestingEntryCancelledAfterTTL(t *testing.T) {
	fb := &fakeBroker{
		orders: []tools.Order{{
			ID: "stale-entry", Symbol: "MSFT", Side: "buy", Type: "limit",
			OrderClass: "", LimitPrice: "400", CreatedAt: time.Now().UTC().Add(-50 * time.Hour),
		}},
	}
	opts := DefaultOptions()
	opts.OrphanMaxAge = 24 * time.Hour
	rep := Run(context.Background(), Deps{Broker: fb}, opts)
	if len(fb.cancelled) != 1 || fb.cancelled[0] != "stale-entry" {
		t.Fatalf("stale entry should be cancelled, cancelled=%v report=%s", fb.cancelled, rep.Summary())
	}
}

func TestFreshEntryOrderIsKept(t *testing.T) {
	fb := &fakeBroker{
		orders: []tools.Order{{
			ID: "fresh-entry", Symbol: "MSFT", Side: "buy", Type: "limit",
			LimitPrice: "400", CreatedAt: time.Now().UTC().Add(-time.Hour),
		}},
	}
	opts := DefaultOptions()
	opts.OrphanMaxAge = 24 * time.Hour
	rep := Run(context.Background(), Deps{Broker: fb}, opts)
	if len(fb.cancelled) != 0 {
		t.Fatalf("a recent pre-order is intentional, cancelled=%v (%s)", fb.cancelled, rep.Summary())
	}
}

func TestReportOnlyChangesNothing(t *testing.T) {
	fb := &fakeBroker{
		orders: []tools.Order{{
			ID: "orph", Symbol: "AAPL", Side: "sell", Type: "stop", StopPrice: "300",
		}},
	}
	opts := DefaultOptions()
	opts.ReportOnly = true
	rep := Run(context.Background(), Deps{Broker: fb}, opts)
	if len(fb.cancelled) != 0 {
		t.Fatalf("report-only must not cancel, got %v", fb.cancelled)
	}
	if len(rep.Actions) == 0 {
		t.Fatal("report-only must still report the finding")
	}
}

func TestFailedCancelEscalatesSeverity(t *testing.T) {
	fb := &fakeBroker{
		cancelErr: os.ErrPermission,
		orders: []tools.Order{{
			ID: "orph", Symbol: "AAPL", Side: "sell", Type: "limit", OrderClass: "oco", LimitPrice: "400",
		}},
	}
	rep := Run(context.Background(), Deps{Broker: fb}, DefaultOptions())
	if len(rep.Actions) != 1 || rep.Actions[0].Severity != "critical" || rep.Actions[0].Repaired {
		t.Fatalf("a failed cancel of an orphan must escalate, not hide: %+v", rep.Actions)
	}
}

func TestUnavailableBrokerReportsSkipNotClean(t *testing.T) {
	rep := Run(context.Background(), Deps{Broker: brokerErr{}}, DefaultOptions())
	if rep.Clean() {
		t.Fatal("a pass that could not see the account is not a clean pass")
	}
	if len(rep.Errors) == 0 {
		t.Fatal("the skip must be recorded")
	}
}

type brokerErr struct{}

func (brokerErr) Positions(context.Context) ([]tools.Position, error) {
	return nil, os.ErrClosed
}
func (brokerErr) OpenOrders(context.Context) ([]tools.Order, error) { return nil, nil }
func (brokerErr) Cancel(context.Context, string) error               { return nil }

func TestBadStateFileIsReportedNotDeleted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "strategy-state.json")
	if err := os.WriteFile(p, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := Run(context.Background(), Deps{Broker: &fakeBroker{}, StatePath: p}, DefaultOptions())
	var found bool
	for _, a := range rep.Actions {
		if a.Check == "state-file" {
			found = true
			if a.Repaired {
				t.Fatal("the healer must not silently delete the only record of intended stop levels")
			}
			if a.Severity != "critical" {
				t.Fatalf("an unreadable state file is not cosmetic: %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("unreadable state must be reported")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("the file must still be there for an operator to inspect")
	}
}

func TestTruncatedJournalTailDetected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "trades.jsonl")
	if err := os.WriteFile(p, []byte("{\"kind\":\"fill\"}\n{\"kind\":\"fill\",\"qty\":\"5"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := Run(context.Background(), Deps{Broker: &fakeBroker{}, JournalPath: p}, DefaultOptions())
	var found bool
	for _, a := range rep.Actions {
		if a.Check == "journal-tail" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a cut-off final line must be reported: %s", rep.Summary())
	}
}

func TestHaltLiftIsOptIn(t *testing.T) {
	var resetCalled bool
	deps := Deps{
		Broker:  &fakeBroker{},
		Halted:  func() bool { return true },
		Equity:  func() (float64, error) { return 150000, nil },
		ResetPeak: func(float64) error { resetCalled = true; return nil },
	}
	rep := Run(context.Background(), deps, DefaultOptions())
	if resetCalled {
		t.Fatal("lifting a halt automatically releases the operator's brake")
	}
	var reported bool
	for _, a := range rep.Actions {
		if a.Check == "halt-still-engaged" && strings.Contains(a.Detail, "SELFHEAL_LIFT_HALT") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("the halt must be reported with how to release it: %s", rep.Summary())
	}

	opts := DefaultOptions()
	opts.HaltLiftEnabled = true
	rep = Run(context.Background(), deps, opts)
	if !resetCalled {
		t.Fatalf("with the opt-in the peak must be re-based: %s", rep.Summary())
	}
}
