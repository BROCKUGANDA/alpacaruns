package ensemble

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// perf.go: rolling per-expert hit-rate over resolved signals.
//
// A pending signal resolves when, within ResolveSessions (5) sessions of
// entry, price moved favorably by >= 0.5x ATR. Pending signals persist
// in the ensemble state file alongside strategy-state.json so restarts
// keep the tracker honest. New experts start at the neutral 0.5
// hit-rate.

const (
	defaultWindow        = 30 // trailing resolved signals per expert
	resolveSessions      = 5  // max sessions for a signal to resolve
	resolveATRFraction   = 0.5 // favorable move >= this fraction of ATR
	neutralHitRate       = 0.5
	maxTrackedPending    = 500 // safety cap on the pending queue
)

// PendingSignal is an unresolved expert recommendation awaiting a
// favorable/stop move within its resolution window.
type PendingSignal struct {
	Expert    string    `json:"expert"`
	Symbol    string    `json:"symbol"`
	Action    Action    `json:"action"`
	Entry     float64   `json:"entry"`     // mark at emit time
	ATR       float64   `json:"atr"`       // ATR at emit time
	EmittedAt time.Time `json:"emitted_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PerfState is the JSON document persisted next to strategy-state.json.
type PerfState struct {
	Version int                       `json:"version"`
	Pending []PendingSignal           `json:"pending,omitempty"`
	History map[string][]bool         `json:"history"` // expert -> outcomes, oldest first
}

// Tracker keeps rolling hit-rates and pending signals. Safe for
// concurrent use.
type Tracker struct {
	mu          sync.Mutex
	window      int
	history     map[string][]bool
	pending     []PendingSignal
	path        string // empty = in-memory only (tests)
}

// NewTracker builds a tracker with a trailing window of window results
// per expert. path may be empty to skip persistence.
func NewTracker(window int, path string) *Tracker {
	if window <= 0 {
		window = defaultWindow
	}
	return &Tracker{window: window, history: map[string][]bool{}, path: path}
}

// Load reads persisted state into the tracker; missing file is not an
// error (fresh install).
func (t *Tracker) Load() error {
	if t.path == "" {
		return nil
	}
	b, err := os.ReadFile(t.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var st PerfState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st.History != nil {
		t.history = st.History
	}
	t.pending = st.Pending
	return nil
}

// Save persists state atomically; a no-op when no path configured.
func (t *Tracker) Save() error {
	if t.path == "" {
		return nil
	}
	t.mu.Lock()
	st := PerfState{Version: 1,
		Pending: append([]PendingSignal(nil), t.pending...),
		History: t.history}
	t.mu.Unlock()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}

// Record appends a Buy/Sell signal as pending (Hold is ignored).
func (t *Tracker) Record(sig Signal, entry, atr float64, now time.Time) {
	if sig.Action != ActionBuy && sig.Action != ActionSell {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = append(t.pending, PendingSignal{
		Expert: sig.ExpertName, Symbol: sig.Symbol, Action: sig.Action,
		Entry: entry, ATR: atr, EmittedAt: now,
		ExpiresAt: now.Add(resolveSessions * 24 * time.Hour),
	})
	if n := len(t.pending); n > maxTrackedPending*lenOrOne(t.history) {
		// Global cap: drop oldest half to keep the file bounded.
		t.pending = append([]PendingSignal(nil), t.pending[n/2:]...)
	}
}

// Resolve settles every expired pending signal against closes
// (symbol -> latest close series). Favorable move >= resolveATRFraction
// x ATR within the window counts a hit; expiry without one counts a
// miss. Returns the number resolved.
func (t *Tracker) Resolve(closes map[string][]float64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	var still []PendingSignal
	resolved := 0
	for _, p := range t.pending {
		hit, done := evaluate(p, closes[p.Symbol], now)
		if !done {
			still = append(still, p)
			continue
		}
		resolved++
		h := append(t.history[p.Expert], hit)
		if len(h) > t.window {
			h = h[len(h)-t.window:]
		}
		t.history[p.Expert] = h
	}
	t.pending = still
	return resolved
}

func evaluate(p PendingSignal, closes []float64, now time.Time) (hit, done bool) {
	if len(closes) == 0 {
		return false, false
	}
	target := favorableMove(p.Action, p.Entry, p.ATR*resolveATRFraction)
	for _, c := range closes {
		if movedFavorably(p.Action, p.Entry, c, target) {
			return true, true
		}
	}
	return false, !now.Before(p.ExpiresAt)
}

func movedFavorably(a Action, entry, px, target float64) bool {
	switch a {
	case ActionBuy:
		return px-entry >= target
	case ActionSell:
		return entry-px >= target
	}
	return false
}

func favorableMove(a Action, entry, target float64) float64 { return target }

// HitRate returns the trailing hit rate of expert; neutral 0.5 when
// fewer than minSamples resolved results exist.
func (t *Tracker) HitRate(expert string, minSamples int) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.history[expert]
	if len(h) == 0 || len(h) < minSamples {
		return neutralHitRate
	}
	var wins int
	for _, v := range h {
		if v {
			wins++
		}
	}
	return float64(wins) / float64(len(h))
}

// Samples reports how many resolved results are tracked for expert.
func (t *Tracker) Samples(expert string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.history[expert])
}

func lenOrOne(m map[string][]bool) int {
	if len(m) == 0 {
		return 8
	}
	return max(8, len(m))
}
