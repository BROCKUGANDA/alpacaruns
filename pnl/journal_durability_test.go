package pnl

// Journal durability tests: the intent index, cumulative fill accounting,
// and the crash-tail tolerance that keeps the trading bot startable.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openAt(t *testing.T) (*Journal, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "trades.jsonl")
	j, err := Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	return j, p
}

// A truncated final line is the signature of a crash mid-append. Open()
// reads the whole log to build its dedupe index, so failing on that line
// meant a single torn row stopped the bot from starting at all — no TP/SL
// enforcement, no halts, nothing watching the account.
func TestOpenToleratesTruncatedTail(t *testing.T) {
	j, p := openAt(t)
	sc := 0.71
	if err := j.Append(Record{Kind: KindFill, OrderID: "ok-1", Symbol: "AAPL",
		Side: "buy", Qty: "5", Price: "319.69", Confidence: &sc}); err != nil {
		t.Fatalf("append: %v", err)
	}
	j.Close()

	// Simulate the crash: a half-written record with no trailing newline.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_, _ = f.WriteString(`{"kind":"fill","order_id":"cut-1","symbol":"MSF`)
	_ = f.Close()

	back, err := Open(p)
	if err != nil {
		t.Fatalf("a torn tail must not prevent the bot from starting: %v", err)
	}
	recs, err := back.Records()
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	if len(recs) != 1 || recs[0].OrderID != "ok-1" {
		t.Fatalf("the intact record must survive: %+v", recs)
	}
	back.Close()
}

// Corruption in the MIDDLE of the log is a different fact, and must not be
// forgiven by the same tolerance: silently skipping it would drop real
// trades from realized P/L.
func TestReadRecordsStillFailsOnMidFileCorruption(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "trades.jsonl")
	var b strings.Builder
	for _, id := range []string{"a", "b"} {
		line, _ := json.Marshal(Record{Kind: KindFill, OrderID: id, Symbol: "X", Qty: "1", Price: "1",
			TS: time.Now().UTC()})
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteString("{not json at all}\n")
	line, _ := json.Marshal(Record{Kind: KindFill, OrderID: "c", Symbol: "X", Qty: "1", Price: "1",
		TS: time.Now().UTC()})
	b.Write(line)
	b.WriteByte('\n')
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readRecords(p); err == nil {
		t.Fatal("mid-file corruption was swallowed")
	}
	if _, err := Open(p); err == nil {
		t.Fatal("Open accepted a corrupted journal; the reader must not lie about the book")
	}
}

// Intents are indexed by client_order_id, and an intent with no score is
// absent rather than zero: reconciliation must not hand the learner a
// fabricated 0.00 to calibrate against.
func TestIntentsByClientOrderID(t *testing.T) {
	j, _ := openAt(t)
	sc := 0.66
	if err := j.Append(Record{Kind: KindIntent, ClientOrderID: "coid-scored",
		Symbol: "AAPL", Side: "buy", Qty: "3", Confidence: &sc}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := j.Append(Record{Kind: KindIntent, ClientOrderID: "coid-plain",
		Symbol: "TSLA", Side: "buy", Qty: "1"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	m, err := j.IntentsByClientOrderID()
	if err != nil {
		t.Fatalf("intents: %v", err)
	}
	if got, ok := m["coid-scored"]; !ok || got != 0.66 {
		t.Fatalf("scored intent missing: %+v", m)
	}
	if _, ok := m["coid-plain"]; ok {
		t.Fatal("an unscored intent was indexed; reconciliation would invent evidence")
	}
	if v, ok := j.IntentScore("coid-plain"); ok || v != 0 {
		t.Fatalf("unscored intent reported a score: %v %v", v, ok)
	}
	if v, ok := j.IntentScore(""); ok || v != 0 {
		t.Fatal("an empty client_order_id resolved to an intent")
	}
	// Decisions and fills must not be mistaken for intents.
	if err := j.Append(Record{Kind: KindDecision, ClientOrderID: "coid-decision"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, ok := j.IntentScore("coid-decision"); ok {
		t.Fatal("a decision row was indexed as an intent")
	}
}

// Cumulative quantity is what makes reconciliation both idempotent and
// partial-fill aware.
func TestFilledByOrderAccumulates(t *testing.T) {
	j, _ := openAt(t)
	for _, q := range []string{"3", "7"} {
		if err := j.Append(Record{Kind: KindFill, OrderID: "o-partial", Symbol: "NVDA",
			Side: "buy", Qty: q, Price: "900"}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := j.Append(Record{Kind: KindFill, OrderID: "", Symbol: "X", Qty: "5", Price: "1"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := j.Append(Record{Kind: KindFill, OrderID: "o-garbage", Symbol: "X", Qty: "abc", Price: "1"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := j.Append(Record{Kind: KindFill, OrderID: "o-negative", Symbol: "X", Qty: "-4", Price: "1"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := j.Append(Record{Kind: KindDecision, OrderID: "o-partial", Symbol: "NVDA", Qty: "100"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := j.FilledByOrder()
	if err != nil {
		t.Fatalf("filled by order: %v", err)
	}
	if got["o-partial"] != 10 {
		t.Fatalf("partial fills did not accumulate: %v", got)
	}
	// A garbage or negative quantity must not enter the index at all: the
	// reconciliation pass compares these totals against the broker, so a
	// phantom number here becomes either a missed fill or a duplicated one.
	if _, ok := got["o-garbage"]; ok {
		t.Fatalf("an unparsable qty entered the index: %+v", got)
	}
	if _, ok := got["o-negative"]; ok {
		t.Fatalf("a negative qty entered the index: %+v", got)
	}
	if len(got) != 1 {
		t.Fatalf("non-fill rows or id-less rows leaked into the index: %+v", got)
	}
}

// The intent kind must survive the wire: an unhandled kind would make the
// whole durability mechanism invisible after a restart.
func TestIntentRoundTrip(t *testing.T) {
	j, p := openAt(t)
	sc := 0.8
	if err := j.Append(Record{Kind: KindIntent, ClientOrderID: "coid-rt", Symbol: "BTC/USD",
		Side: "buy", Qty: "0.01", Price: "60000.0000", Confidence: &sc}); err != nil {
		t.Fatalf("append: %v", err)
	}
	j.Close()
	back, err := Open(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer back.Close()
	m, err := back.IntentsByClientOrderID()
	if err != nil {
		t.Fatalf("intents: %v", err)
	}
	if m["coid-rt"] != 0.8 {
		t.Fatalf("intent lost its score across a restart: %+v", m)
	}
}
