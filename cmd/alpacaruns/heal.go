package main

// heal.go: the auto loop's side of the self-heal pass.
//
// The checks themselves live in the selfheal package; this file wires
// them to the live account and decides the cadence. Two details are
// worth stating because they are the difference between a healer and a
// second trader:
//
//   - the pass may only CANCEL. It never places an order, so the worst
//     thing a bug in here can do is leave a position unprotected the way
//     it already was, not invent exposure;
//   - every action is journaled. A repair that nobody can reconstruct
//     after the fact is indistinguishable from the bot acting on its own.

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/selfheal"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// selfHealBroker adapts *tools.Client to the small surface the pass needs.
type selfHealBroker struct {
	client *tools.Client
}

func (b selfHealBroker) Positions(ctx context.Context) ([]tools.Position, error) {
	if b.client == nil {
		return nil, context.Canceled
	}
	return b.client.GetPositions(ctx)
}

func (b selfHealBroker) OpenOrders(ctx context.Context) ([]tools.Order, error) {
	if b.client == nil {
		return nil, context.Canceled
	}
	return b.client.ListOrders(ctx, "open", 500)
}

func (b selfHealBroker) Cancel(ctx context.Context, orderID string) error {
	if b.client == nil {
		return context.Canceled
	}
	// Confirmed, not acknowledged: a 204 from DELETE does not mean the
	// order stopped resting (measured on the paper account — it still
	// listed as accepted). Marking a repair "done" on the status code is
	// how a healer reports an orphan it left in place.
	return b.client.CancelOrderConfirmed(ctx, orderID, 3*time.Second)
}

// maybeHeal runs one pass when the tick cadence says so, journals every
// action, and persists the report for the API. A pass that cannot read
// the account reports itself as such; it is never treated as clean.
func (l *autoLoop) maybeHeal(ctx context.Context) {
	if !l.cfg.SelfHealEnabled || l.healDeps.Broker == nil {
		return
	}
	if l.tickNum < 1 || (l.healAtTick != 0 && l.tickNum-l.healAtTick < l.healEvery) {
		return
	}
	rep := selfheal.Run(ctx, l.healDeps, l.healOpts)
	l.healAtTick = l.tickNum
	l.healLast = rep
	if l.healStore != nil {
		if err := l.healStore.Save(rep); err != nil {
			log.Printf("[heal] report not persisted: %v", err)
		}
	}
	if rep.Clean() {
		log.Printf("[heal] tick %d: clean", l.tickNum)
		return
	}
	log.Printf("[heal] tick %d: %s", l.tickNum, rep.Summary())
	for _, a := range rep.Actions {
		l.journalHeal(a)
	}
	for _, e := range rep.Errors {
		l.journalHeal(selfheal.Action{
			Check: "unavailable", Severity: "medium",
			Detail: "check skipped: " + e,
		})
	}
}

// journalHeal writes one repair (or finding) to the trade journal under
// source "selfheal:<check>", so the dashboard's decision feed shows it
// next to the trading decisions that caused it.
func (l *autoLoop) journalHeal(a selfheal.Action) {
	if l.journal == nil {
		return
	}
	_ = l.journal.Append(pnl.Record{
		Kind:   pnl.KindDecision,
		Source: "selfheal:" + a.Check,
		Risk:   severityRisk(a.Severity),
		Detail: a.Detail,
	})
}

// severityRisk maps a finding's severity onto the journal's risk field so
// an unrepaired high-severity state is visible in the same column an
// operator already watches.
func severityRisk(s string) string {
	switch s {
	case "critical", "high":
		return "HEAL_REQUIRED"
	case "medium", "low":
		return "HEAL_NOTED"
	default:
		return "HEALED"
	}
}

// resetDrawdownPeak re-bases the high-water mark the TOTAL drawdown halt
// is measured against. Two limits of this repair are worth stating: the
// kill switch itself is a one-way latch inside this process, so re-basing
// only stops the NEXT boot from inheriting the old peak; and the daily /
// weekly halts are computed from the journal's realized fills, which a
// bookkeeping re-base must not touch — erasing those would erase the
// losses that earned the halt.
func (l *autoLoop) resetDrawdownPeak(equity float64) error {
	if l.exec == nil || l.exec.State == nil {
		return errors.New("no state store wired")
	}
	if equity <= 0 {
		return errors.New("refusing to re-base the drawdown peak on a non-positive equity")
	}
	st, err := l.exec.State.Load()
	if err != nil {
		return err
	}
	st.PeakEquity = equity
	return l.exec.State.Save(st)
}

// accountEquity reads the live equity for the halt check. The heal pass
// must not place or cancel on the strength of a stale number, so it
// fetches rather than reusing the tick's cached account snapshot.
func (l *autoLoop) accountEquity() (float64, error) {
	if l.client == nil {
		return 0, context.Canceled
	}
	acct, err := l.client.GetAccount(context.Background())
	if err != nil {
		return 0, err
	}
	eq, err := strconv.ParseFloat(acct.Equity, 64)
	if err != nil {
		return 0, err
	}
	return eq, nil
}
