// Package pnl persists every order fill and trade decision to a local
// JSONL append log and computes realized/unrealized P/L from it.
//
// Storage is deliberately boring: one JSON object per line in a single
// file (default data/trades.jsonl, override with TRADE_LOG). No external
// dependencies; the file survives restarts and is human-readable.
//
// Realized P/L uses FIFO matching per symbol (symmetric: shorts are
// positions opened by a sell with no prior long inventory). Unrealized
// P/L is computed from broker-reported position marks.
package pnl

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Record kinds written to the trade log.
const (
	KindFill      = "fill"
	KindDecision  = "decision"
	KindReconcile = "reconcile"
)

// Record is one line of the JSONL trade log.
type Record struct {
	Kind string    `json:"kind"`
	TS   time.Time `json:"ts"`

	// Fill fields (kind=fill).
	OrderID string `json:"order_id,omitempty"`
	Symbol  string `json:"symbol,omitempty"`
	Side    string `json:"side,omitempty"` // buy | sell
	Qty     string `json:"qty,omitempty"`
	Price   string `json:"price,omitempty"`
	Status  string `json:"status,omitempty"`

	// Decision fields (kind=decision).
	Confidence *float64 `json:"confidence,omitempty"`
	Risk       string   `json:"risk,omitempty"` // APPROVED | REJECTED | HALT_TRADING | ""
	Source     string   `json:"source,omitempty"`
	Detail     string   `json:"detail,omitempty"`

	// Reconcile fields (kind=reconcile).
	Broker map[string]string `json:"broker,omitempty"` // symbol -> "qty@avg_entry"
	Drift  []string          `json:"drift,omitempty"`  // human-readable mismatches
	Equity string            `json:"equity,omitempty"`
}

// Journal is an append-only JSONL trade log. Safe for concurrent use.
type Journal struct {
	mu          sync.Mutex
	path        string
	f           *os.File
	knownOrders map[string]struct{}
}

// Open creates (if needed) and opens the trade log at path, loading the
// set of already-journaled order IDs for deduplication. The file is
// created eagerly so a fresh install produces it on first cycle/pl run.
func Open(path string) (*Journal, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create trade-log dir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open trade log: %w", err)
	}
	j := &Journal{path: path, f: f, knownOrders: map[string]struct{}{}}
	recs, err := readRecords(path)
	if err != nil {
		f.Close()
		return nil, err
	}
	for _, r := range recs {
		if r.Kind == KindFill && r.OrderID != "" {
			j.knownOrders[r.OrderID] = struct{}{}
		}
	}
	return j, nil
}

// Path returns the trade log file path.
func (j *Journal) Path() string { return j.path }

// Append writes one record; TS is stamped if zero.
func (j *Journal) Append(r Record) error {
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("append record: %w", err)
	}
	if r.Kind == KindFill && r.OrderID != "" {
		j.knownOrders[r.OrderID] = struct{}{}
	}
	return nil
}

// KnownOrder reports whether an order ID is already in the log.
func (j *Journal) KnownOrder(orderID string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.knownOrders[orderID]
	return ok
}

// Records returns every record in the log, oldest first.
func (j *Journal) Records() ([]Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return readRecords(j.path)
}

// Fills returns fill records with TS >= since (zero since = all).
func (j *Journal) Fills(since time.Time) ([]Record, error) {
	recs, err := j.Records()
	if err != nil {
		return nil, err
	}
	var fills []Record
	for _, r := range recs {
		if r.Kind != KindFill {
			continue
		}
		if !since.IsZero() && r.TS.Before(since) {
			continue
		}
		fills = append(fills, r)
	}
	return fills, nil
}

// Close flushes and closes the underlying file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

func readRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return recs, fmt.Errorf("%s:%d: corrupt record: %w", path, lineNo, err)
		}
		recs = append(recs, r)
	}
	return recs, sc.Err()
}

// ---- P/L math (pure, unit-testable) ----

// SymbolStats aggregates per-symbol results.
type SymbolStats struct {
	Symbol   string  `json:"symbol"`
	Realized float64 `json:"realized_pl"`
	Wins     int     `json:"wins"`
	Losses   int     `json:"losses"`
	OpenQty  float64 `json:"open_qty"` // signed: +long, -short
	AvgCost  float64 `json:"avg_cost"` // average price of open lots
	Fills    int     `json:"fills"`
}

// Stats is the aggregate result of replaying fills through the FIFO book.
type Stats struct {
	Realized  float64
	Wins      int
	Losses    int
	WinRate   float64 // Wins / (Wins+Losses); 0 when no closed trades
	Symbols   []*SymbolStats
	FillCount int
	ParseErrs int
}

type lot struct {
	qty   float64
	price float64
}

type book struct {
	dir   int // +1 long, -1 short, 0 flat
	lots  []lot
	stats *SymbolStats
}

// Compute replays fills (chronologically; sorted here if timestamps
// disagree with file order) through a per-symbol FIFO book.
func Compute(fills []Record) Stats {
	books := map[string]*book{}
	st := Stats{}

	ordered := make([]Record, len(fills))
	copy(ordered, fills)
	sort.SliceStable(ordered, func(i, k int) bool {
		return ordered[i].TS.Before(ordered[k].TS)
	})

	for _, r := range ordered {
		if r.Kind != KindFill {
			continue
		}
		qty, errQ := strconv.ParseFloat(strings.TrimSpace(r.Qty), 64)
		price, errP := strconv.ParseFloat(strings.TrimSpace(r.Price), 64)
		side := strings.ToLower(strings.TrimSpace(r.Side))
		if errQ != nil || errP != nil || qty <= 0 || price < 0 ||
			(side != "buy" && side != "sell") || strings.TrimSpace(r.Symbol) == "" {
			st.ParseErrs++
			continue
		}
		st.FillCount++
		b := books[r.Symbol]
		if b == nil {
			b = &book{stats: &SymbolStats{Symbol: r.Symbol}}
			books[r.Symbol] = b
		}
		b.apply(side, qty, price)
	}

	for _, b := range books {
		b.stats.OpenQty, b.stats.AvgCost = openPosition(b.dir, b.lots)
		st.Realized += b.stats.Realized
		st.Wins += b.stats.Wins
		st.Losses += b.stats.Losses
		st.Symbols = append(st.Symbols, b.stats)
	}
	sort.Slice(st.Symbols, func(i, k int) bool { return st.Symbols[i].Symbol < st.Symbols[k].Symbol })
	if st.Wins+st.Losses > 0 {
		st.WinRate = float64(st.Wins) / float64(st.Wins+st.Losses)
	}
	return st
}

func (b *book) apply(side string, qty, price float64) {
	b.stats.Fills++
	sign := 1 // +1 = position-opening direction of this fill
	if side == "sell" {
		sign = -1
	}
	switch {
	case b.dir == 0 || b.dir == sign:
		// Opening/extending: push a lot.
		b.dir = sign
		b.lots = append(b.lots, lot{qty: qty, price: price})
	default:
		// Closing against FIFO lots. One closing fill = one closed trade;
		// win/loss judged on its net matched P/L.
		var net float64
		remaining := qty
		for remaining > 1e-12 && len(b.lots) > 0 {
			l := &b.lots[0]
			match := math.Min(l.qty, remaining)
			if b.dir > 0 {
				net += (price - l.price) * match // closing long
			} else {
				net += (l.price - price) * match // closing short
			}
			l.qty -= match
			remaining -= match
			if l.qty <= 1e-12 {
				b.lots = b.lots[1:]
			}
		}
		b.stats.Realized += net
		if net > 0 {
			b.stats.Wins++
		} else if net < 0 {
			b.stats.Losses++
		}
		if len(b.lots) == 0 {
			b.dir = 0
		}
		// Position flip: leftover quantity opens the opposite direction.
		if remaining > 1e-12 {
			b.dir = sign
			b.lots = append(b.lots, lot{qty: remaining, price: price})
		}
	}
}

func openPosition(dir int, lots []lot) (qty, avg float64) {
	if dir == 0 {
		return 0, 0
	}
	var total, cost float64
	for _, l := range lots {
		total += l.qty
		cost += l.qty * l.price
	}
	if total <= 0 {
		return 0, 0
	}
	return float64(dir) * total, cost / total
}

// Unrealized returns mark-to-market P/L for a signed quantity
// (+long / -short) at avgEntry marked at current.
func Unrealized(qty, avgEntry, current float64) float64 {
	return (current - avgEntry) * qty
}

// ---- decision extraction (best-effort, audit-oriented) ----

// StateKeys harvested after a cycle for decision logging.
var StateKeys = []string{"trade_ideas", "risk_assessment", "executions"}

// ExtractDecisions converts one session-state value (key source) into
// zero or more decision records. It tries JSON decoding first and falls
// back to a truncated raw-text record so nothing is lost for audits.
func ExtractDecisions(source string, v any) []Record {
	raw := stringify(v)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var objs []map[string]any
	if !decodeObjects(raw, &objs) {
		risk := sniffRisk(raw)
		return []Record{{
			Kind:   KindDecision,
			Source: source,
			Risk:   risk,
			Detail: truncate(raw, 800),
		}}
	}
	var out []Record
	for _, o := range objs {
		conf := numberField(o, "confidence")
		rec := Record{
			Kind:   KindDecision,
			Source: source,
			Symbol: firstString(o, "ticker", "symbol", "asset"),
			Side:   strings.ToLower(firstString(o, "direction", "side")),
			Qty:    firstString(o, "qty", "quantity", "shares"),
			Risk:   sniffRisk(stringify(o)),
		}
		if conf != nil {
			rec.Confidence = conf
		}
		if detail := stringify(o); len(detail) > 800 {
			rec.Detail = truncate(detail, 800)
		} else {
			rec.Detail = detail
		}
		out = append(out, rec)
	}
	return out
}

func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// decodeObjects tries to interpret s as a JSON array of objects, or a
// single object, scanning inward past surrounding prose if necessary.
func decodeObjects(s string, out *[]map[string]any) bool {
	try := func(txt string) bool {
		dec := json.NewDecoder(strings.NewReader(txt))
		var arr []map[string]any
		if err := dec.Decode(&arr); err == nil {
			*out = arr
			return true
		}
		var one map[string]any
		if err := json.Unmarshal([]byte(txt), &one); err == nil {
			*out = []map[string]any{one}
			return true
		}
		return false
	}
	if try(s) {
		return true
	}
	start := strings.IndexAny(s, "[{")
	end := strings.LastIndexAny(s, "]}")
	if start >= 0 && end > start {
		return try(s[start : end+1])
	}
	return false
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func numberField(m map[string]any, key string) *float64 {
	v, ok := m[key]
	if !ok {
		return nil
	}
	switch n := v.(type) {
	case float64:
		return &n
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return &f
		}
	}
	return nil
}

func sniffRisk(s string) string {
	up := strings.ToUpper(s)
	switch {
	case strings.Contains(up, "HALT_TRADING"):
		return "HALT_TRADING"
	case strings.Contains(up, "APPROVED"):
		return "APPROVED"
	case strings.Contains(up, "REJECTED"):
		return "REJECTED"
	default:
		return ""
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ParseSince parses a start-date value: YYYY-MM-DD, RFC3339, or empty.
func ParseSince(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date %q (want YYYY-MM-DD or RFC3339)", s)
}

// Drift describes a divergence between local books and broker positions.
type Drift struct {
	Symbol    string  `json:"symbol"`
	LocalQty  float64 `json:"local_qty"`
	BrokerQty float64 `json:"broker_qty"`
}

// ComparePositions diffs local FIFO open quantities against broker
// positions (signed quantities, +long/-short). Zero quantities on both
// sides are ignored.
func ComparePositions(local map[string]float64, broker map[string]float64) []Drift {
	syms := map[string]struct{}{}
	for s := range local {
		syms[s] = struct{}{}
	}
	for s := range broker {
		syms[s] = struct{}{}
	}
	var out []Drift
	for s := range syms {
		l, b := local[s], broker[s]
		if math.Abs(l-b) < 1e-9 && l == 0 {
			continue
		}
		if math.Abs(l-b) < 1e-9 {
			continue
		}
		out = append(out, Drift{Symbol: s, LocalQty: l, BrokerQty: b})
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Symbol < out[k].Symbol })
	return out
}
