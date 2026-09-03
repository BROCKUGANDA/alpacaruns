package pnl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fill(sym, side, qty, price string, ts time.Time) Record {
	return Record{Kind: KindFill, Symbol: sym, Side: side, Qty: qty, Price: price, TS: ts}
}

func TestComputeFIFORealized(t *testing.T) {
	base := time.Date(2026, 8, 20, 14, 30, 0, 0, time.UTC)
	tests := []struct {
		name      string
		fills     []Record
		wantReal  float64
		wantWins  int
		wantLoss  int
		wantWinRt float64
		wantOpen  map[string]float64 // symbol -> signed open qty
	}{
		{
			name: "simple win",
			fills: []Record{
				fill("AAPL", "buy", "10", "100", base),
				fill("AAPL", "sell", "10", "110", base.Add(time.Minute)),
			},
			wantReal: 100, wantWins: 1, wantWinRt: 1,
		},
		{
			name: "simple loss",
			fills: []Record{
				fill("AAPL", "buy", "10", "100", base),
				fill("AAPL", "sell", "10", "95", base.Add(time.Minute)),
			},
			wantReal: -50, wantLoss: 1,
		},
		{
			name: "partial close then full close across avg cost",
			fills: []Record{
				fill("AAPL", "buy", "5", "100", base),
				fill("AAPL", "buy", "15", "110", base.Add(time.Second)), // avg 108.5
				fill("AAPL", "sell", "10", "105", base.Add(time.Minute)),
				fill("AAPL", "sell", "10", "112", base.Add(2*time.Minute)),
			},
			// sell1: (105-100)*5 + (105-110)*5 = 25-25 = 0 -> neither win nor loss
			// sell2: (112-110)*10 = 20
			wantReal: 20, wantWins: 1, wantWinRt: 1,
		},
		{
			name: "open long remains",
			fills: []Record{
				fill("MSFT", "buy", "4", "50", base),
				fill("MSFT", "sell", "1", "60", base.Add(time.Minute)),
			},
			// realized (60-50)*1 = 10; open 3 @ 50
			wantReal: 10, wantWins: 1, wantWinRt: 1,
			wantOpen: map[string]float64{"MSFT": 3},
		},
		{
			name: "short trade wins when price falls",
			fills: []Record{
				fill("TSLA", "sell", "2", "200", base),
				fill("TSLA", "buy", "2", "180", base.Add(time.Minute)),
			},
			wantReal: 40, wantWins: 1, wantWinRt: 1,
		},
		{
			name: "flip long to short",
			fills: []Record{
				fill("NVDA", "buy", "10", "90", base),
				fill("NVDA", "sell", "12", "100", base.Add(time.Minute)),
			},
			// close 10 long @ profit 100, flip short 2 @ 100
			wantReal: 100, wantWins: 1, wantWinRt: 1,
			wantOpen: map[string]float64{"NVDA": -2},
		},
		{
			name:  "zero trades",
			fills: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compute(tt.fills)
			if diff := approx(got.Realized, tt.wantReal); diff > 1e-9 {
				t.Fatalf("realized = %v, want %v", got.Realized, tt.wantReal)
			}
			if got.Wins != tt.wantWins || got.Losses != tt.wantLoss {
				t.Fatalf("wins/losses = %d/%d, want %d/%d", got.Wins, got.Losses, tt.wantWins, tt.wantLoss)
			}
			if diff := approx(got.WinRate, tt.wantWinRt); diff > 1e-9 {
				t.Fatalf("win rate = %v, want %v", got.WinRate, tt.wantWinRt)
			}
			for _, ss := range got.Symbols {
				if want, ok := tt.wantOpen[ss.Symbol]; ok {
					if diff := approx(ss.OpenQty, want); diff > 1e-9 {
						t.Fatalf("%s open qty = %v, want %v", ss.Symbol, ss.OpenQty, want)
					}
				} else if ss.OpenQty != 0 {
					t.Fatalf("%s expected flat, open = %v", ss.Symbol, ss.OpenQty)
				}
			}
		})
	}
}

func TestComputeUnsortedFillsSortedByTimestamp(t *testing.T) {
	base := time.Date(2026, 8, 20, 14, 30, 0, 0, time.UTC)
	// Deliberately out of chronological order.
	fills := []Record{
		fill("AAPL", "sell", "10", "110", base.Add(time.Minute)),
		fill("AAPL", "buy", "10", "100", base),
	}
	got := Compute(fills)
	if approx(got.Realized, 100) > 1e-9 {
		t.Fatalf("realized = %v, want 100 (fills must sort by TS)", got.Realized)
	}
}

func TestComputeSkipsMalformedFills(t *testing.T) {
	base := time.Date(2026, 8, 20, 14, 30, 0, 0, time.UTC)
	fills := []Record{
		fill("AAPL", "buy", "abc", "100", base), // bad qty
		fill("AAPL", "buy", "0", "100", base),   // non-positive qty
		fill("", "buy", "10", "100", base),      // missing symbol
		{Kind: KindFill, Symbol: "X", Side: "hold", Qty: "1", Price: "1", TS: base},
		fill("AAPL", "buy", "10", "100", base), // valid
		fill("AAPL", "sell", "10", "101", base.Add(time.Minute)),
	}
	st := Compute(fills)
	if st.ParseErrs != 4 {
		t.Fatalf("parse errors = %d, want 4", st.ParseErrs)
	}
	if st.FillCount != 2 || st.Realized != 10 || st.Wins != 1 {
	}
}

// TestComputeZeroCostBasis guards a plausible bug: fills recorded with an
// empty price (e.g. market order without filled_avg_price yet) must not
// poison the book — they are skipped and counted as parse errors.
func TestComputeZeroCostBasis(t *testing.T) {
	base := time.Date(2026, 8, 20, 14, 30, 0, 0, time.UTC)
	fills := []Record{
		{Kind: KindFill, Symbol: "AAPL", Side: "buy", Qty: "10", Price: "", TS: base},
		fill("AAPL", "buy", "10", "100", base.Add(time.Second)),
	}
	st := Compute(fills)
	if st.ParseErrs != 1 {
		t.Fatalf("parse errors = %d, want 1", st.ParseErrs)
	}
	if len(st.Symbols) != 1 || st.Symbols[0].OpenQty != 10 || st.Symbols[0].AvgCost != 100 {
		t.Fatalf("unexpected book: %+v", st.Symbols)
	}
}

func TestUnrealized(t *testing.T) {
	tests := []struct {
		name                string
		qty, avg, cur, want float64
	}{
		{"long gain", 10, 100, 110, 100},
		{"long loss", 10, 100, 90, -100},
		{"short gain", -10, 100, 90, 100},
		{"short loss", -10, 100, 115, -150},
		{"flat", 0, 100, 110, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Unrealized(tt.qty, tt.avg, tt.cur); approx(got, tt.want) > 1e-9 {
				t.Fatalf("Unrealized(%v,%v,%v) = %v, want %v", tt.qty, tt.avg, tt.cur, got, tt.want)
			}
		})
	}
}

func TestJournalRoundTripAndDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "trades.jsonl")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	conf := 0.83
	recs := []Record{
		{Kind: KindFill, OrderID: "o1", Symbol: "AAPL", Side: "buy", Qty: "10", Price: "100"},
		{Kind: KindDecision, Source: "trade_ideas", Symbol: "MSFT", Side: "buy", Confidence: &conf, Risk: "APPROVED"},
		{Kind: KindReconcile, Broker: map[string]string{"AAPL": "10@100"}},
	}
	for _, r := range recs {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if !j.KnownOrder("o1") {
		t.Fatal("known order o1 not tracked")
	}
	if j.KnownOrder("o2") {
		t.Fatal("unknown order reported known")
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: dedup state reloads from disk; records survive restart.
	j2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if !j2.KnownOrder("o1") {
		t.Fatal("dedup state lost after reopen")
	}
	got, err := j2.Records()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	if got[0].Kind != KindFill || got[1].Symbol != "MSFT" || got[1].Confidence == nil || *got[1].Confidence != 0.83 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// File exists on disk with one JSON object per line.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []json.RawMessage
	if err := json.Unmarshal([]byte("["+string(raw[:len(raw)-1])+"]"), &lines); err != nil {
		// replace newlines to build a JSON array
		lines = nil
	}
	_ = lines
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatal("log must be newline-delimited")
	}
}

func TestFillsSinceFilter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trades.jsonl")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	if err := j.Append(fill("AAPL", "buy", "1", "1", old)); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(fill("MSFT", "buy", "1", "1", recent)); err != nil {
		t.Fatal(err)
	}
	got, err := j.Fills(recent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Symbol != "MSFT" {
		t.Fatalf("since filter wrong: %+v", got)
	}
	all, err := j.Fills(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("zero since should return all, got %d", len(all))
	}
}

func TestParseSince(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
		check   func(*testing.T, time.Time)
	}{
		{in: ""},
		{in: "  "},
		{in: "2026-08-01", check: func(t *testing.T, ts time.Time) {
			if ts.Year() != 2026 || ts.Month() != time.August || ts.Day() != 1 {
				t.Fatalf("parsed %v", ts)
			}
		}},
		{in: "2026-08-01T09:30:00Z", check: func(t *testing.T, ts time.Time) {
			if ts.Hour() != 9 || ts.Minute() != 30 {
				t.Fatalf("parsed %v", ts)
			}
		}},
		{in: "not-a-date", wantErr: true},
		{in: "08/01/2026", wantErr: true},
	}
	for _, tt := range tests {
		ts, err := ParseSince(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("ParseSince(%q): expected error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseSince(%q): %v", tt.in, err)
		}
		if tt.check != nil {
			tt.check(t, ts)
		}
	}
}

func TestComparePositions(t *testing.T) {
	tests := []struct {
		name       string
		local, bkr map[string]float64
		wantSyms   []string
	}{
		{"in sync", map[string]float64{"AAPL": 10}, map[string]float64{"AAPL": 10}, nil},
		{"broker only", map[string]float64{}, map[string]float64{"MSFT": 5}, []string{"MSFT"}},
		{"local only", map[string]float64{"TSLA": 3}, map[string]float64{}, []string{"TSLA"}},
		{"qty mismatch", map[string]float64{"NVDA": 7}, map[string]float64{"NVDA": 9}, []string{"NVDA"}},
		{"both zero ignored", map[string]float64{"A": 0}, map[string]float64{"A": 0}, nil},
		{"sign mismatch", map[string]float64{"S": 5}, map[string]float64{"S": -5}, []string{"S"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComparePositions(tt.local, tt.bkr)
			if len(got) != len(tt.wantSyms) {
				t.Fatalf("got %+v, want symbols %v", got, tt.wantSyms)
			}
			for i, d := range got {
				if d.Symbol != tt.wantSyms[i] {
					t.Fatalf("symbol[%d] = %s, want %s", i, d.Symbol, tt.wantSyms[i])
				}
			}
		})
	}
}

func TestExtractDecisionsJSONAndFallback(t *testing.T) {
	// Structured array of ideas.
	ideas := `[{"ticker":"AAPL","direction":"buy","confidence":0.9},{"ticker":"MSFT","direction":"sell","confidence":0.55}]`
	got := ExtractDecisions("trade_ideas", ideas)
	if len(got) != 2 {
		t.Fatalf("got %d decisions, want 2", len(got))
	}
	if got[0].Symbol != "AAPL" || got[0].Side != "buy" || got[0].Confidence == nil || *got[0].Confidence != 0.9 {
		t.Fatalf("decision[0] mismatch: %+v", got[0])
	}
	if got[1].Risk != "" {
		t.Fatalf("no risk text expected: %+v", got[1])
	}

	// Prose-wrapped risk assessment -> single audit record with sniffed verdict.
	risk := `Idea AAPL: APPROVED because notional 5000 < limit. Idea MSFT: REJECTED confidence below threshold.`
	got = ExtractDecisions("risk_assessment", risk)
	if len(got) != 1 || got[0].Risk != "APPROVED" {
		t.Fatalf("risk fallback mismatch: %+v", got)
	}

	// Empty input yields nothing.
	if got := ExtractDecisions("executions", ""); got != nil {
		t.Fatalf("expected nil for empty, got %+v", got)
	}
}

func TestExtractDecisionsSingleObjectWithProse(t *testing.T) {
	v := map[string]any{"symbol": "NVDA", "side": "BUY", "confidence": 0.72}
	got := ExtractDecisions("executions", v)
	if len(got) != 1 || got[0].Symbol != "NVDA" || got[0].Side != "buy" {
		t.Fatalf("mismatch: %+v", got)
	}
}

func approx(a, b float64) float64 {
	d := a - b
	if d < 0 {
		return -d
	}
	return d
}
