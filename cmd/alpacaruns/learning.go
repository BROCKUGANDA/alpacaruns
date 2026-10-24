package main

// learning.go: the loop's side of the learner — cadence, persistence and
// the narrow place where a learned adjustment is allowed to touch an
// entry.
//
// The learning package decides WHAT the bot should conclude from its
// closed trades. This file decides WHEN it gets to say so, and — more
// importantly — the only three things it is allowed to change about a
// live entry: raise the score bar, shrink the size, tighten the stop.
// Everything else about a trade stays exactly as the operator configured
// it, because a learner that can also add risk is not a learner, it is a
// second trader.

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/learning"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
)

// learningLedger holds the engine, the persisted snapshot, and the
// cadence counter. It is safe to read from the API while the loop ticks.
type learningLedger struct {
	eng   *learning.Engine
	store *learning.Store

	mu  sync.Mutex
	adj learning.Adjustments
	// atSamples is the sample count behind adj. Recomputation is driven by
	// NEW closed trades, not by the clock: a tick that learned nothing
	// should not re-derive the same number and re-log it.
	atSamples int
}

// learningConfig maps the operator's bounds onto the engine's.
func learningConfig(cfg *config.Config) learning.Config {
	c := learning.DefaultConfig()
	c.Enabled = cfg.LearnEnabled
	if cfg.LearnMinTrades > 0 {
		c.MinTrades = cfg.LearnMinTrades
	}
	if cfg.LearnEveryTrades > 0 {
		c.EveryTrades = cfg.LearnEveryTrades
	}
	if cfg.LearnMinSizeMult > 0 {
		c.MinSizeMultiplier = cfg.LearnMinSizeMult
	}
	if cfg.LearnMaxTightenMult > 0 {
		c.MaxTightenFactor = cfg.LearnMaxTightenMult
	}
	if cfg.LearnQuarantineWinPct > 0 {
		c.QuarantineWinRate = cfg.LearnQuarantineWinPct
	}
	if cfg.LearnQuarantineTrades > 0 {
		c.QuarantineTrades = cfg.LearnQuarantineTrades
	}
	if cfg.LearnMinPerClass > 0 {
		c.MinSamplesPerClass = cfg.LearnMinPerClass
	}
	if cfg.LearnScoreBuckets > 0 {
		c.ScoreBuckets = cfg.LearnScoreBuckets
	}
	return c
}

// newLearningLedger builds the engine and restores the last snapshot from
// disk so a restart resumes with what the bot already learned instead of
// re-earning it from tick one.
func newLearningLedger(cfg *config.Config, journalPath string) (*learningLedger, error) {
	eng, err := learning.NewEngine(learningConfig(cfg))
	if err != nil {
		return nil, err
	}
	l := &learningLedger{eng: eng, store: learning.NewStore(journalPath)}
	adj, err := l.store.Load()
	if err != nil {
		// A damaged snapshot is not fatal: the samples are the source of
		// truth and the next calibration rewrites this file.
		log.Printf("[learn] WARNING: stored adjustments unreadable (%v); recomputing from the journal", err)
	}
	l.adj = adj
	l.atSamples = adj.SamplesSeen
	return l, nil
}

// maybeCalibrate recomputes when enough new closed trades have arrived,
// persists the result, and reports whether anything was recomputed.
func (l *learningLedger) maybeCalibrate(samples []learning.Sample, baseFloor float64, now time.Time) (learning.Adjustments, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	c := l.eng.Config()
	if len(samples) < c.MinTrades {
		return l.adj, false, nil
	}
	if l.atSamples > 0 && len(samples) < l.atSamples+c.EveryTrades {
		return l.adj, false, nil
	}
	adj := l.eng.Calibrate(samples, baseFloor, now)
	if err := l.store.Save(adj); err != nil {
		return l.adj, false, fmt.Errorf("persist learning state: %w", err)
	}
	l.adj = adj
	l.atSamples = len(samples)
	return adj, true, nil
}

// snapshot returns the adjustments currently in force.
func (l *learningLedger) snapshot() learning.Adjustments {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.adj
}

// gate is the entry check: a quarantined symbol is blocked outright, and
// a raised score floor applies to everything else. baseFloor is the
// operator's configured floor; learning can only push it upward.
func (l *learningLedger) gate(symbol string, score, baseFloor float64) (bool, string) {
	adj := l.snapshot()
	if !adj.Active {
		return true, ""
	}
	if q, ok := adj.IsQuarantined(symbol); ok {
		return false, fmt.Sprintf("learning quarantine: %s", q.Reason)
	}
	floor := baseFloor + adj.ScoreFloorAdd
	if adj.ScoreFloorAdd > 0 && score < floor {
		return false, fmt.Sprintf(
			"learning raised the entry bar to %.3f (from %.3f): this signal scored %.3f",
			floor, baseFloor, score)
	}
	return true, ""
}

// applyToPlan shrinks the plan's size and tightens its stop by the
// learned multipliers for its class. It never enlarges either, and it
// returns a reason when the shrink leaves nothing tradeable — the
// correct outcome of "this class has been losing, so don't".
func (l *learningLedger) applyToPlan(p *strategy.EntryPlan, class string) (string, bool) {
	adj := l.snapshot()
	if !adj.Active || p == nil {
		return "", true
	}
	sizeMult := adj.SizeMultiplierFor(class)
	stopMult := adj.StopMultiplierFor(class)
	if sizeMult == 1 && stopMult == 1 {
		return "", true
	}
	var notes []string
	if sizeMult < 1 {
		if p.Notional > 0 {
			p.Notional = round2(p.Notional * sizeMult)
			p.Budget = p.Notional
			if p.Notional < 1 {
				return fmt.Sprintf("learning shrank the $%.2f notional to nothing (x%.2f)", p.Notional, sizeMult), false
			}
			notes = append(notes, fmt.Sprintf("notional x%.2f -> $%.2f", sizeMult, p.Notional))
		} else if p.Qty > 0 {
			nq := int(math.Floor(float64(p.Qty) * sizeMult))
			if nq < 1 {
				return fmt.Sprintf("learning shrank the %d-unit position below one unit (x%.2f)", p.Qty, sizeMult), false
			}
			notes = append(notes, fmt.Sprintf("qty x%.2f: %d -> %d", sizeMult, p.Qty, nq))
			p.Qty = nq
			p.Budget = round2(float64(p.Qty) * p.Price)
		}
	}
	if stopMult < 1 && p.Brackets.StopLoss > 0 && p.Price > 0 {
		short := p.Brackets.StopLoss > p.Price
		old := p.Brackets.StopLoss
		if short {
			p.Brackets.StopLoss = round4(p.Price + (old-p.Price)*stopMult)
		} else {
			p.Brackets.StopLoss = round4(p.Price - (p.Price-old)*stopMult)
		}
		notes = append(notes, fmt.Sprintf("stop x%.2f: %.4f -> %.4f", stopMult, old, p.Brackets.StopLoss))
	}
	if len(notes) == 0 {
		return "", true
	}
	return "learning applied: " + strings.Join(notes, ", "), true
}

// refreshAndLearn rebuilds the trade ledger from the journal and lets the
// learner speak. The rebuild is what makes this work after a restart or a
// manual close in the dashboard: the fills are the record, and both the
// per-symbol stats and the learning samples are derived from them rather
// than accumulated in memory.
func (l *autoLoop) refreshAndLearn(now time.Time) {
	if l.stats == nil || l.journal == nil {
		return
	}
	if err := l.stats.refreshFromJournalWith(l.journal, l.classResolver()); err != nil {
		log.Printf("[learn] ledger rebuild failed: %v", err)
		return
	}
	if l.learn == nil {
		return
	}
	adj, changed, err := l.learn.maybeCalibrate(l.stats.learningSamples(), l.cfg.FactorMinScore, now)
	if err != nil {
		log.Printf("[learn] calibration failed: %v", err)
		return
	}
	if !changed {
		return
	}
	recordNotes(l.journal, adj)
	// The samples and aggregates are rebuilt from the journal every tick,
	// so persisting here only matters at a calibration boundary: it saves
	// the ledger that just earned a new conclusion.
	if err := l.stats.persist(); err != nil {
		log.Printf("[learn] ledger persist failed: %v", err)
	}
}

// recordNotes journals and logs what the learner just concluded. Each
// note is one auditable line: the dashboard and the log show the same
// reasoning the entry gate acted on.
func recordNotes(j *pnl.Journal, adj learning.Adjustments) {
	for _, n := range adj.Notes {
		log.Printf("[learn] %s", n)
		if j == nil {
			continue
		}
		_ = j.Append(pnl.Record{
			Kind:   pnl.KindDecision,
			Source: "learning",
			Risk:   "APPROVED",
			Detail: n,
		})
	}
}

// classOf labels a symbol for the learner. The picker's classification is
// authoritative when the registry knows the symbol (it distinguishes an
// ETF from a common stock, which the symbol string alone cannot);
// otherwise the symbol-shape inference is the floor.
func (l *autoLoop) classOf(symbol string) string {
	if l != nil && l.registry != nil {
		if inst, ok := l.registry.Lookup(symbol); ok && inst.Class != "" {
			return string(inst.Class)
		}
	}
	return classOfSymbol(symbol)
}

// classResolver returns the resolver the stats ledger uses when rebuilding
// samples from the journal, so a rebuilt trade is filed under the same
// class as a live one.
func (l *autoLoop) classResolver() classResolver {
	return func(symbol string) string { return l.classOf(symbol) }
}

// learningSizeMult returns the sizing multiplier the learner earned for
// one asset class. Paths that size themselves outside EntryPlan (the
// options overlay) ask for it directly; an inactive snapshot returns 1.0,
// which means "the operator's sizing stands".
func (l *autoLoop) learningSizeMult(class string) float64 {
	if l.learn == nil {
		return 1
	}
	return l.learn.snapshot().SizeMultiplierFor(class)
}

// learningGate applies the learner's entry checks to one candidate trade.
// It is the whole surface through which learning may refuse a trade, and
// it can only ever refuse: a nil ledger, or an inactive snapshot, means
// the operator's own thresholds stand exactly as configured.
func (l *autoLoop) learningGate(symbol string, score float64) (bool, string) {
	if l.learn == nil {
		return true, ""
	}
	return l.learn.gate(symbol, score, l.cfg.FactorMinScore)
}

// learningPlan adjusts one entry plan in place and reports whether the
// adjustment left anything to trade. Like the gate it can only shrink.
func (l *autoLoop) learningPlan(p *strategy.EntryPlan) (string, bool) {
	if l.learn == nil || p == nil {
		return "", true
	}
	return l.learn.applyToPlan(p, l.classOf(p.Symbol))
}

func round2(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return math.Round(x*100) / 100
}

func round4(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return math.Round(x*10000) / 10000
}
