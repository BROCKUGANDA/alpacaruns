// Package universe screens the configured symbols before the bot scores them.
//
// A market-cap screen needs a real source. This one uses SEC EDGAR's
// reported shares outstanding times the broker's latest close: both numbers
// are authoritative, neither is estimated, and the result is stamped with
// when it was observed and where it came from. A screen built on invented
// caps would look like a filter and behave like a random number generator.
//
// Two classes are exempt BY DESIGN, and the exemption is stated rather than
// implied:
//
//   - ETFs (including index trackers): "market cap" is not a property of a
//     fund. Screening SPY by market cap would be a category error; funds are
//     screened by tradability and, at entry, by spread.
//   - Crypto pairs: there is no share count. Screening them by a dollar
//     figure derived from equities would be meaningless.
//
// The screen fails CLOSED for a single-name equity whose cap is unknown,
// stale, or below the floor: an unverified name is not a large cap.
package universe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/picker"
)

// Caps is the observed market-cap table.
type Caps struct {
	// AsOf is when the underlying observations were taken.
	AsOf time.Time `json:"as_of"`
	// Source says where the numbers came from, so a reader can judge them.
	Source string `json:"source"`
	// Caps maps symbol -> market capitalisation in USD.
	Caps map[string]float64 `json:"caps"`
}

// Get returns the cap for a symbol.
func (c Caps) Get(symbol string) (float64, bool) {
	if c.Caps == nil {
		return 0, false
	}
	v, ok := c.Caps[strings.ToUpper(strings.TrimSpace(symbol))]
	return v, ok
}

// Path derives the cap file from the journal path.
func Path(tradeLogPath string) string {
	dir := filepath.Dir(tradeLogPath)
	if dir == "" || dir == "." {
		dir = "data"
	}
	return filepath.Join(dir, "market-caps.json")
}

// LoadCaps reads the table. A missing file is an empty table; a corrupt one
// is an error, because reading it as empty would silently stop screening.
func LoadCaps(path string) (Caps, error) {
	c := Caps{Caps: map[string]float64{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Caps{Caps: map[string]float64{}}, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Caps == nil {
		c.Caps = map[string]float64{}
	}
	return c, nil
}

// SaveCaps writes the table atomically.
func SaveCaps(path string, c Caps) error {
	if c.Caps == nil {
		c.Caps = map[string]float64{}
	}
	c.AsOf = c.AsOf.UTC()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Exclusion explains why a configured symbol was dropped.
type Exclusion struct {
	Symbol string `json:"symbol"`
	Class  string `json:"class"`
	Reason string `json:"reason"`
}

// Screen applies the cap filter to a classified universe.
//
// minCap <= 0 disables screening entirely (returns the input unchanged), so a
// deployment that has not configured a floor behaves exactly as before.
// maxCap <= 0 means "no upper bound".
func Screen(insts []picker.Instrument, caps Caps, minCap, maxCap float64, maxAge time.Duration, now time.Time) (kept []picker.Instrument, excluded []Exclusion) {
	if minCap <= 0 {
		return insts, nil
	}
	fresh := true
	ageReason := ""
	if maxAge > 0 {
		if caps.AsOf.IsZero() {
			fresh = false
			ageReason = "no market-cap data has ever been recorded"
		} else if now.Sub(caps.AsOf) > maxAge {
			fresh = false
			ageReason = fmt.Sprintf("market-cap data is %s old (limit %s)",
				now.Sub(caps.AsOf).Round(time.Hour), maxAge)
		}
	}
	for _, inst := range insts {
		switch inst.Class {
		case picker.ClassCrypto, picker.ClassOption:
			// No share count exists for these; a dollar screen would be
			// meaningless. They are filtered by tradability and spread.
			kept = append(kept, inst)
			continue
		case picker.ClassETF, picker.ClassIndex:
			// A fund's AUM is not a market cap. Exempt, and said so.
			kept = append(kept, inst)
			continue
		}
		if !fresh {
			excluded = append(excluded, Exclusion{Symbol: inst.Symbol, Class: string(inst.Class),
				Reason: "cannot verify market cap: " + ageReason})
			continue
		}
		cap, ok := caps.Get(inst.Symbol)
		switch {
		case !ok || cap <= 0:
			excluded = append(excluded, Exclusion{Symbol: inst.Symbol, Class: string(inst.Class),
				Reason: "no market cap on record (run `alpacaruns caps --refresh`)"})
		case cap < minCap:
			excluded = append(excluded, Exclusion{Symbol: inst.Symbol, Class: string(inst.Class),
				Reason: fmt.Sprintf("market cap $%.3gB is below the $%.3gB floor", cap/1e9, minCap/1e9)})
		case maxCap > 0 && cap > maxCap:
			excluded = append(excluded, Exclusion{Symbol: inst.Symbol, Class: string(inst.Class),
				Reason: fmt.Sprintf("market cap $%.3gB is above the $%.3gB ceiling", cap/1e9, maxCap/1e9)})
		default:
			kept = append(kept, inst)
		}
	}
	return kept, excluded
}

// SharesRecord is the shape EDGAR's companyconcept endpoint returns, reduced
// to what a cap needs. Kept as a struct so the parser can be tested against a
// real captured payload without a network.
type SharesRecord struct {
	End   string  `json:"end"`
	Val   float64 `json:"val"`
	Filed string  `json:"filed"`
	Form  string  `json:"form"`
}

// LatestShares picks the most recent reported share count.
//
// "Most recent" is by period end, and ties break on filing date: a 10-Q for
// the same period re-filed later supersedes the earlier one.
func LatestShares(records []SharesRecord) (float64, time.Time, error) {
	var best SharesRecord
	var bestEnd, bestFiled time.Time
	for _, r := range records {
		if r.Val <= 0 || strings.TrimSpace(r.End) == "" {
			continue
		}
		end, err := time.Parse("2006-01-02", r.End)
		if err != nil {
			continue
		}
		filed, _ := time.Parse("2006-01-02", r.Filed)
		if best.Val == 0 || end.After(bestEnd) || (end.Equal(bestEnd) && filed.After(bestFiled)) {
			best, bestEnd, bestFiled = r, end, filed
		}
	}
	if best.Val <= 0 {
		return 0, time.Time{}, fmt.Errorf("no usable share count in %d record(s)", len(records))
	}
	return best.Val, bestEnd, nil
}
