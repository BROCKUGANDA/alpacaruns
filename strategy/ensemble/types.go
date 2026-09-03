// Package ensemble implements the Layer-2 multi-strategy expert layer on
// top of the deterministic factor engine. Multiple independent experts
// score the same batch-fetched market data; a performance-weighted gater
// stacks their votes per symbol; a risk-budget module sizes and vetoes
// entries before the existing executor runs. No LLM anywhere.
//
// Data flows ONE way: bars are fetched ONCE per tick by the caller,
// packaged into MarketData, and shared read-only across every expert.
package ensemble

import (
	"context"
	"sort"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// Action is an expert's directional verdict for one symbol.
type Action string

const (
	ActionBuy  Action = "buy"
	ActionSell Action = "sell" // exit a held position
	ActionHold Action = "hold"
)

// RegimeTag labels the market regime an expert believes it is acting in.
type RegimeTag string

const (
	RegimeTrending     RegimeTag = "trending"
	RegimeRanging      RegimeTag = "ranging"
	RegimeVolExpansion RegimeTag = "vol-expansion"
	RegimeCalendarOnly RegimeTag = "calendar-only"
)

// Signal is one expert's conviction for one symbol on one tick.
type Signal struct {
	Symbol     string
	Action     Action
	Confidence float64   // 0..1, the expert's own conviction
	Regime     RegimeTag // which regime the signal assumes
	Reason     string    // human-readable, journaled verbatim

	// ExpertName stamps the emitting voice; set by the ensemble runner
	// so the performance tracker can attribute outcomes without every
	// expert having to fill it in.
	ExpertName string `json:"expert,omitempty"`
}

// Expert is one strategy voice in the ensemble. Evaluate receives the
// SHARED market dataset (batch-fetched once per tick); experts MUST NOT
// perform network I/O of their own.
type Expert interface {
	Name() string
	Evaluate(ctx context.Context, symbol string, data MarketData) ([]Signal, error)
}

// SymbolData is the derived per-symbol dataset every expert reads.
type SymbolData struct {
	Bars        []tools.Bar // daily bars ascending by Time
	Closes      []float64   // convenience: Bars[i].Close
	ATR14       float64     // average true range over the last 14 sessions
	RealizedVol float64     // stdev of the last 20 daily returns (per-session)
}

// MarketData carries the batch-fetched bars plus everything each expert
// needs, shared across all experts on one tick.
type MarketData struct {
	// Symbol is the target of THIS Evaluate call.
	Symbol string
	// Symbols holds the dataset for the entire universe (including the
	// benchmark), keyed by symbol. Read-only.
	Symbols map[string]*SymbolData
	// Held marks currently-open positions (exits apply only to these).
	Held map[string]bool
	// Now anchors calendar effects (turn-of-month, day-of-week).
	Now time.Time
	// benchmark is the vol-regime reference symbol (default SPY).
	benchmark string
}

// UniverseExpert marks experts that consume the WHOLE dataset per call
// (cross-sectional ranking, pairs) rather than one symbol at a time;
// the runner invokes them exactly once per tick.
type UniverseExpert interface {
	WholeUniverse() bool
}

// Universe returns the sorted symbol list present in the dataset
// (benchmark included so it is tradable/scorable like any other).
func (m MarketData) Universe() []string {
	syms := make([]string, 0, len(m.Symbols))
	for sym := range m.Symbols {
		if sd := m.Symbols[sym]; sd != nil && len(sd.Bars) > 0 {
			syms = append(syms, sym)
		}
	}
	sort.Strings(syms)
	return syms
}

// Benchmark returns the vol-regime reference symbol.
func (m MarketData) Benchmark() string {
	if m.benchmark == "" {
		return "SPY"
	}
	return m.benchmark
}

// WithBenchmark returns a copy of m pointing at the given benchmark.
func (m MarketData) WithBenchmark(sym string) MarketData {
	m.benchmark = sym
	return m
}

// SD returns the dataset for sym, or nil when absent (caller decides
// whether absence is fatal).
func (m MarketData) SD(sym string) *SymbolData { return m.Symbols[sym] }
