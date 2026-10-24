package store

// sqlite_test.go: the mirror must be idempotent, and — more importantly —
// the verifier must be ABLE to fail. A comparison that cannot disagree with
// its subject is decoration.

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
)

func fill(order, sym, side string, qty, price float64, at time.Time) pnl.Record {
	return pnl.Record{
		Kind: pnl.KindFill, OrderID: order, ClientOrderID: "coid-" + order,
		Symbol: sym, Side: side,
		Qty:   strconv.FormatFloat(qty, 'f', -1, 64),
		Price: strconv.FormatFloat(price, 'f', -1, 64),
		TS:    at,
	}
}

func sampleJournal() []pnl.Record {
	now := time.Now().UTC()
	sc := 0.66
	return []pnl.Record{
		{Kind: pnl.KindIntent, ClientOrderID: "coid-o1", Symbol: "AAPL", Side: "buy",
			Qty: "5", Price: "100", Confidence: &sc, TS: now.Add(-3 * time.Hour)},
		fill("o1", "AAPL", "buy", 5, 100, now.Add(-3*time.Hour)),
		fill("o2", "AAPL", "sell", 5, 104, now.Add(-time.Hour)),
		{Kind: pnl.KindDecision, Source: "strategy:auto", Risk: "APPROVED", Symbol: "AAPL",
			Side: "buy", Detail: "entry", TS: now.Add(-3 * time.Hour)},
		{Kind: pnl.KindReconcile, Source: "reconcile:fills", Detail: "recovered", TS: now.Add(-2 * time.Hour)},
	}
}

func sampleState() *strategy.State {
	now := time.Now().UTC()
	return &strategy.State{
		Version:        1,
		PeakEquity:     102500,
		StartingEquity: 100000,
		TickNumber:     42,
		LastTick:       now,
		Levels: map[string]*strategy.PositionLevels{
			"AAPL": {Symbol: "AAPL", EntryPrice: 100, TakeProfit: 110, StopLoss: 95, Qty: "5", Since: now},
			"BTC/USD|intraday": {Symbol: "BTC/USD", Track: "intraday", EntryPrice: 60000,
				TakeProfit: 63000, StopLoss: 58000, Qty: "0.01", Since: now},
		},
	}
}

func openTemp(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// Ingestion is a diff under the hood: running it twice must add nothing.
func TestIngestIsIdempotent(t *testing.T) {
	d := openTemp(t)
	recs := sampleJournal()
	if n, err := d.Ingest(recs); err != nil || n == 0 {
		t.Fatalf("first ingest: n=%d err=%v", n, err)
	}
	n, err := d.Ingest(recs)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if n != 0 {
		t.Fatalf("second ingest wrote %d rows; the mirror would grow on every tick", n)
	}
	if err := d.MirrorState(sampleState()); err != nil {
		t.Fatalf("mirror state: %v", err)
	}
	rep, err := d.Verify(recs, sampleState())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.OK {
		t.Fatalf("clean mirror reported %v", rep.Mismatches)
	}
	if rep.Fills != 2 || rep.Intents != 1 || rep.Decisions != 2 || rep.Levels != 2 {
		t.Fatalf("unexpected counts: %+v", rep)
	}
}

// The verifier must catch a mirror that disagrees with the sources.
func TestVerifyCatchesDivergence(t *testing.T) {
	recs := sampleJournal()
	st := sampleState()

	t.Run("missing level", func(t *testing.T) {
		d := openTemp(t)
		if err := d.MirrorState(st); err != nil {
			t.Fatalf("mirror: %v", err)
		}
		if _, err := d.sql.Exec(`DELETE FROM levels WHERE symbol='AAPL'`); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		rep, err := d.Verify(recs, st)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if rep.OK {
			t.Fatal("a mirror missing a level verified clean")
		}
		found := false
		for _, m := range rep.Mismatches {
			if m == "level missing in mirror: AAPL" {
				found = true
			}
		}
		if !found {
			t.Fatalf("mismatch did not name the level: %v", rep.Mismatches)
		}
	})

	t.Run("level prices drifted", func(t *testing.T) {
		d := openTemp(t)
		if err := d.MirrorState(st); err != nil {
			t.Fatalf("mirror: %v", err)
		}
		// A stale stop is the failure that matters: protection at the wrong
		// price looks identical to protection at the right one.
		if _, err := d.sql.Exec(`UPDATE levels SET stop_loss=90 WHERE symbol='AAPL'`); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		rep, err := d.Verify(recs, st)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if rep.OK {
			t.Fatal("a stop-loss that drifted verified clean")
		}
	})

	t.Run("fill missing", func(t *testing.T) {
		d := openTemp(t)
		if _, err := d.Ingest(recs); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if _, err := d.sql.Exec(`DELETE FROM fills WHERE order_id='o2'`); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		rep, err := d.Verify(recs, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if rep.OK {
			t.Fatal("a missing fill verified clean")
		}
	})
}

// Two prints of one order are two rows, and the totals still add up — this is
// what makes the mirror usable for the reconciliation pass.
func TestPartialFillsAreSeparateRows(t *testing.T) {
	d := openTemp(t)
	now := time.Now().UTC()
	recs := []pnl.Record{
		fill("part-1", "NVDA", "buy", 3, 900, now.Add(-2*time.Hour)),
		fill("part-1", "NVDA", "buy", 7, 901, now.Add(-time.Hour)),
	}
	if _, err := d.Ingest(recs); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	totals, err := d.FillTotals()
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals["part-1"] != 10 {
		t.Fatalf("partial fills did not accumulate: %+v", totals)
	}
	// Re-ingesting must not duplicate the second print.
	if _, err := d.Ingest(recs); err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if totals, _ := d.FillTotals(); totals["part-1"] != 10 {
		t.Fatalf("re-ingest duplicated a print: %+v", totals)
	}
}

// The mirror's realised P/L must equal the journal's, or moving a reader over
// would silently change the numbers it reports.
func TestRealizedMatchesJournalComputation(t *testing.T) {
	d := openTemp(t)
	recs := sampleJournal()
	if _, err := d.Ingest(recs); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got, err := d.RealizedBySymbol()
	if err != nil {
		t.Fatalf("realized: %v", err)
	}
	want := pnl.Compute(recs)
	var wantAAPL float64
	for _, s := range want.Symbols {
		if s.Symbol == "AAPL" {
			wantAAPL = s.Realized
		}
	}
	if wantAAPL == 0 {
		t.Fatal("test premise: the fixture should realise a profit")
	}
	if diff := got["AAPL"] - wantAAPL; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("realised differs: mirror %.6f vs journal %.6f", got["AAPL"], wantAAPL)
	}
}

// A file written by a newer build must not be silently downgraded.
func TestRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := d.sql.Exec(`UPDATE meta SET value='99' WHERE key='schema_version'`); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	d.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("opened a database from a newer schema")
	}
}
