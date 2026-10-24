package learning

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
)

// Store persists the computed Adjustments beside the trade log, so the
// dashboard API (a separate process) shows exactly what the trading bot
// is applying rather than a recomputed guess.
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore derives the path from the trade-log path
// (data/trades.jsonl -> data/learning-state.json).
func NewStore(tradeLogPath string) *Store {
	dir := filepath.Dir(tradeLogPath)
	if dir == "" || dir == "." {
		dir = "data"
	}
	return &Store{path: filepath.Join(dir, "learning-state.json")}
}

// Path returns the backing file.
func (s *Store) Path() string { return s.path }

// Load returns the stored adjustments; a missing file yields an inactive
// snapshot rather than an error, because "no history yet" is normal.
func (s *Store) Load() (Adjustments, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var a Adjustments
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return Adjustments{Classes: map[string]ClassAdjustment{}}, nil
		}
		return Adjustments{}, fmt.Errorf("read %s: %w", s.path, err)
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return Adjustments{}, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if a.Classes == nil {
		a.Classes = map[string]ClassAdjustment{}
	}
	// Defensive re-clamp on read: the file is on disk and could have been
	// hand-edited. A tampered value must not scale a position up.
	a.ScoreFloorAdd = clampNonNeg(a.ScoreFloorAdd)
	for k, c := range a.Classes {
		c.SizeMultiplier = clampUnit(c.SizeMultiplier)
		c.StopMultiplier = clampUnit(c.StopMultiplier)
		a.Classes[k] = c
	}
	return a, nil
}

// Save writes the snapshot atomically.
func (s *Store) Save(a Adjustments) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Classes == nil {
		a.Classes = map[string]ClassAdjustment{}
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func clampUnit(x float64) float64 {
	// Affirmative test: NaN fails both `x <= 0` and `x > 1`, so a
	// rejection-style guard would wave it through and a NaN multiplier
	// would reach the trade. Anything not in (0,1] means "no opinion".
	if x > 0 && x <= 1 && !math.IsInf(x, 0) {
		return x
	}
	return 1
}

func clampNonNeg(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, -1) {
		return 0
	}
	if x < 0 {
		return 0
	}
	return x
}
