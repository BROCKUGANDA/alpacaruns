package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/a2a"
	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
)

// processA2A evaluates the signals waiting in the agent-to-agent bridge.
//
// Every signal goes through the SAME deterministic risk gate as the
// bot's own decisions — the bridge never places an order on its own.
// The agent's ramp position decides what happens next:
//
//	off    the signal is not even queued (the ingress refuses it)
//	shadow the signal is scored and journaled, but no order is placed;
//	        the shadow streak is what later earns the right to ramp on
//	on     the signal is executed through the normal executor
//
// The result of every evaluation (approved or not) is fed back into
// the bridge so the shadow streak reflects reality.
func (l *autoLoop) processA2A(ctx context.Context, pfv float64) {
	if l.bridge == nil {
		return
	}
	sigs, err := l.bridge.Drain(time.Now().UTC())
	if err != nil {
		log.Printf("[a2a] drain failed: %v", err)
		return
	}
	if len(sigs) == 0 {
		return
	}
	consumed := make([]string, 0, len(sigs))
	for _, sig := range sigs {
		l.evaluateA2A(ctx, sig, pfv)
		consumed = append(consumed, sig.ID)
	}
	// Acknowledge everything this tick evaluated. Drain is at-least-once,
	// so without this a still-valid signal is re-executed on every tick
	// until it expires — three buys for one 15-minute signal at a 5-minute
	// poll, and a shadow streak that advances itself.
	if len(consumed) > 0 {
		n, err := l.bridge.Ack(consumed...)
		if err != nil {
			log.Printf("[a2a] ack failed for %d signal(s); they will be re-evaluated: %v", len(consumed), err)
			return
		}
		if n > 0 {
			log.Printf("[a2a] consumed %d signal(s) from the bridge queue", n)
		}
	}
}

// evaluateA2A handles one inbound signal.
func (l *autoLoop) evaluateA2A(ctx context.Context, sig a2a.Signal, pfv float64) {
	mode, err := l.bridge.ModeOf(sig.Agent)
	if err != nil {
		log.Printf("[a2a] %s: ramp state unavailable: %v", sig.Agent, err)
		return
	}
	if mode == a2a.ModeOff {
		l.bridge.RecordOutcome(sig.Agent, false, "agent is ramped off")
		return
	}
	// A halt closes new risk to peer agents exactly as it does to the
	// bot's own signals. The refusal is fed back into the bridge so the
	// agent's shadow streak does not silently advance while the account is
	// frozen.
	if l.exec != nil && l.exec.Kill != nil && l.exec.Kill.Halted() {
		l.journalA2A(sig, "REJECTED", "kill switch halted: no new exposure accepted")
		l.bridge.RecordOutcome(sig.Agent, false, "halted: no new exposure accepted")
		return
	}

	side := strings.ToLower(strings.TrimSpace(sig.Side))
	if side != "buy" && side != "sell" {
		l.bridge.RecordOutcome(sig.Agent, false, "unsupported side "+sig.Side)
		return
	}
	if side == "sell" {
		// Sells are exits, not new exposure: they are journalled and
		// handed to the same exit path the bot uses. The ramp still
		// governs whether they are acted on.
		l.journalA2A(sig, "REJECTED", "sell signals are journalled only")
		l.bridge.RecordOutcome(sig.Agent, false, "sell signals are not executed by the bridge")
		return
	}

	dec := strategy.Decision{
		Symbol: sig.Symbol,
		Signal: strategy.SignalBuy,
		Reason: "a2a: " + sig.Reason,
	}
	dec.Sizing = l.set.SizingFor(sig.Symbol, l.cfg.MaxPositionUSD)

	// Learning applies to inbound signals as well: a symbol the bot has
	// watched lose repeatedly stays quarantined no matter which agent
	// asked for it, and a peer's confidence must clear the same bar the
	// bot raised for itself.
	if ok, why := l.learningGate(sig.Symbol, sig.Confidence); !ok {
		l.journalA2A(sig, "REJECTED", "learning: "+why)
		l.bridge.RecordOutcome(sig.Agent, false, why)
		return
	}

	// The agent's own size wins, but never above the per-position cap.
	budget := 0.0
	if n, err := strconv.ParseFloat(strings.TrimSpace(sig.Notional), 64); err == nil && n > 0 {
		budget = n
	}
	if q, err := strconv.ParseFloat(strings.TrimSpace(sig.Qty), 64); err == nil && q > 0 {
		// A qty signal is converted into a budget at the mark price.
		if px, err := l.engine.PriceOf(ctx, sig.Symbol); err == nil && px > 0 {
			if budget == 0 || q*px < budget {
				budget = q * px
			}
		}
	}
	if budget > l.cfg.MaxPositionUSD {
		l.journalA2A(sig, "REJECTED", fmt.Sprintf("requested $%.2f exceeds the per-position cap $%.2f",
			budget, l.cfg.MaxPositionUSD))
		l.bridge.RecordOutcome(sig.Agent, false, "signal exceeds the per-position cap")
		return
	}
	if budget <= 0 {
		l.journalA2A(sig, "REJECTED", "signal carried no usable size")
		l.bridge.RecordOutcome(sig.Agent, false, "signal carried no usable size")
		return
	}

	plan, err := l.engine.PlanEntry(ctx, dec, pfv)
	if err != nil {
		l.journalA2A(sig, "REJECTED", "plan failed: "+err.Error())
		l.bridge.RecordOutcome(sig.Agent, false, "plan failed: "+err.Error())
		return
	}
	// Override the sized quantity with the agent's requested budget.
	plan.Budget = budget
	// The broker intent is keyed to the SIGNAL id, not to a fresh random
	// one: if this tick dies after the order is submitted and the signal is
	// redelivered, the broker refuses the second attempt as a duplicate
	// client_order_id instead of filling it twice.
	plan.IntentID = fmt.Sprintf("alp-a2a-%s", sanitizeForOrderID(sig.ID))
	if plan.Price > 0 {
		plan.Qty = int(budget / plan.Price)
		if plan.Qty < 1 {
			l.journalA2A(sig, "REJECTED", "budget is below one share")
			l.bridge.RecordOutcome(sig.Agent, false, "budget is below one share")
			return
		}
		plan.Notional = 0
	}

	// The deterministic risk gate — the same one every other path uses.
	// Learning has already had its say on size and stop: an agent asking
	// for more than the learner allows for this class does not get it.
	if whyPlan, okPlan := l.learningPlan(&plan); !okPlan {
		l.journalA2A(sig, "REJECTED", "learning: "+whyPlan)
		l.bridge.RecordOutcome(sig.Agent, false, whyPlan)
		return
	} else if whyPlan != "" {
		l.journalA2A(sig, "APPROVED", whyPlan)
	}
	verdict := l.exec.Validator.Validate(riskProposalFor(plan, sig.Confidence))
	if !verdict.Approved {
		l.journalA2A(sig, "REJECTED", verdict.String())
		l.bridge.RecordOutcome(sig.Agent, false, verdict.String())
		return
	}

	if mode == a2a.ModeShadow {
		l.journalA2A(sig, "APPROVED", fmt.Sprintf(
			"shadow: would buy %s qty=%d @~%.4f notional=%.2f (no order placed)",
			plan.Symbol, plan.Qty, plan.Price, verdict.Notional))
		l.bridge.RecordOutcome(sig.Agent, true, "shadow evaluation approved")
		return
	}

	// Ramp is on: execute through the normal executor.
	if l.dryRun {
		l.journalA2A(sig, "APPROVED", fmt.Sprintf(
			"dry-run: would buy %s qty=%d @~%.4f", plan.Symbol, plan.Qty, plan.Price))
		l.bridge.RecordOutcome(sig.Agent, true, "dry-run")
		return
	}
	if err := l.submitEntry(ctx, plan, sig.Confidence); err != nil {
		l.journalA2A(sig, "REJECTED", "entry failed: "+err.Error())
		l.bridge.RecordOutcome(sig.Agent, false, "entry failed: "+err.Error())
		return
	}
	l.journalA2A(sig, "APPROVED", fmt.Sprintf("executed: buy %s qty=%d @~%.4f",
		plan.Symbol, plan.Qty, plan.Price))
	l.bridge.RecordOutcome(sig.Agent, true, "executed")
}

// journalA2A writes the evaluation into the trade log so every inbound
// signal leaves an audit trail, accepted or rejected.
func (l *autoLoop) journalA2A(sig a2a.Signal, risk string, detail string) {
	if l.journal == nil {
		return
	}
	conf := sig.Confidence
	// The peer's own text is attacker-controlled. It is journalled inside an
	// untrusted envelope (length-capped, control characters stripped) so it
	// cannot pose as a system line in a log a human or a model later reads.
	// If it looks like an instruction aimed at the model, say so.
	reason := agents.WrapUntrusted("peer-reason", sig.Reason)
	if agents.LooksLikeInjection(sig.Reason) {
		reason += " [FLAGGED: instruction-like text from a peer]"
		log.Printf("[a2a] %s signal %s carries instruction-like text; flagged in the journal", sig.Agent, sig.ID)
	}
	_ = l.journal.Append(pnl.Record{
		Kind:       pnl.KindDecision,
		Source:     "a2a:" + sig.Agent,
		Symbol:     sig.Symbol,
		Side:       strings.ToLower(strings.TrimSpace(sig.Side)),
		Risk:       risk,
		Confidence: &conf,
		Detail:     fmt.Sprintf("a2a signal %s %s: %s %s", sig.ID, sig.Symbol, detail, reason),
	})
}

// riskProposalFor builds the risk-gate proposal for an A2A entry.
func riskProposalFor(plan strategy.EntryPlan, confidence float64) risk.Proposal {
	return risk.Proposal{
		Symbol:      plan.Symbol,
		Side:        "buy",
		Qty:         strconv.Itoa(plan.Qty),
		Notional:    strconv.FormatFloat(plan.Budget, 'f', 2, 64),
		Confidence:  &confidence,
		OrderType:   "market",
		TimeInForce: "day",
	}
}
