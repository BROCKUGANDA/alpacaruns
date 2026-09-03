package strategy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// TP/SL levels persisted next to the trade log so restarts keep enforcing
// them. Equities rely on Alpaca server-side bracket orders; this local
// book exists for CRYPTO positions (brackets unsupported on crypto) and
// as an audit trail of intended levels for every entry.
type PositionLevels struct {
	Symbol     string    `json:"symbol"` // venue symbol, e.g. BTC/USD or AAPL
	EntryPrice float64   `json:"entry_price"`
	TakeProfit float64   `json:"take_profit"`
	StopLoss   float64   `json:"stop_loss"`
	Qty        string    `json:"qty"`    // broker-side qty string at entry
	Crypto     bool      `json:"crypto"` // locally-enforced when true
	Since      time.Time `json:"since"`
}

// State is the persisted strategy-state.json document.
type State struct {
	Version int                        `json:"version"`
	Levels  map[string]*PositionLevels `json:"levels"`
	// PeakEquity tracks the running high-water mark for total drawdown.
	PeakEquity float64 `json:"peak_equity,omitempty"`
	// WeekStart is the Monday 00:00 UTC timestamp anchoring weekly P/L.
	WeekStart time.Time `json:"week_start,omitempty"`
}

const stateVersion = 1

// StateStore persists strategy state to a small JSON file (default
// alongside TRADE_LOG: strategy-state.json). Safe for concurrent use.
type StateStore struct {
	mu   sync.Mutex
	path string
}

// NewStateStore derives the state path from the trade-log path so both
// live together (data/trades.jsonl -> data/strategy-state.json).
func NewStateStore(tradeLogPath string) *StateStore {
	dir := filepath.Dir(tradeLogPath)
	return &StateStore{path: filepath.Join(dir, "strategy-state.json")}
}

// Path returns the backing file path.
func (s *StateStore) Path() string { return s.path }

// Load reads the state file; a missing file yields a fresh empty state.
func (s *StateStore) Load() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *StateStore) load() (*State, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{Version: stateVersion, Levels: map[string]*PositionLevels{}}, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if st.Levels == nil {
		st.Levels = map[string]*PositionLevels{}
	}
	if st.Version != stateVersion && st.Version != 0 {
		// Unknown future version: keep loading (fields are additive) but
		// rewrite will downgrade to what we know.
		st.Version = stateVersion
	}
	return &st, nil
}

// Save atomically writes the state file (write temp + rename).
func (s *StateStore) Save(st *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(st)
}

// saveLocked writes the file; caller must hold s.mu.
func (s *StateStore) saveLocked(st *State) error {
	st.Version = stateVersion
	if st.Levels == nil {
		st.Levels = map[string]*PositionLevels{}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// SetLevel records/updates one position's TP/SL levels.
func (s *StateStore) SetLevel(lv PositionLevels) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	lv.Crypto = IsCrypto(lv.Symbol)
	st.Levels[lv.Symbol] = &lv
	return s.saveLocked(st)
}

// ClearLevel removes levels for a symbol (after exit fills).
func (s *StateStore) ClearLevel(symbol string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	delete(st.Levels, symbol)
	return s.saveLocked(st)
}

// CryptoLevels returns locally-enforced levels for every open crypto
// position, sorted by symbol for deterministic iteration.
func (s *StateStore) CryptoLevels() ([]*PositionLevels, error) {
	st, err := s.Load()
	if err != nil {
		return nil, err
	}
	var out []*PositionLevels
	for _, lv := range st.Levels {
		if lv.Crypto {
			out = append(out, lv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

func (s *StateStore) save(st *State) error { return s.Save(st) }
