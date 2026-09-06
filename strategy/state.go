package strategy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	// Track distinguishes the intraday book from the swing book when
	// both hold the same symbol. Levels are keyed by the PLAIN symbol
	// (no prefix): broker positions, the monitor's TP/SL matcher and the
	// dashboard's since-lookup all match on the venue symbol, and a
	// prefixed key ("intraday:BTC/USD") never matched any of them —
	// which silently disabled intraday TP/SL enforcement and wiped the
	// intraday held-map on every monitor tick. Empty = swing (default).
	Track string `json:"track,omitempty"`
}

// State is the persisted strategy-state.json document.
type State struct {
	Version int                        `json:"version"`
	Levels  map[string]*PositionLevels `json:"levels"`
	// PeakEquity tracks the running high-water mark for total drawdown.
	PeakEquity float64 `json:"peak_equity,omitempty"`
	// WeekStart is the Monday 00:00 UTC timestamp anchoring weekly P/L.
	WeekStart time.Time `json:"week_start,omitempty"`
	// TickNumber is a monotonically increasing counter bumped on every
	// bot tick. It is the only cross-process heartbeat the dashboard
	// API has access to (the bot and `serve` are separate processes
	// that share data/strategy-state.json, not in-memory atomics).
	// The dashboard renders "Tick N" from it; the API treats a fresh
	// LastTick — within 3x the poll interval — as proof the bot is alive.
	TickNumber int64 `json:"tick_number,omitempty"`
	// LastTick is the UTC timestamp of the most recent completed bot
	// tick. Zero until the bot's first tick writes it.
	LastTick time.Time `json:"last_tick,omitempty"`
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
	migrateIntradayPrefix(&st)
	return &st, nil
}

// migrateIntradayPrefix rewrites legacy "intraday:SYM" level keys into
// plain-symbol keys with Track="intraday" (the pre-track layout never
// matched broker symbols in the monitor, so those TP/SL levels were
// dead). Runs on every load; the rewrite is idempotent and the next
// Save persists it.
func migrateIntradayPrefix(st *State) {
	for key, lv := range st.Levels {
		if !strings.HasPrefix(key, "intraday:") {
			continue
		}
		base := strings.TrimPrefix(key, "intraday:")
		delete(st.Levels, key)
		if base == "" {
			continue
		}
		if lv != nil {
			lv.Symbol = base
			lv.Track = "intraday"
		}
		st.Levels[base] = lv
	}
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
	return s.ClearLevelTrack(symbol, "")
}

// ClearLevelTrack removes levels for a symbol ONLY when they belong to
// the given track ("" = swing, "intraday"). A swing exit must not clear
// an intraday bracket on the same symbol, and vice versa; the plain
// ClearLevel keeps its historical swing-only semantics.
func (s *StateStore) ClearLevelTrack(symbol, track string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	if lv, ok := st.Levels[symbol]; ok && lv.Track == track {
		delete(st.Levels, symbol)
	}
	return s.saveLocked(st)
}

// Heartbeat bumps the tick counter and stamps the last-tick time.
// Called once per bot tick so the dashboard API can derive liveness
// without any in-memory sharing (bot and serve run as two processes).
// Atomic: load → mutate → save occupy the same mutex naturally, so a
// concurrent SetLevel from the same tick cannot clobber the heartbeat.
func (s *StateStore) Heartbeat(tickNum int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	st.TickNumber = tickNum
	st.LastTick = time.Now().UTC()
	return s.saveLocked(st)
}

// CryptoLevels returns locally-enforced levels for every open crypto
// position, sorted by symbol for deterministic iteration. Intraday-track
// levels are EXCLUDED from the swing monitor pass: the intraday book
// flattens EOD and manages its own exits; a shared pass would close a
// swing position through an intraday bracket (or vice versa).
func (s *StateStore) CryptoLevels() ([]*PositionLevels, error) {
	return s.cryptoLevelsByTrack("")
}

// CryptoLevelsForTrack returns locally-enforced crypto levels for one
// track: "" = swing, "intraday" = the intraday book. Used by the
// per-track monitor passes so each book enforces only its own brackets.
func (s *StateStore) CryptoLevelsForTrack(track string) ([]*PositionLevels, error) {
	return s.cryptoLevelsByTrack(track)
}

func (s *StateStore) cryptoLevelsByTrack(track string) ([]*PositionLevels, error) {
	st, err := s.Load()
	if err != nil {
		return nil, err
	}
	var out []*PositionLevels
	for _, lv := range st.Levels {
		if lv.Crypto && lv.Track == track {
			out = append(out, lv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

func (s *StateStore) save(st *State) error { return s.Save(st) }
