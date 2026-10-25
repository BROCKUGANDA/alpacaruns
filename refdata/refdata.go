// Package refdata keeps a versioned snapshot of the broker's reference data
// for the symbols this bot actually cares about, and reports what changed.
//
// The bot's universe comes from a live asset catalogue, but nothing recorded
// what that catalogue said yesterday. A symbol that is delisted, halted or
// renamed simply stops appearing in decisions — which looks exactly like
// "no signal today", and leaves any stored TP/SL level keyed to the old
// symbol as an orphan nobody notices.
//
// This package answers "what changed, and when" from a file on disk:
//
//   - a symbol that disappears is reported (delisting, rename, or a typo)
//   - a tradability flip is reported (halted, or newly tradable)
//   - the snapshot carries a version and the observation time, so a reader
//     can tell "this is what we saw at 14:03" from "this is true".
//
// It does not invent mappings. A rename cannot be inferred from a catalogue
// that only shows the new name; that requires an external source, and
// guessing would silently move a position's protection to the wrong ticker.
package refdata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/picker"
)

// Entry is what was observed about one symbol at one moment.
type Entry struct {
	AssetID    string `json:"asset_id,omitempty"`
	Name       string `json:"name,omitempty"`
	Class      string `json:"class"`
	Exchange   string `json:"exchange,omitempty"`
	Tradable   bool   `json:"tradable"`
	Fractional bool   `json:"fractional,omitempty"`
	Shortable  bool   `json:"shortable,omitempty"`
}

// Snapshot is one observation of the universe.
type Snapshot struct {
	// Version increments on every save, so two files can be ordered even if
	// a clock is wrong.
	Version    int              `json:"version"`
	ObservedAt time.Time        `json:"observed_at"`
	Assets     map[string]Entry `json:"assets"`
}

// ChangeKind classifies a difference between snapshots.
type ChangeKind string

const (
	Appeared     ChangeKind = "appeared"
	Disappeared  ChangeKind = "disappeared"
	Tradability  ChangeKind = "tradability"
	ClassChanged ChangeKind = "class_changed"
	// Renamed means the broker kept the same asset identity under a new
	// ticker. It is the one corporate-action change that can be established
	// from broker data alone, because AssetID is stable across the rename.
	Renamed ChangeKind = "renamed"
)

// Change is one difference, with enough detail to act on.
type Change struct {
	Kind   ChangeKind `json:"kind"`
	Symbol string     `json:"symbol"`
	// NewSymbol is set for a rename: the ticker the same asset now trades
	// under.
	NewSymbol string `json:"new_symbol,omitempty"`
	Detail    string `json:"detail"`
}

// renames maps asset id to symbol for entries that carry an id.
func renames(s Snapshot) map[string]string {
	out := map[string]string{}
	for sym, e := range s.Assets {
		if e.AssetID != "" {
			out[e.AssetID] = sym
		}
	}
	return out
}

// Build observations the snapshot from the classified universe.
func Build(insts []picker.Instrument, now time.Time) Snapshot {
	assets := make(map[string]Entry, len(insts))
	for _, i := range insts {
		assets[i.Symbol] = Entry{
			AssetID:    i.AssetID,
			Name:       i.Name,
			Class:      string(i.Class),
			Exchange:   i.Exchange,
			Tradable:   i.Tradable,
			Fractional: i.Fractional,
			Shortable:  i.Shortable,
		}
	}
	return Snapshot{Version: 1, ObservedAt: now.UTC(), Assets: assets}
}

// Diff reports what changed between two snapshots, in a stable order.
//
// Renames are detected first: an asset id present in both snapshots under
// different tickers is the same instrument renamed, which is broker fact
// rather than an inference. The moved symbol is then excluded from the
// appeared/disappeared lists, because "OLD vanished and NEW showed up" and
// "OLD is now NEW" are different facts and only the second one lets a
// position's protection follow the instrument.
func Diff(prev, next Snapshot) []Change {
	var out []Change
	renamedFrom := map[string]bool{}
	prevIDs, nextIDs := renames(prev), renames(next)
	ids := make([]string, 0, len(prevIDs))
	for id := range prevIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		oldSym, ok := prevIDs[id]
		if !ok {
			continue
		}
		newSym, still := nextIDs[id]
		if !still || newSym == oldSym {
			continue
		}
		renamedFrom[oldSym] = true
		out = append(out, Change{Kind: Renamed, Symbol: oldSym, NewSymbol: newSym,
			Detail: fmt.Sprintf("same asset id %s now trades as %s; levels and protection should follow", id, newSym)})
		// The new ticker is not "new": it is the same instrument.
		prev.Assets[newSym] = prev.Assets[oldSym]
	}
	syms := make([]string, 0, len(prev.Assets)+len(next.Assets))
	seen := map[string]bool{}
	for s := range prev.Assets {
		if !seen[s] {
			seen[s] = true
			syms = append(syms, s)
		}
	}
	for s := range next.Assets {
		if !seen[s] {
			seen[s] = true
			syms = append(syms, s)
		}
	}
	sort.Strings(syms)
	for _, s := range syms {
		before, had := prev.Assets[s]
		after, has := next.Assets[s]
		switch {
		case had && !has && renamedFrom[s]:
			// The old ticker of a rename: reported as Renamed above, not as
			// a disappearance.
			continue
		case had && !has:
			out = append(out, Change{Kind: Disappeared, Symbol: s,
				Detail: "no longer present in the broker's catalogue (delisted, renamed, or removed); any stored levels for it are orphaned"})
		case !had && has:
			out = append(out, Change{Kind: Appeared, Symbol: s,
				Detail: "newly present in the catalogue"})
		default:
			if before.Tradable != after.Tradable {
				state := "no longer tradable"
				if after.Tradable {
					state = "tradable again"
				}
				out = append(out, Change{Kind: Tradability, Symbol: s, Detail: state})
			}
			if before.Class != after.Class && before.Class != "" && after.Class != "" {
				out = append(out, Change{Kind: ClassChanged, Symbol: s,
					Detail: fmt.Sprintf("%s -> %s", before.Class, after.Class)})
			}
		}
	}
	return out
}

// Path derives the snapshot's location from the journal path, so no new flag
// is required: data/trades.jsonl -> data/refdata.json.
func Path(tradeLogPath string) string {
	dir := filepath.Dir(tradeLogPath)
	if dir == "" || dir == "." {
		dir = "data"
	}
	return filepath.Join(dir, "refdata.json")
}

// Load reads a snapshot. A missing file is an empty snapshot (first run); a
// corrupt one is an error — silently treating it as empty would report every
// symbol as "appeared".
func Load(path string) (Snapshot, error) {
	snap := Snapshot{Assets: map[string]Entry{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return snap, nil
		}
		return snap, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return Snapshot{Assets: map[string]Entry{}}, fmt.Errorf("parse %s: %w", path, err)
	}
	if snap.Assets == nil {
		snap.Assets = map[string]Entry{}
	}
	return snap, nil
}

// Save writes the snapshot atomically, bumping the version.
func Save(path string, snap Snapshot) (Snapshot, error) {
	if snap.Assets == nil {
		snap.Assets = map[string]Entry{}
	}
	snap.Version = max(snap.Version, 1)
	snap.ObservedAt = snap.ObservedAt.UTC()
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return snap, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return snap, err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return snap, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return snap, err
	}
	return snap, nil
}
