// Package selfheal persists the last pass so the dashboard API — a
// separate process — can show what the trading bot found and fixed
// without running its own broker reads.
package selfheal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ReportStore writes and reads the most recent Report.
type ReportStore struct {
	mu   sync.Mutex
	path string
}

// NewReportStore derives the path from the trade-log path
// (data/trades.jsonl -> data/selfheal-report.json).
func NewReportStore(tradeLogPath string) *ReportStore {
	dir := filepath.Dir(tradeLogPath)
	if dir == "" || dir == "." {
		dir = "data"
	}
	return &ReportStore{path: filepath.Join(dir, "selfheal-report.json")}
}

// Path returns the backing file.
func (s *ReportStore) Path() string { return s.path }

// Save writes the report atomically.
func (s *ReportStore) Save(r Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(r, "", "  ")
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

// Load returns the last stored report; a missing file is an empty
// report, not an error — "the pass has not run yet" is a real state the
// dashboard should show rather than hide behind a 500.
func (s *ReportStore) Load() (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return Report{}, nil
		}
		return Report{}, fmt.Errorf("read %s: %w", s.path, err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return Report{}, fmt.Errorf("parse %s: %w", s.path, err)
	}
	return r, nil
}
