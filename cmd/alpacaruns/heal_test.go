package main

// heal_test.go covers the loop's side of the self-heal pass: the cadence,
// the journaling, and the two deps the pass needs from the bot (peak
// re-base and live equity). The checks themselves are covered in the
// selfheal package.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/selfheal"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// fakeHealBroker is a scripted broker for the pass.
type fakeHealBroker struct {
	positions []tools.Position
	orders    []tools.Order
	posErr    error
	ordErr    error
	cancelled []string
	cancelErr error
}

func (f *fakeHealBroker) Positions(context.Context) ([]tools.Position, error) {
	if f.posErr != nil {
		return nil, f.posErr
	}
	return f.positions, nil
}

func (f *fakeHealBroker) OpenOrders(context.Context) ([]tools.Order, error) {
	if f.ordErr != nil {
		return nil, f.ordErr
	}
	return f.orders, nil
}

func (f *fakeHealBroker) Cancel(_ context.Context, id string) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelled = append(f.cancelled, id)
	return nil
}

func healLoop(t *testing.T, broker selfheal.Broker) (*autoLoop, *pnl.Journal, *fakeHealBroker) {
	t.Helper()
	dir := t.TempDir()
	j, err := pnl.Open(filepath.Join(dir, "trades.jsonl"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	fb, _ := broker.(*fakeHealBroker)
	st := strategy.NewStateStore(j.Path())
	loop := &autoLoop{
		cfg:       &config.Config{SelfHealEnabled: true, SelfHealEveryTicks: 3},
		journal:   j,
		client:    nil,
		healStore: selfheal.NewReportStore(j.Path()),
		healEvery: 3,
		healDeps: selfheal.Deps{
			Broker:      broker,
			StatePath:   st.Path(),
			JournalPath: j.Path(),
		},
		healOpts: selfheal.Options{OrphanMaxAge: 24 * time.Hour},
	}
	return loop, j, fb
}

func order(sym, id, oclass, otype string, created time.Time) tools.Order {
	return tools.Order{ID: id, Symbol: sym, Side: "sell", Type: otype,
		OrderClass: oclass, Status: "accepted", CreatedAt: created}
}

// A protective order resting over no position is a short sale waiting to
// fill; the pass must cancel it and the loop must write that down.
func TestHealCancelsOrphanAndJournalsIt(t *testing.T) {
	loop, j, fb := healLoop(t, &fakeHealBroker{
		orders: []tools.Order{order("AAPL", "orph-1", "oco", "limit", time.Now().UTC())},
	})
	loop.tickNum = 1
	loop.maybeHeal(context.Background())

	if len(fb.cancelled) != 1 || fb.cancelled[0] != "orph-1" {
		t.Fatalf("orphan not cancelled: %v", fb.cancelled)
	}
	recs, err := j.Records()
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	var found *pnl.Record
	for i := range recs {
		if strings.HasPrefix(recs[i].Source, "selfheal:") {
			found = &recs[i]
		}
	}
	if found == nil {
		t.Fatal("the repair was not journaled")
	}
	if found.Kind != pnl.KindDecision {
		t.Fatalf("journalled heal action has kind %q", found.Kind)
	}
	if found.Source != "selfheal:orphan-protective-order" {
		t.Fatalf("source = %q", found.Source)
	}
	if found.Risk != "HEAL_REQUIRED" {
		t.Fatalf("a high-severity orphan should read HEAL_REQUIRED, got %q", found.Risk)
	}
	// The report must reach the store the dashboard API reads.
	back, err := loop.healStore.Load()
	if err != nil {
		t.Fatalf("report load: %v", err)
	}
	if len(back.Actions) == 0 {
		t.Fatal("report persisted with no actions")
	}
}

// The pass runs on a tick cadence, not every tick — the checks read the
// whole order list and the states they look for do not move between two
// polls.
func TestHealRunsOnceThenOnCadence(t *testing.T) {
	loop, _, fb := healLoop(t, &fakeHealBroker{})
	seen := 0
	loop.healDeps.Broker = countingBroker{inner: fb, n: &seen}

	for i := int64(1); i <= 7; i++ {
		loop.tickNum = i
		loop.maybeHeal(context.Background())
	}
	// Ticks 1, 4, 7.
	if seen != 3 {
		t.Fatalf("heal ran %d times across 7 ticks at every-3 cadence, want 3", seen)
	}
}

type countingBroker struct {
	inner *fakeHealBroker
	n     *int
}

func (c countingBroker) Positions(ctx context.Context) ([]tools.Position, error) {
	*c.n++
	return c.inner.Positions(ctx)
}
func (c countingBroker) OpenOrders(ctx context.Context) ([]tools.Order, error) {
	return c.inner.OpenOrders(ctx)
}
func (c countingBroker) Cancel(ctx context.Context, id string) error { return c.inner.Cancel(ctx, id) }

// A pass that cannot read the account is not a clean pass, and the loop
// must journal the gap instead of logging "clean".
func TestHealReportsUnavailableAsFindings(t *testing.T) {
	loop, j, _ := healLoop(t, &fakeHealBroker{posErr: errors.New("401 unauthorized")})
	loop.tickNum = 1
	loop.maybeHeal(context.Background())

	recs, _ := j.Records()
	if len(recs) == 0 {
		t.Fatal("a failed pass journalled nothing")
	}
	var ok bool
	for _, r := range recs {
		if strings.Contains(r.Detail, "positions unavailable") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("the skipped check was not recorded: %+v", recs)
	}
}

// Report-only mode must look and never touch.
func TestHealReportOnlyCancelsNothing(t *testing.T) {
	loop, _, fb := healLoop(t, &fakeHealBroker{
		orders: []tools.Order{order("TSLA", "orph-2", "", "stop", time.Now().UTC())},
	})
	loop.healOpts.ReportOnly = true
	loop.tickNum = 1
	loop.maybeHeal(context.Background())
	if len(fb.cancelled) != 0 {
		t.Fatalf("report-only cancelled: %v", fb.cancelled)
	}
}

// Disabled config means the pass never runs.
func TestHealDisabledDoesNotRun(t *testing.T) {
	loop, _, fb := healLoop(t, &fakeHealBroker{
		orders: []tools.Order{order("AMD", "orph-3", "bracket", "stop", time.Now().UTC())},
	})
	loop.cfg.SelfHealEnabled = false
	loop.tickNum = 1
	loop.maybeHeal(context.Background())
	if len(fb.cancelled) != 0 {
		t.Fatalf("disabled heal still acted: %v", fb.cancelled)
	}
}

// A tick counter of zero means the loop has not started; the pass must not
// fire before then with a half-built dependency set.
func TestHealSkipsBeforeFirstTick(t *testing.T) {
	loop, _, fb := healLoop(t, &fakeHealBroker{
		orders: []tools.Order{order("AMD", "orph-4", "oco", "stop", time.Now().UTC())},
	})
	loop.tickNum = 0
	loop.maybeHeal(context.Background())
	if len(fb.cancelled) != 0 {
		t.Fatal("heal ran before the first tick")
	}
}

// The peak re-base is the only lever the pass has against a stuck halt,
// and it must refuse to write a nonsense peak.
func TestResetDrawdownPeak(t *testing.T) {
	dir := t.TempDir()
	journalPath := filepath.Join(dir, "trades.jsonl")
	st := strategy.NewStateStore(journalPath)
	loop := &autoLoop{exec: &strategy.Executor{State: st}}

	if err := loop.resetDrawdownPeak(-5); err == nil {
		t.Fatal("re-based the peak on a negative equity")
	}
	if err := loop.resetDrawdownPeak(0); err == nil {
		t.Fatal("re-based the peak on zero equity")
	}
	if err := loop.resetDrawdownPeak(50000); err != nil {
		t.Fatalf("resetDrawdownPeak: %v", err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.PeakEquity != 50000 {
		t.Fatalf("peak not written: %v", got.PeakEquity)
	}
	// No state store wired: refuse rather than panic.
	if err := (&autoLoop{}).resetDrawdownPeak(100); err == nil {
		t.Fatal("reset succeeded with no state store")
	}
}

// A cancel that fails at the broker must be visible in the report as a
// critical finding, not swallowed.
func TestHealCancelFailureIsRecorded(t *testing.T) {
	loop, j, _ := healLoop(t, &fakeHealBroker{
		orders:    []tools.Order{order("NVDA", "orph-5", "oco", "limit", time.Now().UTC())},
		cancelErr: errors.New("422 unprocessable"),
	})
	loop.tickNum = 1
	loop.maybeHeal(context.Background())
	recs, _ := j.Records()
	var seen bool
	for _, r := range recs {
		if strings.Contains(r.Detail, "CANCEL FAILED") {
			seen = true
		}
	}
	if !seen {
		t.Fatal("a failed cancel was not journaled")
	}
}

// severityRisk is the column an operator already reads; a high finding
// must not look like a routine note.
func TestSeverityRiskMapping(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"critical", "HEAL_REQUIRED"},
		{"high", "HEAL_REQUIRED"},
		{"medium", "HEAL_NOTED"},
		{"low", "HEAL_NOTED"},
		{"", "HEALED"},
	} {
		if got := severityRisk(tc.in); got != tc.want {
			t.Errorf("severityRisk(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The adapted broker must refuse cleanly when no client is wired — a nil
// client would otherwise panic inside the recover barrier and turn into a
// "clean" report.
func TestSelfHealBrokerWithoutClient(t *testing.T) {
	b := selfHealBroker{}
	ctx := context.Background()
	if _, err := b.Positions(ctx); err == nil {
		t.Fatal("Positions succeeded with no client")
	}
	if _, err := b.OpenOrders(ctx); err == nil {
		t.Fatal("OpenOrders succeeded with no client")
	}
	if err := b.Cancel(ctx, "x"); err == nil {
		t.Fatal("Cancel succeeded with no client")
	}
}

// Keep the kill switch reference honest: the heal pass reports a halt but
// only re-bases when it is allowed to.
func TestHealHaltCheckNeedsWiring(t *testing.T) {
	loop, j, _ := healLoop(t, &fakeHealBroker{})
	kill := agents.NewKillSwitch()
	kill.Halt()
	loop.exec = &strategy.Executor{State: strategy.NewStateStore(j.Path()), Kill: kill}
	loop.healDeps.Halted = kill.Halted
	loop.healDeps.Equity = func() (float64, error) { return 1000, nil }
	loop.healDeps.ResetPeak = func(float64) error { return nil }
	loop.tickNum = 1
	loop.maybeHeal(context.Background())

	rep, err := loop.healStore.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var halted bool
	for _, a := range rep.Actions {
		if a.Check == "halt-still-engaged" {
			halted = true
			if a.Repaired {
				t.Fatal("a halt was lifted with SELFHEAL_LIFT_HALT off")
			}
		}
	}
	if !halted {
		t.Fatalf("an engaged halt was not reported: %+v", rep.Actions)
	}

	// With lifting enabled, the peak is re-based.
	loop.healAtTick = 0
	loop.healOpts.HaltLiftEnabled = true
	loop.tickNum = 5
	loop.maybeHeal(context.Background())
	rep, _ = loop.healStore.Load()
	var repaired bool
	for _, a := range rep.Actions {
		if a.Check == "halt-still-engaged" && a.Repaired {
			repaired = true
		}
	}
	if !repaired {
		t.Fatalf("halt-lift enabled but nothing was re-based: %+v", rep.Actions)
	}
}
