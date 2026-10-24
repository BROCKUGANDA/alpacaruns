// state_ownership_test.go — pins the cross-process file-ownership rules
// for the two files under data/:
//
//   - strategy-state.json is written ONLY by the bot (strategy.StateStore).
//   - config-overrides.json is written ONLY by the dashboard API.
//   - starting_equity is a real typed field, so no bot save can drop it.
//   - a document this build does not fully understand (unknown/future
//     version, unknown top-level keys) round-trips verbatim instead of
//     being silently downgraded and rewritten.
//
// The tests here are written so they fail on the pre-ownership code: the
// assertions that check the bot's document bytes are unchanged, that the
// scratch names are process-scoped, and that starting_equity survives a
// Heartbeat are exactly the writes the old shared "<path>.tmp" read-
// modify-write performed.
package strategy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ownershipReadRaw reads a file as a string; fails the test if it cannot.
func ownershipReadRaw(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ownershipDecodeDoc decodes a JSON object document generically.
func ownershipDecodeDoc(t *testing.T, body string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("document is not valid JSON: %v\n%s", err, body)
	}
	if doc == nil {
		t.Fatalf("document decoded to nil: %s", body)
	}
	return doc
}

// ownershipScratchFiles lists every *.tmp in dir, sorted-ish and stable
// enough for a set comparison.
func ownershipScratchFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}

// newOwnershipStore returns a state store in a fresh temp dir plus the
// path its config-overrides sibling would take.
func newOwnershipStore(t *testing.T) (*StateStore, string) {
	t.Helper()
	dir := t.TempDir()
	return NewStateStore(filepath.Join(dir, "trades.jsonl")), dir
}

// TestStartingEquitySurvivesTypedSaveAndHeartbeat seeds the exact shape of
// the live production file — where starting_equity was patched in by the
// old raw-map writer in cmd/alpacaruns auto — and proves the per-tick
// Heartbeat keeps it. Before StartingEquity was a typed field, every
// typed Save marshalled the struct, had no such key, and dropped it, so
// the adaptive-sizing baseline was destroyed on every tick.
func TestStartingEquitySurvivesTypedSaveAndHeartbeat(t *testing.T) {
	st, _ := newOwnershipStore(t)
	seed := `{
	  "version": 1,
	  "levels": {"BTC/USD": {"symbol": "BTC/USD", "entry_price": 50000,
	    "take_profit": 54000, "stop_loss": 48000, "qty": "0.5", "crypto": true}},
	  "peak_equity": 199915.97,
	  "starting_equity": 100000,
	  "tick_number": 3,
	  "last_tick": "2026-10-24T18:00:00Z"
	}`
	if err := os.WriteFile(st.Path(), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.StartingEquity != 100000 {
		t.Fatalf("typed load must read the live key: StartingEquity=%v", loaded.StartingEquity)
	}
	if loaded.PeakEquity != 199915.97 {
		t.Fatalf("peak_equity = %v, want 199915.97", loaded.PeakEquity)
	}

	// The heartbeat is a plain load → mutate counters → save. It must not
	// touch anything else in the document.
	if err := st.Heartbeat(4); err != nil {
		t.Fatal(err)
	}
	doc := ownershipDecodeDoc(t, ownershipReadRaw(t, st.Path()))
	if got := doc["starting_equity"]; got != float64(100000) {
		t.Fatalf("Heartbeat dropped starting_equity: %v (document: %s)", got, ownershipReadRaw(t, st.Path()))
	}
	if got := doc["peak_equity"]; got != 199915.97 {
		t.Fatalf("Heartbeat dropped peak_equity: %v", got)
	}
	levels, ok := doc["levels"].(map[string]any)
	if !ok || levels["BTC/USD"] == nil {
		t.Fatalf("Heartbeat dropped the crypto level: %v", doc["levels"])
	}
	if again, err := st.Load(); err != nil || again.StartingEquity != 100000 {
		t.Fatalf("round-trip failed: %v / %+v", err, again)
	}
}

// TestSetStartingEquityIsWrittenThroughTheTypedStore covers the two typed
// setters the bot uses: the one-shot baseline (idempotent, race-free) and
// the explicit write.
func TestSetStartingEquityIsWrittenThroughTheTypedStore(t *testing.T) {
	st, _ := newOwnershipStore(t)

	if got, err := st.ReadStartingEquity(); err != nil || got != 0 {
		t.Fatalf("missing file must read as 0: got=%v err=%v", got, err)
	}

	wrote, err := st.SetStartingEquityIfUnset(100_000)
	if err != nil || !wrote {
		t.Fatalf("first baseline must be written: wrote=%v err=%v", wrote, err)
	}
	// A second tick that also thinks it is first must NOT rebase the P&L.
	wrote, err = st.SetStartingEquityIfUnset(250_000)
	if err != nil || wrote {
		t.Fatalf("baseline is one-shot: wrote=%v err=%v", wrote, err)
	}
	if got, _ := st.ReadStartingEquity(); got != 100_000 {
		t.Fatalf("baseline changed under a live position: %v", got)
	}

	// A deliberate overwrite still works, and survives a level write.
	if err := st.SetStartingEquity(120_000); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLevel(PositionLevels{Symbol: "BTC/USD", EntryPrice: 50000,
		TakeProfit: 54000, StopLoss: 48000, Qty: "0.5"}); err != nil {
		t.Fatal(err)
	}
	doc := ownershipDecodeDoc(t, ownershipReadRaw(t, st.Path()))
	if doc["starting_equity"] != float64(120_000) {
		t.Fatalf("SetLevel dropped starting_equity: %v", doc)
	}
	if got, _ := st.ReadStartingEquity(); got != 120_000 {
		t.Fatalf("ReadStartingEquity = %v, want 120000", got)
	}
}

// TestFutureVersionKeepsUnknownFieldsVerbatim pins the version decision:
// an unknown (future) version is LOADED, not rejected and not downgraded.
// Every key this build cannot decode is re-attached verbatim on save, and
// the version stamp is kept, so a newer dashboard/bot document survives an
// older binary's rewrite instead of being truncated to what the old
// struct knows.
func TestFutureVersionKeepsUnknownFieldsVerbatim(t *testing.T) {
	st, _ := newOwnershipStore(t)
	seed := `{
	  "version": 7,
	  "levels": {"BTC/USD": {"symbol": "BTC/USD", "entry_price": 50000,
	    "take_profit": 54000, "stop_loss": 48000, "qty": "0.5", "crypto": true}},
	  "peak_equity": 199915.97,
	  "week_start": "2026-10-19T00:00:00Z",
	  "tick_number": 12,
	  "last_tick": "2026-10-24T18:00:00Z",
	  "starting_equity": 100000,
	  "sentinel_block": {"daily": 2, "streaks": [1, 2, 3]},
	  "brand_new_scalar": "keep me"
	}`
	if err := os.WriteFile(st.Path(), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := st.Load()
	if err != nil {
		t.Fatalf("an unknown version must still load (refusing would stop TP/SL enforcement): %v", err)
	}
	if loaded.Version != 7 {
		t.Fatalf("load must not downgrade the version stamp in memory: %v", loaded.Version)
	}
	if loaded.StartingEquity != 100000 || loaded.PeakEquity != 199915.97 {
		t.Fatalf("known fields must still decode: %+v", loaded)
	}

	// Any bot write — here a new intraday level — must preserve the rest.
	if err := st.SetLevel(PositionLevels{Symbol: "ETH/USD", EntryPrice: 3000,
		TakeProfit: 3300, StopLoss: 2800, Qty: "2", Track: "intraday"}); err != nil {
		t.Fatal(err)
	}
	raw := ownershipReadRaw(t, st.Path())
	doc := ownershipDecodeDoc(t, raw)

	if doc["version"] != float64(7) {
		t.Fatalf("save downgraded a future version to %v; the writer that set it gets no warning", doc["version"])
	}
	if !reflect.DeepEqual(doc["sentinel_block"], map[string]any{
		"daily": float64(2), "streaks": []any{float64(1), float64(2), float64(3)},
	}) {
		t.Fatalf("unknown object not preserved verbatim: %#v", doc["sentinel_block"])
	}
	if doc["brand_new_scalar"] != "keep me" {
		t.Fatalf("unknown scalar not preserved: %#v", doc["brand_new_scalar"])
	}
	if doc["starting_equity"] != float64(100000) || doc["peak_equity"] != 199915.97 {
		t.Fatalf("known fields lost on the preserve path: %s", raw)
	}
	if doc["week_start"] != "2026-10-19T00:00:00Z" {
		t.Fatalf("week_start not preserved: %#v", doc["week_start"])
	}
	levels, _ := doc["levels"].(map[string]any)
	if levels == nil || levels["BTC/USD"] == nil || levels["ETH/USD|intraday"] == nil {
		t.Fatalf("levels must survive the preserve path: %#v", doc["levels"])
	}

	// A state built in-process (never loaded from a newer file) still
	// writes the current version, so the fresh-boot shape is unchanged.
	fresh := &State{Levels: map[string]*PositionLevels{}}
	if err := st.Save(fresh); err != nil {
		t.Fatal(err)
	}
	if v := ownershipDecodeDoc(t, ownershipReadRaw(t, st.Path()))["version"]; v != float64(stateVersion) {
		t.Fatalf("a hand-built State must stamp version %d, got %v", stateVersion, v)
	}
}

// TestStateSaveScratchIsProcessScoped proves the bot no longer publishes
// through the shared "<state>.tmp" name the dashboard used to rename from
// — one scratch path, two processes, is how a stale snapshot landed on
// top of a fresh one.
func TestStateSaveScratchIsProcessScoped(t *testing.T) {
	st, dir := newOwnershipStore(t)

	legacy := st.Path() + ".tmp"
	if err := os.WriteFile(legacy, []byte("STALE SNAPSHOT FROM THE OLD DASHBOARD WRITER"), 0o644); err != nil {
		t.Fatal(err)
	}

	tmp := st.tmpPath()
	if tmp == legacy {
		t.Fatalf("the bot still uses the shared scratch name %q", tmp)
	}
	if want := fmt.Sprintf(".state-%d.tmp", os.Getpid()); !strings.Contains(tmp, want) {
		t.Fatalf("scratch name %q must be process-scoped (contains %q)", tmp, want)
	}

	if err := st.SetLevel(PositionLevels{Symbol: "BTC/USD", EntryPrice: 50000,
		TakeProfit: 54000, StopLoss: 48000, Qty: "0.5"}); err != nil {
		t.Fatal(err)
	}

	// The bot's document is valid and carries the level.
	got, err := st.Load()
	if err != nil {
		t.Fatalf("state file unreadable after save: %v", err)
	}
	if got.Levels[levelKey("BTC/USD", "")] == nil {
		t.Fatalf("level missing after save: %+v", got.Levels)
	}
	// The planted legacy scratch file was neither consumed nor rewritten.
	if b := ownershipReadRaw(t, legacy); !strings.Contains(b, "STALE") {
		t.Fatalf("bot save touched the legacy shared scratch path: %q", b)
	}
	for _, name := range ownershipScratchFiles(t, dir) {
		if name != filepath.Base(legacy) {
			t.Fatalf("save left scratch behind: %s", name)
		}
	}
}

// TestConfigOverridesPathDerivesFromDataDir: the override path must fall
// out of the existing data paths so no new flag is required anywhere.
func TestConfigOverridesPathDerivesFromDataDir(t *testing.T) {
	cases := []struct{ in, wantDir string }{
		{"data/strategy-state.json", "data"},
		{"data/trades.jsonl", "data"},
		{"/opt/alpacaruns/data/strategy-state.json", "/opt/alpacaruns/data"},
		{"strategy-state.json", "data"}, // no directory component
		{"", "data"},                    // unset entirely
	}
	for _, c := range cases {
		got := ConfigOverridesPath(c.in)
		want := filepath.Join(c.wantDir, ConfigOverridesFileName)
		if filepath.Clean(got) != filepath.Clean(want) {
			t.Fatalf("ConfigOverridesPath(%q) = %q, want %q", c.in, got, want)
		}
		if strings.HasSuffix(filepath.Base(got), "strategy-state.json") {
			t.Fatalf("override path must not resolve to the bot's document: %q", got)
		}
	}
}

// TestConfigOverridesRoundTripIsSelfContained proves the dashboard's file
// holds exactly its own knobs and that writing it cannot disturb the bot's
// document (byte for byte).
func TestConfigOverridesRoundTripIsSelfContained(t *testing.T) {
	st, dir := newOwnershipStore(t)
	if err := st.SetLevel(PositionLevels{Symbol: "BTC/USD", EntryPrice: 50000,
		TakeProfit: 54000, StopLoss: 48000, Qty: "0.5"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStartingEquity(100_000); err != nil {
		t.Fatal(err)
	}
	stateBefore := ownershipReadRaw(t, st.Path())

	path := ConfigOverridesPath(st.Path())
	if path == st.Path() {
		t.Fatal("override path must differ from the state path")
	}
	ov := ConfigOverrides{
		MaxPositionUSD:       9000,
		MaxPortfolioPct:      0.25,
		CryptoMaxPositionUSD: 4000,
		MinConfidence:        0.55,
		DailyDDHalt:          0.04,
		WeeklyDDHalt:         0.08,
		TotalDDHalt:          0.12,
	}
	if err := SaveConfigOverrides(path, ov); err != nil {
		t.Fatalf("save overrides: %v", err)
	}

	got, err := LoadConfigOverrides(path)
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("SaveConfigOverrides must stamp updated_at")
	}
	// updated_at is stamped by the writer, so compare against the value
	// that was actually written; every knob must match exactly.
	want := ov
	want.UpdatedAt = got.UpdatedAt
	if *got != want {
		t.Fatalf("overrides did not round-trip:\n got %+v\nwant %+v", *got, want)
	}

	doc := ownershipDecodeDoc(t, ownershipReadRaw(t, path))
	ownedKeys := map[string]bool{
		"max_position_usd": true, "max_portfolio_pct": true, "crypto_max_position_usd": true,
		"min_confidence": true, "daily_dd_halt": true, "weekly_dd_halt": true,
		"total_dd_halt": true, "updated_at": true,
	}
	for k := range doc {
		if !ownedKeys[k] {
			t.Fatalf("override document carries a key it does not own: %q (%s)", k, ownershipReadRaw(t, path))
		}
	}
	for k := range ownedKeys {
		if _, ok := doc[k]; !ok {
			t.Fatalf("override document must always carry %q (absent and zero are the same read)", k)
		}
	}
	if _, leaked := doc["levels"]; leaked {
		t.Fatal("override document must not carry the bot's levels")
	}

	// The bot's document is untouched by an override write.
	if after := ownershipReadRaw(t, st.Path()); after != stateBefore {
		t.Fatalf("override write changed the bot's document:\nbefore: %s\nafter:  %s", stateBefore, after)
	}
	// ...and it left no scratch behind.
	if scratch := ownershipScratchFiles(t, dir); len(scratch) != 0 {
		t.Fatalf("atomic write left scratch files: %v", scratch)
	}
}

// TestProductionShapeFileRoundTrips uses the document that is actually on
// the server today (data/strategy-state.json), including the seven knob
// keys the old dashboard merged into it, and proves the bot can still load
// it, keep the drawdown peak, and lose nothing on the next write.
func TestProductionShapeFileRoundTrips(t *testing.T) {
	st, _ := newOwnershipStore(t)
	production := `{
	  "crypto_max_position_usd": 10000,
	  "daily_dd_halt": 0.05,
	  "levels": {},
	  "max_portfolio_pct": 0.2,
	  "max_position_usd": 8000,
	  "min_confidence": 0.55,
	  "peak_equity": 199915.97,
	  "total_dd_halt": 0.15,
	  "version": 1,
	  "week_start": "0001-01-01T00:00:00Z",
	  "weekly_dd_halt": 0.1
	}`
	if err := os.WriteFile(st.Path(), []byte(production), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatalf("the live file must load: %v", err)
	}
	if got.PeakEquity != 199915.97 {
		t.Fatalf("peak_equity misread: %v", got.PeakEquity)
	}
	if len(got.Levels) != 0 {
		t.Fatalf("empty levels map must load empty, got %d", len(got.Levels))
	}

	// The bot's next tick writes a level and a heartbeat; the keys that
	// belong to neither must not vanish with it.
	if err := st.SetLevel(PositionLevels{Symbol: "BTC/USD", EntryPrice: 50000,
		TakeProfit: 54000, StopLoss: 48000, Qty: "0.5"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Heartbeat(1); err != nil {
		t.Fatal(err)
	}
	doc := ownershipDecodeDoc(t, ownershipReadRaw(t, st.Path()))
	for _, knob := range []string{"max_position_usd", "daily_dd_halt", "weekly_dd_halt",
		"total_dd_halt", "min_confidence", "crypto_max_position_usd", "max_portfolio_pct"} {
		if _, ok := doc[knob]; !ok {
			t.Fatalf("bot write discarded the pre-existing key %q: %s", knob, ownershipReadRaw(t, st.Path()))
		}
	}
	if doc["peak_equity"] != 199915.97 {
		t.Fatalf("peak_equity lost: %v", doc["peak_equity"])
	}
	if levels, ok := doc["levels"].(map[string]any); !ok || levels["BTC/USD"] == nil {
		t.Fatalf("level missing: %v", doc["levels"])
	}
	if doc["version"] != float64(stateVersion) || doc["tick_number"] != float64(1) {
		t.Fatalf("version/tick stamp wrong: %v / %v", doc["version"], doc["tick_number"])
	}
}

// TestConfigOverridesMissingAndCorrupt: a missing file is a normal cold
// start; a corrupt file is a loud error rather than a silent zeroing of
// the operator's risk caps.
func TestConfigOverridesMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, ConfigOverridesFileName)
	got, err := LoadConfigOverrides(missing)
	if err != nil || got == nil || *got != (ConfigOverrides{}) {
		t.Fatalf("missing file must yield the zero document: %+v err=%v", got, err)
	}
	if got, err := LoadConfigOverrides(""); err != nil || *got != (ConfigOverrides{}) {
		t.Fatalf("empty path must yield the zero document: %+v err=%v", got, err)
	}

	bad := filepath.Join(dir, "bad-overrides.json")
	if err := os.WriteFile(bad, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigOverrides(bad); err == nil {
		t.Fatal("a corrupt override file must error, not be read as zeroed risk caps")
	}

	// Save into a directory that does not exist yet (fresh box).
	nested := filepath.Join(dir, "sub", ConfigOverridesFileName)
	if err := SaveConfigOverrides(nested, ConfigOverrides{MaxPositionUSD: 1}); err != nil {
		t.Fatalf("save must create the data dir: %v", err)
	}
	if v, err := LoadConfigOverrides(nested); err != nil || v.MaxPositionUSD != 1 {
		t.Fatalf("round-trip through a created dir: %+v err=%v", v, err)
	}
}
