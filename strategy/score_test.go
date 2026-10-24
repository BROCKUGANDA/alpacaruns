package strategy

// score_test.go covers the entry-score attribution the learner depends on:
// a fill must carry the number the bot actually acted on, and must carry
// nothing when that number does not exist.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

func TestEntryScoreOf(t *testing.T) {
	for _, tc := range []struct {
		name      string
		planScore float64
		conf      float64
		want      float64
	}{
		{"plan score wins", 0.62, 1.0, 0.62},
		{"gate confidence is the fallback", 0, 0.71, 0.71},
		{"a self-reported perfect score is no evidence", 0, 1.0, 0},
		{"nothing at all stays unattributed", 0, 0, 0},
		{"negatives are not scores", -0.5, -1, 0},
		{"a plan score of exactly one is not trusted", 1.0, 0.5, 1.0},
	} {
		if got := entryScoreOf(tc.planScore, tc.conf); got != tc.want {
			t.Errorf("%s: entryScoreOf(%v,%v) = %v, want %v", tc.name, tc.planScore, tc.conf, got, tc.want)
		}
	}
}

// A filled order must be journaled with its score, because a closed trade
// without one cannot be traced back to the decision rule that opened it.
func TestJournalFillCarriesTheEntryScore(t *testing.T) {
	dir := t.TempDir()
	j, err := pnl.Open(filepath.Join(dir, "trades.jsonl"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	defer j.Close()
	x := &Executor{Journal: j}

	o := &tools.Order{ID: "o1", Symbol: "AAPL", Side: "buy", Status: "filled",
		FilledQty: "5", FilledAvgPrice: "319.69"}
	x.journalFill(o, 0.62)
	x.journalFill(&tools.Order{ID: "o2", Symbol: "TSLA", Side: "buy", Status: "filled",
		FilledQty: "1", FilledAvgPrice: "450"}, 0)

	recs, err := j.Records()
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want two fills, got %d", len(recs))
	}
	if recs[0].Confidence == nil || *recs[0].Confidence != 0.62 {
		t.Fatalf("score not journaled on the fill: %+v", recs[0].Confidence)
	}
	// The unscored fill must be recorded with NO score rather than an
	// invented one: a fabricated 0.00 or 1.00 would become evidence.
	if recs[1].Confidence != nil {
		t.Fatalf("unscored fill was given a score: %v", *recs[1].Confidence)
	}
	if recs[0].Kind != pnl.KindFill || recs[0].Price != "319.69" {
		t.Fatalf("fill record shape changed: %+v", recs[0])
	}
}

// Orders that never filled must not create a trade.
func TestJournalFillSkipsUnfilled(t *testing.T) {
	dir := t.TempDir()
	j, err := pnl.Open(filepath.Join(dir, "trades.jsonl"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	defer j.Close()
	x := &Executor{Journal: j}
	x.journalFill(&tools.Order{ID: "a", Symbol: "AAPL", Side: "buy", Status: "new", FilledQty: "", FilledAvgPrice: "10"}, 0.7)
	x.journalFill(&tools.Order{ID: "b", Symbol: "AAPL", Side: "buy", Status: "filled", FilledQty: "3", FilledAvgPrice: ""}, 0.7)
	x.journalFill(nil, 0.7)
	recs, _ := j.Records()
	if len(recs) != 0 {
		t.Fatalf("unfilled or price-less orders journalled: %+v", recs)
	}
}

// The plan must carry the decision's composite, otherwise every entry is
// journaled under the gate confidence and the calibration table is a
// straight line.
func TestPlanEntryCarriesTheDecisionScore(t *testing.T) {
	e, err := NewEngine(EngineConfig{
		Scorer:   fakeScorer{res: map[string]factorsScored{"AAPL": {Composite: 0.66}}},
		Prices:   fakePrices{px: map[string]float64{"AAPL": 100}},
		TPPct:    0.1,
		SLPct:    0.05,
		Sizing:   Sizing{PositionPct: 0.1, MaxPositionUSD: 10000},
		Threshold: Thresholds{FactorMinScore: 0.5, TrendBuy: -1, MomentumBuy: -1},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	plan, err := e.PlanEntry(t.Context(), Decision{Symbol: "AAPL", Composite: 0.66, Signal: SignalBuy}, 100000)
	if err != nil {
		t.Fatalf("PlanEntry: %v", err)
	}
	if plan.Score != 0.66 {
		t.Fatalf("plan did not carry the decision score: %v", plan.Score)
	}
	if plan.Qty < 1 {
		t.Fatalf("plan not sized: %+v", plan)
	}
	// A decision with no composite must not invent one.
	blank, err := e.PlanEntry(t.Context(), Decision{Symbol: "AAPL", Signal: SignalBuy}, 100000)
	if err != nil {
		t.Fatalf("PlanEntry (unscored): %v", err)
	}
	if blank.Score != 0 {
		t.Fatalf("unscored decision produced a score: %v", blank.Score)
	}
}

func TestJournalFillTimestampIsUTC(t *testing.T) {
	dir := t.TempDir()
	j, err := pnl.Open(filepath.Join(dir, "trades.jsonl"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	defer j.Close()
	x := &Executor{Journal: j}
	before := time.Now().UTC().Add(-time.Second)
	x.journalFill(&tools.Order{ID: "z", Symbol: "AAPL", Side: "buy", Status: "filled",
		FilledQty: "1", FilledAvgPrice: "100"}, 0.5)
	recs, _ := j.Records()
	if len(recs) != 1 || recs[0].TS.Before(before) || recs[0].TS.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("fill timestamp is not now-UTC: %+v", recs)
	}
}
