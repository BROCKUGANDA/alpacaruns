package main

// stats.go: per-symbol trade outcome tracking and adaptive thresholds.
//
// The journal records every fill, but Compute() only gives aggregate
// Wins/Losses/Realized per symbol — not avg win/loss %, hold time, or
// signal quality over time. The helpers here persist a richer
// per-symbol ledger to data/strategy-stats.json so the bot can:
//
//   - Drop symbols with chronically low win rates.
//   - Lower confidence thresholds on high-win symbols.
//   - Re-deploy capital immediately after a profitable close.
//   - Skip symbols that have bled recently.
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

	"github.com/BROCKUGANDA/alpacaruns/pnl"
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
	ConfidenceBias float64   `json:"confidence_bias"` // additive bias: -0.10 to +0.15
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
}

// statsLedger is the in-memory cache. The auto loop reads it every
// tick (cheap) and writes back on every trade closure.
type statsLedger struct {
	mu    sync.Mutex
	path  string
	cache map[string]TradeStats
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
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.cache[symbol]
	if s.Symbol == "" {
		s.Symbol = symbol
	}
	now := time.Now().UTC()
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
	// ConfidenceBias: positive when the symbol is reliably profitable
	// (raise the bar for losers, lower it for winners). Range -0.20
	// to +0.20. Below 30% win rate after 5+ trades -> -0.20 (block).
	// Above 60% win rate with positive expectancy -> up to +0.15.
	s.ConfidenceBias = computeConfidenceBias(s)
	l.cache[symbol] = s
}

// computeConfidenceBias translates the rolling record into a per-
// decision confidence offset. See the doc on TradeStats.ConfidenceBias.
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
	// Expectancy: avg_win * win_rate + avg_loss * loss_rate.
	// Positive expectancy unlocks a confidence discount.
	exp := s.AvgWinPct*s.WinRate + s.AvgLossPct*(1-s.WinRate)
	if s.WinRate >= 0.60 && exp > 0 {
		return math.Min(0.15, 0.10+exp*2)
	}
	return 0
}

func ewma(prev, sample, alpha float64) float64 {
	if math.IsNaN(prev) || prev == 0 {
		return sample
	}
	return alpha*sample + (1-alpha)*prev
}

// confidenceRequired returns the effective MinConfidence for a symbol
// decision. The base threshold (e.g. 0.50) is shifted by the per-
// symbol bias. The auto loop passes this into the ensemble gater.
func (l *statsLedger) confidenceRequired(base float64, symbol string) float64 {
	s := l.get(symbol)
	return clamp01(base + s.ConfidenceBias)
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
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
func closedRoundTrips(fills []pnl.Record) []TradeOutcome {
	type openLot struct {
		qty, price float64
		openedAt   time.Time
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
			lots = append(lots, openLot{qty: qty, price: price, openedAt: f.TS})
		case dir == -1:
			for len(lots) > 0 && qty > 0 {
				lot := lots[0]
				matchQty := math.Min(lot.qty, qty)
				// PnL for a long: (sell - buy) * qty; for a short: reverse.
				pnl := (price - lot.price) * matchQty
				pct := (price - lot.price) / lot.price
				out = append(out, TradeOutcome{
					Symbol: sym,
					PnL:    pnl,
					Pct:    pct,
					Hold:   f.TS.Sub(lot.openedAt),
					Opened: lot.openedAt,
					Closed: f.TS,
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
}

// refreshTrades walks the journal and updates the ledger with all
// closed round-trips. Idempotent on the same fill set. Run this at
// startup to rebuild the ledger from history.
func (l *statsLedger) refreshFromJournal(j *pnl.Journal) error {
	recs, err := j.Records()
	if err != nil {
		return err
	}
	// Reset cache for symbols that have fills in the journal; we
	// rebuild from scratch to avoid double-counting on restarts.
	fillsBySym := map[string][]pnl.Record{}
	for _, r := range recs {
		sym := r.Symbol
		if sym == "" {
			continue
		}
		fills := fillsBySym[sym]
		fills = append(fills, r)
		fillsBySym[sym] = fills
		outcomes := closedRoundTrips(fills)
		for _, o := range outcomes {
			l.recordClose(o.Symbol, o.Pct, o.PnL, o.Hold)
		}
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
