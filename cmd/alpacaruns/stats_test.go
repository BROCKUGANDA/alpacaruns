package main

// stats_test.go covers the ledger's learning plumbing: the sample window
// the learner calibrates from, the entry score each sample must carry, and
// the rebuild's idempotence. The rebuild bug this file guards against was
// real — refreshing used to add the same trades on top of the persisted
// cache, permanently inflating every win rate.

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/learning"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
)

func ptrF(x float64) *float64 { return &x }

// writeFills appends a buy/sell round trip straight to the journal file.
func writeFills(t *testing.T, j *pnl.Journal, sym string, buyPx, sellPx, qty float64, score *float64) {
	t.Helper()
	base := time.Now().UTC().Add(-2 * time.Hour)
	if err := j.Append(pnl.Record{
		Kind: pnl.KindFill, Symbol: sym, Side: "buy",
		Qty: formatF(qty), Price: formatF(buyPx), TS: base, Confidence: score,
	}); err != nil {
		t.Fatalf("append buy: %v", err)
	}
	if err := j.Append(pnl.Record{
		Kind: pnl.KindFill, Symbol: sym, Side: "sell",
		Qty: formatF(qty), Price: formatF(sellPx), TS: base.Add(time.Hour),
	}); err != nil {
		t.Fatalf("append sell: %v", err)
	}
}

func formatF(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func openJournalAt(t *testing.T) (*pnl.Journal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trades.jsonl")
	j, err := pnl.Open(path)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	return j, path
}

// A closed round trip must reach the sample window with the score the
// entry carried and the class the resolver assigned.
func TestRefreshBuildsLearningSamples(t *testing.T) {
	j, path := openJournalAt(t)
	l := newStatsLedger(path)
	writeFills(t, j, "SPY", 100, 106, 5, ptrF(0.62))
	writeFills(t, j, "AAPL", 200, 190, 2, nil) // hand-placed: no score

	if err := l.refreshFromJournalWith(j, func(sym string) string {
		if sym == "SPY" {
			return "etf"
		}
		return ""
	}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got := l.learningSamples()
	if len(got) != 2 {
		t.Fatalf("want 2 samples, got %d: %+v", len(got), got)
	}
	var spy, aapl *learning.Sample
	for i := range got {
		switch got[i].Symbol {
		case "SPY":
			spy = &got[i]
		case "AAPL":
			aapl = &got[i]
		}
	}
	if spy == nil || aapl == nil {
		t.Fatalf("both symbols must be present: %+v", got)
	}
	if spy.Class != "etf" {
		t.Fatalf("resolver class not used: %+v", spy)
	}
	if spy.Score != 0.62 {
		t.Fatalf("entry score not carried to the sample: %+v", spy)
	}
	if spy.Pct != 0.06 || spy.PnL != 30 {
		t.Fatalf("round-trip maths: %+v", spy)
	}
	if spy.Hold < time.Minute {
		t.Fatalf("hold time not derived from the fills: %v", spy.Hold)
	}
	// The unscored entry stays unscored — the learner refuses to treat an
	// invented score as evidence.
	if aapl.Score != 0 {
		t.Fatalf("unscored fill was given a score: %v", aapl.Score)
	}
	if aapl.Class != "equity" {
		t.Fatalf("with no resolver answer the symbol shape must decide: %+v", aapl)
	}
	if aapl.Pct >= 0 {
		t.Fatalf("a 200->190 close must be a loss: %+v", aapl)
	}
}

// Rebuilding twice must not double-count. This is the regression the
// replace-not-add rewrite fixed.
func TestRefreshIsIdempotent(t *testing.T) {
	j, path := openJournalAt(t)
	l := newStatsLedger(path)
	writeFills(t, j, "AAPL", 100, 101, 10, ptrF(0.7))

	if err := l.refreshFromJournalWith(j, nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	first := l.get("AAPL")
	if first.Wins != 1 || first.Losses != 0 || first.TotalFills != 1 {
		t.Fatalf("first rebuild wrong: %+v", first)
	}
	if n := len(l.learningSamples()); n != 1 {
		t.Fatalf("first rebuild produced %d samples, want 1", n)
	}
	for i := 0; i < 3; i++ {
		if err := l.refreshFromJournalWith(j, nil); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	again := l.get("AAPL")
	if again.Wins != 1 || again.TotalFills != 1 {
		t.Fatalf("refresh double-counted: %+v", again)
	}
	if n := len(l.learningSamples()); n != 1 {
		t.Fatalf("refresh duplicated samples: %d", n)
	}
}

// The window is bounded: the learner reads a recent window, so the file
// must not grow forever to serve a number that ignores the old half.
func TestSampleWindowIsBounded(t *testing.T) {
	j, path := openJournalAt(t)
	l := newStatsLedger(path)
	for i := 0; i < maxLearningSamples+40; i++ {
		if err := j.Append(pnl.Record{
			Kind: pnl.KindFill, Symbol: "AAPL", Side: "buy",
			Qty: "1", Price: "100", TS: time.Now().UTC().Add(time.Duration(i) * time.Second),
			Confidence: ptrF(0.6),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := j.Append(pnl.Record{
			Kind: pnl.KindFill, Symbol: "AAPL", Side: "sell",
			Qty: "1", Price: "10" + string(rune('0'+i%9)), TS: time.Now().UTC().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := l.refreshFromJournalWith(j, nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if n := len(l.learningSamples()); n > maxLearningSamples {
		t.Fatalf("window grew past the bound: %d", n)
	}
}

// The persisted window must survive a restart, or every deploy resets the
// learner to below its own trade bar.
func TestSamplesSurvivePersistAndReload(t *testing.T) {
	j, path := openJournalAt(t)
	l := newStatsLedger(path)
	writeFills(t, j, "AAPL", 100, 110, 5, ptrF(0.8))
	if err := l.refreshFromJournalWith(j, nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := l.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	fresh := newStatsLedger(path)
	if err := fresh.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := fresh.learningSamples()
	if len(got) != 1 || got[0].Score != 0.8 || got[0].Symbol != "AAPL" {
		t.Fatalf("samples lost across reload: %+v", got)
	}
	if s := fresh.get("AAPL"); s.Wins != 1 {
		t.Fatalf("aggregates lost across reload: %+v", s)
	}
}

// recordClose is the live path: a close the bot itself placed must land in
// both the aggregates and the window.
func TestRecordOutcomeFeedsBothLedgers(t *testing.T) {
	_, path := openJournalAt(t)
	l := newStatsLedger(path)
	l.recordOutcome(TradeOutcome{
		Symbol: "TSLA", Class: "equity", Pct: -0.03, PnL: -30,
		Hold: 3 * time.Hour, EntryScore: 0.55, Closed: time.Now().UTC(),
	})
	s := l.get("TSLA")
	if s.Losses != 1 || s.WinRate != 0 {
		t.Fatalf("aggregate not updated: %+v", s)
	}
	got := l.learningSamples()
	if len(got) != 1 || got[0].Score != 0.55 || got[0].Class != "equity" {
		t.Fatalf("sample not recorded: %+v", got)
	}
	// Empty class falls back to the symbol shape rather than "" — a blank
	// class would group every unlabelled trade into one bucket.
	l.recordOutcome(TradeOutcome{Symbol: "BTC/USD", Pct: 0.01, PnL: 1, Closed: time.Now().UTC()})
	if c := l.learningSamples()[1].Class; c != "crypto" {
		t.Fatalf("class fallback failed: %q", c)
	}
}

// The bias the entry path reads must punish a bad record and stay silent
// about a good one: a winner earning a bonus would push a fresh signal
// past the risk gate's floor, which is the one direction this ledger is
// not allowed to move.
func TestConfidenceBiasIsPenaltyOnly(t *testing.T) {
	_, path := openJournalAt(t)
	l := newStatsLedger(path)
	for i := 0; i < 6; i++ {
		l.recordOutcome(TradeOutcome{Symbol: "NVDA", Pct: -0.02, PnL: -20, Closed: time.Now().UTC()})
	}
	b := l.get("NVDA").ConfidenceBias
	if b >= 0 {
		t.Fatalf("a 0%% win rate over 6 trades produced no penalty: %v", b)
	}
	// Weak but not hopeless: a smaller penalty, still negative.
	for i := 0; i < 4; i++ {
		l.recordOutcome(TradeOutcome{Symbol: "IBM", Pct: -0.02, PnL: -20, Closed: time.Now().UTC()})
	}
	l.recordOutcome(TradeOutcome{Symbol: "IBM", Pct: 0.05, PnL: 50, Closed: time.Now().UTC()})
	l.recordOutcome(TradeOutcome{Symbol: "IBM", Pct: 0.05, PnL: 50, Closed: time.Now().UTC()})
	if got := l.get("IBM").ConfidenceBias; got > 0 {
		t.Fatalf("a 33%% win rate earned a bonus: %v", got)
	}
	for i := 0; i < 8; i++ {
		l.recordOutcome(TradeOutcome{Symbol: "MSFT", Pct: 0.04, PnL: 40, Closed: time.Now().UTC()})
	}
	if got := l.get("MSFT").ConfidenceBias; got != 0 {
		t.Fatalf("a 100%% win rate must not change the bar in the symbol's favour: %v", got)
	}
	// Below the trade bar, no opinion at all.
	l.recordOutcome(TradeOutcome{Symbol: "AMD", Pct: -0.02, PnL: -20, Closed: time.Now().UTC()})
	if got := l.get("AMD").ConfidenceBias; got != 0 {
		t.Fatalf("one trade should not bias anything: %v", got)
	}
	// Every stored bias in the ledger must be <= 0, whatever the mix.
	for _, s := range l.all() {
		if s.ConfidenceBias > 0 {
			t.Fatalf("%s carries a positive bias: %+v", s.Symbol, s)
		}
	}
}
