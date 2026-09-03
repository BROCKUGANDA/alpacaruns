// journal.go — read the trade log (data/trades.jsonl) the same way
// pnl/journal.go writes it, but without holding a write handle. The
// server re-reads the file on every request: it's small (< few MB
// for a paper-trade month) and never grows unbounded thanks to the
// bot's TS-first append layout. A read-time mtime cache is used only
// inside one request, never across requests, so a freshly appended
// record shows up on the next request without delay.
package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/pnl"
)

// readAllRecords loads the trade log at path, oldest first. Missing
// file is not an error — the bot creates it on first cycle, so a
// freshly-cloned VPS returns an empty list rather than 500'ing.
func readAllRecords(path string) ([]pnl.Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open trade log %s: %w", path, err)
	}
	defer f.Close()

	var out []pnl.Record
	sc := bufio.NewScanner(f)
	// Allow long ensemble decision rows (vote trails can run >64 KiB).
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r pnl.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			// Corrupt line: skip and continue. The dashboard would
			// otherwise show a misleading 500 when one bad row lands
			// in the log. The bot's own readers fail loudly because
			// they need to journal; readers can be lenient.
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return out, fmt.Errorf("scan trade log %s: %w", path, err)
	}
	return out, nil
}

// readFills returns only kind=fill records in [since, until]. The bot
// does not write times outside the JSONL convention so TS comparisons
// are safe.
func readFills(records []pnl.Record, since, until time.Time) []pnl.Record {
	var out []pnl.Record
	for _, r := range records {
		if r.Kind != pnl.KindFill {
			continue
		}
		if !since.IsZero() && r.TS.Before(since) {
			continue
		}
		if !until.IsZero() && r.TS.After(until) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// readDecisions returns only kind=decision | kind=reconcile records.
// The "path" argument maps to the journal's Source field:
//   - "agent"     -> strategy:auto / strategy:ensemble / monitor
//   - "ensemble"  -> strategy:ensemble
//   - "manual"    -> cli:trade / cli:chain / cli:factors
//
// Empty path returns every decision record.
func readDecisions(records []pnl.Record, path string, since, until time.Time) []pnl.Record {
	var out []pnl.Record
	for _, r := range records {
		if r.Kind != pnl.KindDecision && r.Kind != pnl.KindReconcile {
			continue
		}
		if path != "" && !sourceMatchesPath(r.Source, path) {
			continue
		}
		if !since.IsZero() && r.TS.Before(since) {
			continue
		}
		if !until.IsZero() && r.TS.After(until) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// sourceMatchesPath is a small taxonomy: the journal's Source is
// "subsystem:verb" (strategy:auto, cli:trade, ...). The dashboard
// groups them into "agent", "ensemble", "manual" so the UI can keep
// a stable vocabulary even when new subsystems are added.
func sourceMatchesPath(source, want string) bool {
	switch want {
	case "agent":
		return source == "strategy:auto" || source == "monitor"
	case "ensemble":
		return source == "strategy:ensemble"
	case "manual":
		return source == "cli:trade" || source == "cli:chain" || source == "cli:factors"
	}
	// Unknown bucket: treat as exact match.
	return source == want
}

// readStateJSON reads data/strategy-state.json (a flat key/value
// object). Missing file or partial fields return zero values.
type strategyState struct {
	PeakEquity    float64   `json:"peak_equity"`
	WeekStart     time.Time `json:"week_start"`
	StartingEquity float64  `json:"starting_equity"`
}

func readStrategyState(path string) strategyState {
	var s strategyState
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(b, &s)
	return s
}

// ensureDataDir creates the data/ directory tree lazily so a fresh
// /api/control/* write doesn't 500 when the bot hasn't booted yet.
func ensureDataDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}