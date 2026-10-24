package main

// stats.go: per-symbol trade outcome tracking and adaptive thresholds.
//
// The journal records every fill, but Compute() only gives aggregate
// Wins/Losses/Realized per symbol — not avg win/loss %, hold time, or
// signal quality over time. The helpers here persist a richer
// per-symbol ledger to data/strategy-stats.json so the bot can:
//
//   - Tax the signal confidence of symbols with a chronically low win
//     rate, so the unchanged risk floor now rejects them.
//   - Feed the learner the per-trade samples it calibrates from.
//   - Re-deploy capital immediately after a profitable close.
//
// The ledger never makes a trade easier to take. A good record changes
// nothing about the bar; only a bad one moves the number, and it moves it
// against the symbol.
//
// The file is JSON, reloaded on every tick, and rewritten under a
// temp+rename for crash safety. Cheap to read (<1ms for hundreds of
// symbols) so no caching is required.

import (
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

	"github.com/BROCKUGANDA/alpacaruns/learning"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// TradeStats is the per-symbol rolling performance record. The bot
// reads it on every tick to gate entries (ConfidenceBias) and write
// it on every closed trade.
type TradeStats struct {
	Symbol         string    `json:"symbol"`
	Wins           int       `json:"wins"`
	Losses         int       `json:"losses"`
	WinRate        float64   `json:"win_rate"`        // 0..1, smoothed by EWMA
	AvgWinPct      float64   `json:"avg_win_pct"`     // mean gain on winners, e.g. 0.025 = 2.5%
	AvgLossPct     float64   `json:"avg_loss_pct"`    // mean loss on losers, e.g. -0.012 = -1.2%
	AvgHoldMin     float64   `json:"avg_hold_min"`    // mean minutes between buy and sell
	LastWinAt      time.Time `json:"last_win_at"`     // last profitable close
	LastLossAt     time.Time `json:"last_loss_at"`    // last losing close
	LastTradeAt    time.Time `json:"last_trade_at"`   // any close (buy or sell)
	ConfidenceBias float64   `json:"confidence_bias"` // penalty applied to the signal: 0 to -0.20
	TotalPnL       float64   `json:"total_pnl"`       // cumulative realized P/L
	TotalFills     int       `json:"total_fills"`
}

// statsFile is the on-disk format. All symbols in one map; the file is
// keyed by TRADE_LOG-derived path so the same stats stay paired with
// the same journal.
type statsFile struct {
	Version   int                   `json:"version"`
	UpdatedAt time.Time             `json:"updated_at"`
	BySymbol  map[string]TradeStats `json:"by_symbol"`
	// Samples is the bounded history the learner calibrates from. It is
	// stored beside the aggregates rather than derived from them: an
	// average cannot be re-cut by asset class or by entry score, and
	// those two splits are the whole point of the learner.
	Samples []learning.Sample `json:"samples,omitempty"`
}

// statsLedger is the in-memory cache. The auto loop reads it every
// tick (cheap) and writes back on every trade closure.
type statsLedger struct {
	mu      sync.Mutex
	path    string
	cache   map[string]TradeStats
	samples []learning.Sample
	// tickCounter is incremented by the auto loop every tick; used
	// to gate periodic stats-dump logs (every Nth tick) without
	// reaching for a clock.
	tickCounter int
}

func newStatsLedger(journalPath string) *statsLedger {
	dir := filepath.Dir(journalPath)
	return &statsLedger{
		path:  filepath.Join(dir, "strategy-stats.json"),
		cache: map[string]TradeStats{},
	}
}

// load reads the stats file (idempotent on missing file).
func (l *statsLedger) load() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f statsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	if f.BySymbol == nil {
		f.BySymbol = map[string]TradeStats{}
	}
	l.cache = f.BySymbol
	// Re-seed the learner's window from disk. The journal is rebuilt on
	// every start, so without this the sample history restarts empty on
	// every restart and the learner stays below its own bar forever.
	l.samples = append([]learning.Sample(nil), f.Samples...)
	if n := len(l.samples); n > maxLearningSamples {
		l.samples = append([]learning.Sample(nil), l.samples[n-maxLearningSamples:]...)
	}
	return nil
}

// persist writes the cache atomically (temp + rename) so a crash
// mid-write doesn't corrupt the file.
func (l *statsLedger) persist() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := statsFile{
		Version:   1,
		UpdatedAt: time.Now().UTC(),
		BySymbol:  l.cache,
		Samples:   l.samples,
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), "strategy-stats-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), l.path)
}

// get returns a copy of the stats for a symbol (zero-value if absent).
func (l *statsLedger) get(symbol string) TradeStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.cache[symbol]; ok {
		return s
	}
	return TradeStats{Symbol: symbol}
}

// all returns a sorted slice of every symbol's stats.
func (l *statsLedger) all() []TradeStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]TradeStats, 0, len(l.cache))
	for _, s := range l.cache {
		out = append(out, s)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Symbol < out[k].Symbol })
	return out
}

// recordClose updates the per-symbol stats after a profitable or
// losing close. The caller passes pct = (close - entry)/entry (signed),
// hold = time between buy and sell, and pnl = realized dollars.
func (l *statsLedger) recordClose(symbol string, pct, pnl float64, hold time.Duration) {
	l.recordOutcome(TradeOutcome{Symbol: symbol, Pct: pct, PnL: pnl, Hold: hold, Closed: time.Now().UTC()})
}

// recordOutcome folds one closed round-trip into the per-symbol record
// AND into the bounded sample history the learner calibrates from.
func (l *statsLedger) recordOutcome(o TradeOutcome) {
	l.mu.Lock()
	defer l.mu.Unlock()
	symbol := o.Symbol
	s := l.cache[symbol]
	if s.Symbol == "" {
		s.Symbol = symbol
	}
	applyClose(&s, o.Pct, o.PnL, o.Hold, o.Closed)
	l.cache[symbol] = s
	if o.Class == "" {
		o.Class = classOfSymbol(symbol)
	}
	if o.Closed.IsZero() {
		o.Closed = time.Now().UTC()
	}
	l.samples = append(l.samples, learning.Sample{
		Symbol: symbol, Class: o.Class, Score: o.EntryScore,
		Pct: o.Pct, PnL: o.PnL, Hold: o.Hold, Closed: o.Closed,
	})
	// Bounded: the learner reads a window, so an unbounded history would
	// be a file that grows forever to serve a number that ignores it.
	if n := len(l.samples); n > maxLearningSamples {
		l.samples = append([]learning.Sample(nil), l.samples[n-maxLearningSamples:]...)
	}
}

// maxLearningSamples bounds the stored trade history.
const maxLearningSamples = 600

// learningSamples returns a copy of the stored history.
func (l *statsLedger) learningSamples() []learning.Sample {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]learning.Sample(nil), l.samples...)
}

// applyClose folds one closed round-trip into a stats record. It is
// shared by the live path (recordClose) and the journal rebuild so both
// compute the same numbers.
func applyClose(s *TradeStats, pct, pnl float64, hold time.Duration, now time.Time) {
	s.TotalFills++
	s.TotalPnL += pnl
	s.LastTradeAt = now
	holdMin := hold.Minutes()
	// EWMA over the per-trade pct with alpha=0.3 so a single big
	// winner or loser doesn't dominate. EWMA on pct is split into
	// winner and loser streams so a 10% win doesn't average with a
	// -1% loss into a meaningless +0.5%.
	const alpha = 0.3
	if pnl > 0 {
		s.Wins++
		s.AvgWinPct = ewma(s.AvgWinPct, pct, alpha)
		s.LastWinAt = now
	} else {
		s.Losses++
		s.AvgLossPct = ewma(s.AvgLossPct, pct, alpha)
		s.LastLossAt = now
	}
	if s.Wins+s.Losses > 0 {
		s.WinRate = float64(s.Wins) / float64(s.Wins+s.Losses)
	}
	if holdMin > 0 {
		s.AvgHoldMin = ewma(s.AvgHoldMin, holdMin, alpha)
	}
	// ConfidenceBias: the penalty side only. Below a 30% win rate after
	// 5+ trades -> -0.20; below 45% -> -0.10; otherwise 0.
	s.ConfidenceBias = computeConfidenceBias(*s)
}

// computeConfidenceBias translates the rolling record into a per-symbol
// confidence PENALTY. It returns zero or a negative number, and never a
// bonus: the entry path adds this to the signal's own confidence, so a
// positive value would let a past win inflate a fresh signal past the
// floor the risk gate applies to it. Winning may not buy extra risk —
// that is the same brake-only rule the learner follows.
func computeConfidenceBias(s TradeStats) float64 {
	total := s.Wins + s.Losses
	if total < 5 {
		return 0 // not enough data; don't bias yet
	}
	if s.WinRate < 0.30 {
		return -0.20 // chronic loser: block until proven otherwise
	}
	if s.WinRate < 0.45 {
		return -0.10 // weak signal: require more confidence
	}
	return 0
}

func ewma(prev, sample, alpha float64) float64 {
	if math.IsNaN(prev) || prev == 0 {
		return sample
	}
	return alpha*sample + (1-alpha)*prev
}

// losers returns symbols with the worst recent track record (used to
// demote them or drop them from the active universe).
func (l *statsLedger) losers(minTrades int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for sym, s := range l.cache {
		if s.Wins+s.Losses < minTrades {
			continue
		}
		if s.WinRate < 0.35 {
			out = append(out, sym)
		}
	}
	return out
}

// ---- Trade outcome reconstruction (FIFO round-trips) ----

// closedRoundTrips replays the journal's fills through a FIFO book
// and returns one TradeOutcome per closed round-trip (a buy followed
// by a sell, or vice versa). It also keeps a running count of open
// positions so callers can distinguish "no data" from "no closed
// trades yet".
// classResolver maps a symbol to its asset class. A nil resolver falls
// back to the symbol-shape inference.
type classResolver func(symbol string) string

func classOf(sym string, fn classResolver) string {
	if fn != nil {
		if c := strings.ToLower(strings.TrimSpace(fn(sym))); c != "" {
			return c
		}
	}
	return classOfSymbol(sym)
}

func closedRoundTrips(fills []pnl.Record, classFn classResolver) []TradeOutcome {
	type openLot struct {
		qty, price float64
		openedAt   time.Time
		score      float64
	}
	open := map[string][]openLot{}
	var out []TradeOutcome
	for _, f := range fills {
		if f.Kind != pnl.KindFill {
			continue
		}
		qty, _ := strconv.ParseFloat(strings.TrimSpace(f.Qty), 64)
		price, _ := strconv.ParseFloat(strings.TrimSpace(f.Price), 64)
		side := strings.ToLower(strings.TrimSpace(f.Side))
		sym := strings.TrimSpace(f.Symbol)
		if qty <= 0 || price <= 0 || sym == "" {
			continue
		}
		dir := +1
		if side == "sell" {
			dir = -1
		}
		lots := open[sym]
		switch {
		case dir == +1:
			score := 0.0
			if f.Confidence != nil {
				score = *f.Confidence
			}
			lots = append(lots, openLot{qty: qty, price: price, openedAt: f.TS, score: score})
		case dir == -1:
			for len(lots) > 0 && qty > 0 {
				lot := lots[0]
				matchQty := math.Min(lot.qty, qty)
				// PnL for a long: (sell - buy) * qty; for a short: reverse.
				pnl := (price - lot.price) * matchQty
				pct := (price - lot.price) / lot.price
				out = append(out, TradeOutcome{
					Symbol:     sym,
					PnL:        pnl,
					Pct:        pct,
					Hold:       f.TS.Sub(lot.openedAt),
					Opened:     lot.openedAt,
					Closed:     f.TS,
					EntryScore: lot.score,
					Class:      classOf(sym, classFn),
				})
				lot.qty -= matchQty
				qty -= matchQty
				if lot.qty <= 1e-9 {
					lots = lots[1:]
				} else {
					lots[0] = lot
				}
			}
		}
		open[sym] = lots
	}
	return out
}

// TradeOutcome is one completed round-trip (entry + exit).
type TradeOutcome struct {
	Symbol string
	PnL    float64
	Pct    float64
	Hold   time.Duration
	Opened time.Time
	Closed time.Time
	// EntryScore is the factor composite the bot acted on, carried from
	// the opening fill. It is what turns a list of results into evidence
	// about the decision rule: without it the bot knows it lost money but
	// not that it lost money on a 0.62 while a 0.80 would have won.
	EntryScore float64
	// Class is the asset class the trade belonged to, inferred from the
	// symbol. Expectancy differs by class, so a single blended number
	// would let crypto losses subsidise equity ones.
	Class string
}

// classOfSymbol maps a venue symbol onto the learning bucket it belongs
// to using only what the symbol itself says (contract, crypto pair, or
// plain equity). The picker's richer classification is layered on top by
// classResolver; this is the floor that works even with no registry.
func classOfSymbol(sym string) string {
	s := strings.ToUpper(strings.TrimSpace(sym))
	switch {
	case tools.IsOCCSymbol(s):
		return "option"
	case strings.Contains(s, "/"):
		return "crypto"
	}
	return "equity"
}

// refreshTrades walks the journal and updates the ledger with all
// closed round-trips. Idempotent on the same fill set. Run this at
// startup to rebuild the ledger from history.
func (l *statsLedger) refreshFromJournal(j *pnl.Journal) error {
	return l.refreshFromJournalWith(j, nil)
}

// refreshFromJournalWith rebuilds the ledger, resolving each trade's
// asset class through classFn so realized results group the same way
// entries do.
func (l *statsLedger) refreshFromJournalWith(j *pnl.Journal, classFn classResolver) error {
	recs, err := j.Records()
	if err != nil {
		return err
	}
	// Rebuild from scratch so the ledger always reflects the journal
	// exactly: every fill for a symbol is collected first, then the FIFO
	// book is replayed ONCE per symbol and the per-symbol stats are
	// REPLACED (not added to). The previous behaviour replayed the
	// growing prefix per record and then added the results on top of the
	// persisted cache, so every refresh inflated wins, losses and win
	// rate — permanently, because the inflated numbers were saved.
	fillsBySym := map[string][]pnl.Record{}
	for _, r := range recs {
		sym := r.Symbol
		if sym == "" {
			continue
		}
		fillsBySym[sym] = append(fillsBySym[sym], r)
	}
	syms := make([]string, 0, len(fillsBySym))
	for sym := range fillsBySym {
		syms = append(syms, sym)
	}
	sort.Strings(syms)

	l.mu.Lock()
	defer l.mu.Unlock()
	var rebuilt []learning.Sample
	for _, sym := range syms {
		outcomes := closedRoundTrips(fillsBySym[sym], classFn)
		if len(outcomes) == 0 {
			continue
		}
		s := TradeStats{Symbol: sym}
		for _, o := range outcomes {
			applyClose(&s, o.Pct, o.PnL, o.Hold, o.Closed)
			// Rebuild the learner's window too, so a restart reproduces
			// the same samples rather than starting from nothing.
			rebuilt = append(rebuilt, learning.Sample{
				Symbol: sym, Class: o.Class, Score: o.EntryScore,
				Pct: o.Pct, PnL: o.PnL, Hold: o.Hold, Closed: o.Closed,
			})
		}
		l.cache[sym] = s
	}
	if len(rebuilt) > 0 {
		rebuiltByClosed := append([]learning.Sample(nil), rebuilt...)
		sort.SliceStable(rebuiltByClosed, func(i, j int) bool {
			return rebuiltByClosed[i].Closed.Before(rebuiltByClosed[j].Closed)
		})
		if n := len(rebuiltByClosed); n > maxLearningSamples {
			rebuiltByClosed = rebuiltByClosed[n-maxLearningSamples:]
		}
		l.samples = rebuiltByClosed
	}
	return nil
}

// formatDump returns a one-line per-symbol summary for logs.
func (l *statsLedger) formatDump() string {
	all := l.all()
	parts := make([]string, 0, len(all))
	for _, s := range all {
		parts = append(parts, fmt.Sprintf("%s wr=%.0f%%(%dw/%dl) avgW=%.1f%% avgL=%.1f%% bias=%+.2f",
			s.Symbol, s.WinRate*100, s.Wins, s.Losses, s.AvgWinPct*100, s.AvgLossPct*100, s.ConfidenceBias))
	}
	return strings.Join(parts, " | ")
}
