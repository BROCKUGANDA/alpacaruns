// state_ownership_test.go — the dashboard must never write the bot's
// data/strategy-state.json. persistConfig used to read that document into
// a map, merge seven knob keys into it, and rename it back through the
// same "<path>.tmp" scratch the bot used. Two processes, one file, no
// flock: whichever rename landed second won, and the loser's data (a
// crypto TP/SL level, the drawdown peak, the P&L baseline) was gone until
// the next write rebuilt it — and the bot's CryptoLevelsForTrack pass has
// nothing to enforce once its level key is missing.
//
// These tests reproduce that interleaving deterministically: snapshot the
// bot's document (the read the old code held across the bot's write), let
// the bot write a new level and a new peak, then run the dashboard's
// config save and assert the bot's document came out byte-identical.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/strategy"
)

// ownershipRaw reads a file as a string, failing the test on error.
func ownershipRaw(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ownershipDoc decodes a JSON object document from a file.
func ownershipDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(ownershipRaw(t, path)), &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if doc == nil {
		t.Fatalf("%s decoded to null", path)
	}
	return doc
}

// ownershipScratch lists every *.tmp left in dir.
func ownershipScratch(t *testing.T, dir string) []string {
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

// ownershipPostConfig drives POST /api/config through the real router and
// returns the decoded ConfigResponse.
func ownershipPostConfig(t *testing.T, ts *httptest.Server, body string) ConfigResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/config", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/config status %d, want 200 (body %s)", resp.StatusCode, body)
	}
	var cr ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	return cr
}

// seedBotState writes the bot's document the way the bot writes it: a
// swing crypto level, the drawdown high-water mark and the P&L baseline.
func seedBotState(t *testing.T, dir string) *strategy.StateStore {
	t.Helper()
	st := strategy.NewStateStore(filepath.Join(dir, "trades.jsonl"))
	if err := st.SetLevel(strategy.PositionLevels{Symbol: "BTC/USD", EntryPrice: 50000,
		TakeProfit: 54000, StopLoss: 48000, Qty: "0.5"}); err != nil {
		t.Fatal(err)
	}
	cur, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	cur.PeakEquity = 199915.97
	if err := st.Save(cur); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStartingEquity(100000); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestConfigSaveDoesNotRewriteBotStateDocument is the interleaving test.
// T0 = the dashboard's read of the bot's document (the snapshot the old
// persistConfig held); T1 = the bot writes a fresh level and moves the
// peak; T2 = the dashboard saves config. The bot's writes must still be
// there at T2, and the file must not have been rewritten at all.
func TestConfigSaveDoesNotRewriteBotStateDocument(t *testing.T) {
	dir := t.TempDir()
	bot := seedBotState(t, dir)
	statePath := bot.Path()

	// T0: the document the dashboard would have captured by reading it.
	_ = ownershipRaw(t, statePath)

	// T1: the bot opens a new crypto position and marks a new high-water
	// peak between that read and the dashboard's write.
	if err := bot.SetLevel(strategy.PositionLevels{Symbol: "ETH/USD", EntryPrice: 3000,
		TakeProfit: 3300, StopLoss: 2800, Qty: "2"}); err != nil {
		t.Fatal(err)
	}
	cur, err := bot.Load()
	if err != nil {
		t.Fatal(err)
	}
	cur.PeakEquity = 201500
	if err := bot.Save(cur); err != nil {
		t.Fatal(err)
	}
	afterBotWrite := ownershipRaw(t, statePath)

	// T2: the operator clicks Save on the dashboard.
	s := newTestServerWithConfig(t, dir, 9000, 0.25, 0.04, 0.08, 0.12)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	ownershipPostConfig(t, ts, `{"max_position_usd": 9000, "max_portfolio_pct": 0.25}`)

	// The bot's document is exactly what the bot wrote. The old code
	// renamed its T0 snapshot over it here, so this assertion is the one
	// that catches the pre-ownership behaviour — and it is the assertion
	// that protects the ETH/USD level and the new peak below.
	if got := ownershipRaw(t, statePath); got != afterBotWrite {
		t.Fatalf("the dashboard rewrote the bot's document over its own write:\nbot:  %s\nnow:  %s", afterBotWrite, got)
	}

	got, err := bot.Load()
	if err != nil {
		t.Fatalf("state unreadable after the dashboard save: %v", err)
	}
	if got.Levels["BTC/USD"] == nil || got.Levels["ETH/USD"] == nil {
		t.Fatalf("a TP/SL level was lost to the config save: %+v", got.Levels)
	}
	if got.PeakEquity != 201500 {
		t.Fatalf("peak_equity rolled back to %v, want 201500", got.PeakEquity)
	}
	if got.StartingEquity != 100000 {
		t.Fatalf("starting_equity lost: %v", got.StartingEquity)
	}
	// And the knob values no longer live in the bot's document.
	if strings.Contains(ownershipRaw(t, statePath), "max_position_usd") {
		t.Fatal("config keys are still being merged into the bot's state file")
	}
}

// TestConfigOverridesFileHasOnlyDashboardKeys checks the dashboard's own
// file lands next to the state file, carries exactly its seven knobs plus
// updated_at, and holds none of the bot's keys.
func TestConfigOverridesFileHasOnlyDashboardKeys(t *testing.T) {
	dir := t.TempDir()
	bot := seedBotState(t, dir)
	s := newTestServerWithConfig(t, dir, 7500, 0.15, 0.04, 0.08, 0.12)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	cr := ownershipPostConfig(t, ts, `{"max_position_usd": 9000, "crypto_max_position_usd": 4000, "min_confidence": 0.6}`)
	if cr.MaxPositionUSD != 9000 || cr.CryptoMaxPositionUSD != 4000 || cr.MinConfidence != 0.6 {
		t.Fatalf("POST response must echo the saved knobs: %+v", cr)
	}

	path := filepath.Join(dir, strategy.ConfigOverridesFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("override file not created at %s: %v", path, err)
	}
	doc := ownershipDoc(t, path)
	wantKeys := map[string]bool{
		"max_position_usd": true, "max_portfolio_pct": true, "crypto_max_position_usd": true,
		"min_confidence": true, "daily_dd_halt": true, "weekly_dd_halt": true,
		"total_dd_halt": true, "updated_at": true,
	}
	for k := range doc {
		if !wantKeys[k] {
			t.Fatalf("override file carries an unexpected key %q: %s", k, ownershipRaw(t, path))
		}
	}
	for k := range wantKeys {
		if _, ok := doc[k]; !ok {
			t.Fatalf("override file must always carry %q so the bot can read it blindly", k)
		}
	}
	for _, botKey := range []string{"levels", "peak_equity", "starting_equity", "tick_number", "version"} {
		if _, leaked := doc[botKey]; leaked {
			t.Fatalf("bot key %q leaked into the override document", botKey)
		}
	}
	if doc["max_position_usd"] != float64(9000) || doc["daily_dd_halt"] != 0.04 {
		t.Fatalf("saved values not persisted: %v", doc)
	}
	// The bot reads this file with the exported loader — same struct, so
	// the shape is guaranteed to match what the API just wrote.
	ov, err := strategy.LoadConfigOverrides(path)
	if err != nil {
		t.Fatal(err)
	}
	if ov.MaxPositionUSD != 9000 || ov.DailyDDHalt != 0.04 || ov.TotalDDHalt != 0.12 || ov.UpdatedAt.IsZero() {
		t.Fatalf("reader sees a different document: %+v", ov)
	}
	// The bot's document is still the bot's alone.
	if strings.Contains(ownershipRaw(t, bot.Path()), "max_position_usd") {
		t.Fatal("the bot's state file still carries dashboard knobs")
	}
}

// TestConfigOverridesWriteIsAtomicAndSharesNoScratch proves the dashboard
// no longer renames through the bot's scratch path: a stale "<state>.tmp"
// is left untouched, the override file lands whole, and no scratch is
// left behind.
func TestConfigOverridesWriteIsAtomicAndSharesNoScratch(t *testing.T) {
	dir := t.TempDir()
	bot := seedBotState(t, dir)
	legacyScratch := bot.Path() + ".tmp"
	if err := os.WriteFile(legacyScratch, []byte("STALE BOT DOCUMENT FROM THE OLD SHARED TMP"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServerWithConfig(t, dir, 7500, 0.15, 0.04, 0.08, 0.12)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	ownershipPostConfig(t, ts, `{"max_position_usd": 12345}`)

	if b := ownershipRaw(t, legacyScratch); !strings.Contains(b, "STALE") {
		t.Fatalf("the config save consumed the shared scratch path: %q", b)
	}
	for _, name := range ownershipScratch(t, dir) {
		if name != filepath.Base(legacyScratch) {
			t.Fatalf("config save left scratch behind: %s", name)
		}
	}
	// The document is complete, not a partial write.
	ov, err := strategy.LoadConfigOverrides(filepath.Join(dir, strategy.ConfigOverridesFileName))
	if err != nil || ov.MaxPositionUSD != 12345 {
		t.Fatalf("override document unreadable or partial: %+v err=%v", ov, err)
	}
}

// TestStartingEquitySurvivesDashboardConfigSave covers the baseline the
// adaptive sizer and the dashboard's P&L curve both read.
func TestStartingEquitySurvivesDashboardConfigSave(t *testing.T) {
	dir := t.TempDir()
	bot := seedBotState(t, dir)

	s := newTestServerWithConfig(t, dir, 7500, 0.15, 0.04, 0.08, 0.12)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	ownershipPostConfig(t, ts, `{"max_position_usd": 8000}`)

	// A bot tick after the dashboard write must not drop it either.
	if err := bot.Heartbeat(42); err != nil {
		t.Fatal(err)
	}
	doc := ownershipDoc(t, bot.Path())
	if doc["starting_equity"] != float64(100000) {
		t.Fatalf("starting_equity dropped from the bot document: %v", doc)
	}
	if doc["peak_equity"] != 199915.97 {
		t.Fatalf("peak_equity dropped: %v", doc)
	}
	got, err := bot.Load()
	if err != nil || got.StartingEquity != 100000 || got.TickNumber != 42 {
		t.Fatalf("typed round-trip failed: %+v err=%v", got, err)
	}
}

// TestConfigOverridesPathDerivesWithoutANewFlag: a deployment that passes
// no override flag (only --env / TRADE_LOG) still lands the file in the
// bot's data dir, derived from StateFile.
func TestConfigOverridesPathDerivesWithoutANewFlag(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithConfig(t, dir, 7500, 0.15, 0.04, 0.08, 0.12)
	if s.settings.ConfigOverridesFile != "" {
		t.Fatalf("fixture should exercise the fallback, got %q", s.settings.ConfigOverridesFile)
	}
	if got := s.configOverridesPath(); got != filepath.Join(dir, strategy.ConfigOverridesFileName) {
		t.Fatalf("derived override path = %q", got)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	ownershipPostConfig(t, ts, `{"max_position_usd": 5000}`)
	if _, err := os.Stat(filepath.Join(dir, strategy.ConfigOverridesFileName)); err != nil {
		t.Fatalf("fallback path not used: %v", err)
	}
}

// TestConfigSaveWithoutAnyDataPathWritesNothing: with neither a state file
// nor an override path configured, the POST still succeeds in-memory and
// creates no files.
func TestConfigSaveWithoutAnyDataPathWritesNothing(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithConfig(t, dir, 7500, 0.15, 0.04, 0.08, 0.12)
	s.settings.StateFile = ""
	s.settings.ConfigOverridesFile = ""
	if p := s.configOverridesPath(); p != "" {
		t.Fatalf("expected no override path, got %q", p)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	cr := ownershipPostConfig(t, ts, `{"max_position_usd": 4321}`)
	if cr.MaxPositionUSD != 4321 {
		t.Fatalf("in-memory update missing: %+v", cr)
	}
	if _, err := os.Stat(filepath.Join(dir, strategy.ConfigOverridesFileName)); !os.IsNotExist(err) {
		t.Fatalf("no override path configured, yet the file exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "strategy-state.json")); !os.IsNotExist(err) {
		t.Fatalf("no state path configured, yet the bot document exists: %v", err)
	}
	for _, name := range ownershipScratch(t, dir) {
		t.Fatalf("config save left scratch behind: %s", name)
	}
}
