package strategy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/tools"
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
	// Side is the POSITION side ("long" or "short"). TP/SL sit above the
	// entry for a long and below it for a short, and closing a short is
	// a buy — without this the monitor assumes long and would place an
	// exit that doubles the exposure.
	Side string `json:"side,omitempty"`
	// AssetClass is the broker's class (us_equity, us_option, crypto).
	// It decides whether protection is a broker-side order pair or a
	// locally enforced level; the symbol text alone is not enough now
	// that the universe spans every class.
	AssetClass string `json:"asset_class,omitempty"`
	// Track distinguishes the intraday book from the swing book when
	// both hold the same symbol. Levels are keyed by the PLAIN symbol
	// (no prefix): broker positions, the monitor's TP/SL matcher and the
	// dashboard's since-lookup all match on the venue symbol, and a
	// prefixed key ("intraday:BTC/USD") never matched any of them —
	// which silently disabled intraday TP/SL enforcement and wiped the
	// intraday held-map on every monitor tick. Empty = swing (default).
	Track string `json:"track,omitempty"`
}

// IsShort reports whether this level protects a short position.
func (p *PositionLevels) IsShort() bool {
	return strings.EqualFold(strings.TrimSpace(p.Side), "short")
}

// LocallyEnforced reports whether TP/SL for this level are enforced by
// the monitor rather than by the broker. Alpaca supports bracket/OCO
// order classes for equities only, so crypto pairs and option contracts
// always live here.
func (p *PositionLevels) LocallyEnforced() bool {
	return p.Crypto || p.AssetClass == tools.AssetClassOption || tools.IsOCCSymbol(p.Symbol)
}

// State is the persisted strategy-state.json document.
//
// WRITE OWNERSHIP: this document belongs to the trading bot. `alpacaruns
// auto` is the only process that writes it; the dashboard API reads it and
// writes its own knobs to config-overrides.json (see ConfigOverrides).
// Sharing a read-modify-write of this file across two processes lost
// TP/SL levels and the drawdown peak whenever the API renamed a stale
// snapshot over a fresher bot write — there is no flock between them.
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
	// StartingEquity is the adaptive-sizing baseline (the account equity
	// first seen with ADAPTIVE_SIZING on) and the P&L reference the
	// dashboard plots against. It is a real field, not a side-channel
	// key: the bot used to patch it into the file with a raw map write,
	// and the next typed Save — the per-tick Heartbeat — marshalled the
	// struct and dropped it, resetting the baseline every tick. The JSON
	// key name is unchanged ("starting_equity") because the live file on
	// the server already carries it and api/journal.go reads it.
	StartingEquity float64 `json:"starting_equity,omitempty"`

	// unknownKeys holds top-level keys this build cannot decode, captured
	// verbatim by load() and re-attached verbatim by marshalState(). It is
	// unexported so encoding/json never treats it as part of the schema.
	unknownKeys map[string]json.RawMessage
	// diskVersion is the "version" stamp found on disk. marshalState keeps
	// it when it is newer than stateVersion instead of downgrading.
	diskVersion int
}

// stateVersion is the schema version this build writes for a fresh file.
// Additive fields do not need a bump — unknown keys and a newer stamp on
// disk are preserved through a rewrite (see load + marshalState).
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
	// Unknown (future) version: load it and keep every key we cannot decode
	// (marshalState re-attaches them verbatim). Erroring would stop TP/SL
	// enforcement over a field this build merely does not know, and the old
	// silent downgrade deleted data on rewrite.
	st.unknownKeys = unknownTopLevelKeys(b)
	st.diskVersion = st.Version
	migrateLevelKeys(&st)
	return &st, nil
}

// stateJSONKeys is the set of top-level keys this build's State knows how
// to decode, derived from the struct tags so a newly added field is
// automatically "known" and never re-emitted as an opaque blob.
var stateJSONKeys = func() map[string]bool {
	t := reflect.TypeOf(State{})
	out := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported — never part of the document
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out[name] = true
	}
	return out
}()

// unknownTopLevelKeys returns every top-level key of the raw document that
// stateJSONKeys does not cover, with its original bytes. A document that
// is not a JSON object (or that has no extras) yields nil.
func unknownTopLevelKeys(b []byte) map[string]json.RawMessage {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil
	}
	var out map[string]json.RawMessage
	for k, raw := range doc {
		if stateJSONKeys[k] {
			continue
		}
		if out == nil {
			out = map[string]json.RawMessage{}
		}
		out[k] = raw
	}
	return out
}

// marshalState renders a State for disk. Keys this build does not know are
// re-attached verbatim, and the version stamp is only ever raised to what
// is already on disk — an older binary rewriting a newer file must lose
// nothing.
func marshalState(st *State) ([]byte, error) {
	version := stateVersion
	if st.diskVersion > version {
		version = st.diskVersion
	}
	st.Version = version
	if len(st.unknownKeys) == 0 {
		return json.MarshalIndent(st, "", "  ")
	}
	base, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(base, &doc); err != nil {
		return nil, err
	}
	for k, raw := range st.unknownKeys {
		if _, claimed := doc[k]; !claimed {
			doc[k] = raw
		}
	}
	vb, err := json.Marshal(version)
	if err != nil {
		return nil, err
	}
	doc["version"] = vb
	return json.MarshalIndent(doc, "", "  ")
}

// levelKey is the map key for a position's TP/SL levels. The swing book
// uses the plain venue symbol; any other track is suffixed, so the
// intraday and swing books can hold the same symbol at the same time
// without one clobbering the other's levels.
func levelKey(symbol, track string) string {
	if track == "" {
		return symbol
	}
	return symbol + "|" + track
}

// migrateLevelKeys rewrites legacy level keys into the track-aware
// layout. Two legacy shapes existed: "intraday:SYM" prefixed keys, and
// plain keys carrying a non-empty Track. Both are normalised here so an
// intraday entry can no longer overwrite the swing book's levels for
// the same symbol.
func migrateLevelKeys(st *State) {
	for key, lv := range st.Levels {
		if lv == nil {
			delete(st.Levels, key)
			continue
		}
		base := strings.TrimSpace(lv.Symbol)
		if strings.HasPrefix(key, "intraday:") {
			base = strings.TrimPrefix(key, "intraday:")
			lv.Symbol = base
			lv.Track = "intraday"
		}
		if base == "" {
			delete(st.Levels, key)
			continue
		}
		lv.Symbol = base
		want := levelKey(base, lv.Track)
		if want == key {
			continue
		}
		delete(st.Levels, key)
		st.Levels[want] = lv
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
	if st.Levels == nil {
		st.Levels = map[string]*PositionLevels{}
	}
	b, err := marshalState(st)
	if err != nil {
		return err
	}
	tmp := s.tmpPath()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		// Never leave a half-written scratch file behind: the next
		// process would have no way to tell it from a live one.
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// tmpPath is the scratch file the atomic rename publishes from. It is
// process-scoped (".state-<pid>.tmp") rather than the shared
// "<path>.tmp" the dashboard used to write the same document through —
// two processes renaming through one temp name is how a stale snapshot
// landed on top of a fresh one.
func (s *StateStore) tmpPath() string {
	return fmt.Sprintf("%s.state-%d.tmp", s.path, os.Getpid())
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
	st.Levels[levelKey(lv.Symbol, lv.Track)] = &lv
	return s.saveLocked(st)
}

// Level returns the stored levels for one symbol in one track
// ("" = swing), or nil when nothing protects that position. This is the
// lookup the reconciliation pass uses to tell "already covered" from
// "the broker holds a position we have no levels for".
func (s *StateStore) Level(symbol, track string) *PositionLevels {
	st, err := s.Load()
	if err != nil {
		return nil
	}
	key := levelKey(CanonicalSymbol(symbol), track)
	lv, ok := st.Levels[key]
	if !ok || lv == nil {
		return nil
	}
	cp := *lv
	return &cp
}

// LevelAnyTrack returns the stored levels for a symbol in ANY track,
// and the track that owns them. The broker reports one aggregated
// position per symbol, so this is how the monitor knows a position is
// already someone's exposure: a swing pass must not adopt a symbol the
// intraday book is guarding, or both books would size, protect and
// close the same shares.
func (s *StateStore) LevelAnyTrack(symbol string) (*PositionLevels, string) {
	st, err := s.Load()
	if err != nil {
		return nil, ""
	}
	want := CanonicalSymbol(symbol)
	for _, lv := range st.Levels {
		if lv == nil || CanonicalSymbol(lv.Symbol) != want {
			continue
		}
		cp := *lv
		return &cp, lv.Track
	}
	return nil, ""
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
	key := levelKey(symbol, track)
	if lv, ok := st.Levels[key]; ok && lv.Track == track {
		delete(st.Levels, key)
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

// ReadStartingEquity returns the persisted adaptive-sizing baseline, or 0
// when none is stored. Read through the typed store so it can never drift
// from what the next Save re-emits.
func (s *StateStore) ReadStartingEquity() (float64, error) {
	st, err := s.Load()
	if err != nil {
		return 0, err
	}
	return st.StartingEquity, nil
}

// SetStartingEquity records the adaptive-sizing baseline (the equity the
// P&L curve and the adaptive cap are measured against). It goes through
// the typed StateStore, so the value is part of every later Save — the old
// raw map patch in `cmd/alpacaruns auto` wrote a key the struct did not
// have, and the next typed Save (the per-tick Heartbeat) dropped it.
func (s *StateStore) SetStartingEquity(equity float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	st.StartingEquity = equity
	return s.saveLocked(st)
}

// SetStartingEquityIfUnset stores the baseline only when nothing is stored
// yet, and reports whether it wrote. Read-and-write happen under the
// store's lock, so two ticks that both see "no baseline" cannot each
// persist a different equity.
func (s *StateStore) SetStartingEquityIfUnset(equity float64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return false, err
	}
	if st.StartingEquity != 0 {
		return false, nil
	}
	st.StartingEquity = equity
	if err := s.saveLocked(st); err != nil {
		return false, err
	}
	return true, nil
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
		// Every class the broker cannot protect for itself: crypto has
		// no order classes, and Alpaca rejects brackets on options too.
		// Filtering on Crypto alone left every option position with no
		// take-profit and no stop at all.
		if lv.LocallyEnforced() && lv.Track == track {
			out = append(out, lv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

func (s *StateStore) save(st *State) error { return s.Save(st) }

// ---- dashboard-owned config overrides ----

// ConfigOverridesFileName is the dashboard's own knobs document. It lives
// next to strategy-state.json ("data/config-overrides.json") and is the
// ONLY file the API process writes for configuration; the bot reads it and
// never writes it, and the API never writes strategy-state.json. One
// writer per file, so no cross-process read-modify-write can lose a
// TP/SL level, a drawdown peak, or an operator's risk cap.
const ConfigOverridesFileName = "config-overrides.json"

// ConfigOverrides is the full schema of that file. It is written whole on
// every save (no merge with the bot's document) and read whole by the bot.
//
//	{
//	  "max_position_usd":        number, USD cap per equity position
//	  "max_portfolio_pct":       number, [0,1] share of portfolio per name
//	  "crypto_max_position_usd": number, USD cap per crypto position
//	  "min_confidence":          number, [0,1] signal floor to act
//	  "daily_dd_halt":           number, [0,1] daily drawdown kill level
//	  "weekly_dd_halt":          number, [0,1] weekly drawdown kill level
//	  "total_dd_halt":           number, [0,1] total drawdown kill level
//	  "updated_at":              string, RFC3339Nano UTC of the last write
//	}
//
// Every key is always present (no omitempty). A consumer that cannot tell
// "operator set 0" from "never written" should treat <= 0 as "not set —
// keep the env/compiled default", which is what the dashboard's own
// validation already enforces (caps must be > 0, fractions in [0,1]).
type ConfigOverrides struct {
	MaxPositionUSD       float64   `json:"max_position_usd"`
	MaxPortfolioPct      float64   `json:"max_portfolio_pct"`
	CryptoMaxPositionUSD float64   `json:"crypto_max_position_usd"`
	MinConfidence        float64   `json:"min_confidence"`
	DailyDDHalt          float64   `json:"daily_dd_halt"`
	WeeklyDDHalt         float64   `json:"weekly_dd_halt"`
	TotalDDHalt          float64   `json:"total_dd_halt"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// ConfigOverridesPath derives the override path from any data-dir path —
// the state file or the trade log — so no new flag has to be plumbed:
// data/strategy-state.json -> data/config-overrides.json.
func ConfigOverridesPath(stateOrTradeLogPath string) string {
	fallback := filepath.Join("data", ConfigOverridesFileName)
	p := strings.TrimSpace(stateOrTradeLogPath)
	if p == "" {
		return fallback
	}
	dir := filepath.Dir(p)
	if dir == "" || dir == "." {
		return fallback
	}
	return filepath.Join(dir, ConfigOverridesFileName)
}

// LoadConfigOverrides reads the dashboard's overrides. A missing file is
// not an error — a fresh box has no overrides and must fall back to env
// defaults. A corrupt file IS an error: silently zeroing an operator's
// risk caps is worse than refusing to apply them.
func LoadConfigOverrides(path string) (*ConfigOverrides, error) {
	if strings.TrimSpace(path) == "" {
		return &ConfigOverrides{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ConfigOverrides{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return &ConfigOverrides{}, nil
	}
	var ov ConfigOverrides
	if err := json.Unmarshal(b, &ov); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &ov, nil
}

// SaveConfigOverrides writes the document atomically: mkdir, write to a
// process-scoped scratch name, rename. The scratch suffix carries
// "overrides" plus the pid so it can never collide with the bot's
// "<state>.state-<pid>.tmp" scratch file — or with an older API build's
// "<state>.tmp".
func SaveConfigOverrides(path string, ov ConfigOverrides) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("config overrides: empty path")
	}
	if ov.UpdatedAt.IsZero() {
		ov.UpdatedAt = time.Now().UTC()
	} else {
		ov.UpdatedAt = ov.UpdatedAt.UTC()
	}
	b, err := json.MarshalIndent(ov, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.overrides-%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
