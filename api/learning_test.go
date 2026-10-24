package api

// learning_test.go proves the dashboard shows what the trading bot
// actually wrote, and that a missing or corrupt snapshot is reported
// rather than papered over with an empty table.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/learning"
	"github.com/BROCKUGANDA/alpacaruns/selfheal"
)

func TestLearningEndpointServesStoredAdjustments(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	s.cfg = &config.Config{LearnEnabled: true, LearnMinTrades: 12, LearnMinSizeMult: 0.25}

	store := learning.NewStore(filepath.Join(dir, "trades.jsonl"))
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.Save(learning.Adjustments{
		Active: true, SamplesSeen: 30, ComputedAt: now,
		ScoreFloorAdd: 0.05, SuggestedFloor: 0.55,
		Classes: map[string]learning.ClassAdjustment{
			"crypto": {Class: "crypto", Samples: 12, SizeMultiplier: 0.5, StopMultiplier: 0.8},
		},
		Quarantined: []learning.Quarantined{{Symbol: "TSLA", Samples: 9, WinRate: 0.11}},
		Buckets:     []learning.ScoreBucket{{From: 0.6, To: 0.8, Samples: 14, Expectancy: -0.02}},
		Notes:       []string{"score floor raised 0.050 to 0.550"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	var out LearningResponse
	resp, err := http.Get(ts.URL + "/api/learning")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Enabled {
		t.Fatal("LEARN_ENABLED not surfaced")
	}
	if !out.Adj.Active || out.Adj.SamplesSeen != 30 {
		t.Fatalf("stored snapshot not served: %+v", out.Adj)
	}
	if got := out.Adj.SizeMultiplierFor("CRYPTO"); got != 0.5 {
		t.Fatalf("class adjustment lost: %v", got)
	}
	if _, ok := out.Adj.IsQuarantined("tsla"); !ok {
		t.Fatal("quarantine list lost")
	}
	// Bounds come from config, so the panel can label a shrink as bounded.
	if out.Bounds.MinTrades != 12 || out.Bounds.MinSizeMultiplier != 0.25 {
		t.Fatalf("bounds not mapped from config: %+v", out.Bounds)
	}
	if out.Bounds.ScoreBuckets <= 1 {
		t.Fatalf("unset bounds must fall back to the shipped defaults, got %+v", out.Bounds)
	}
}

// With no snapshot yet (a fresh install) the endpoint answers with an
// inactive zero state rather than an error: "nothing learned yet" is a
// fact, not a fault.
func TestLearningEndpointWithNoHistory(t *testing.T) {
	s := newTestServer(t, t.TempDir())
	s.cfg = &config.Config{}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/learning")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cold start should be 200, got %d", resp.StatusCode)
	}
	var out LearningResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Adj.Active {
		t.Fatal("empty state reported as active")
	}
	if out.Adj.Classes == nil || len(out.Adj.Classes) != 0 {
		t.Fatalf("empty state must still carry a usable class map: %+v", out.Adj.Classes)
	}
}

// A snapshot the bot could not write cleanly must surface as an error
// status: showing an empty table would read as "learning found nothing",
// which is a different claim from "the file is unreadable".
func TestLearningEndpointReportsCorruptState(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	s.cfg = &config.Config{}
	if err := os.WriteFile(filepath.Join(dir, "learning-state.json"), []byte("{ broken"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/learning")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("corrupt state must be 503, got %d", resp.StatusCode)
	}
	var e map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("error body: %v", err)
	}
	if _, ok := e["error"]; !ok {
		t.Fatalf("error envelope missing the error field: %+v", e)
	}
}

func TestSelfHealEndpointServesStoredReport(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	s.cfg = &config.Config{
		SelfHealEnabled: true, SelfHealEveryTicks: 6,
		SelfHealOrphanHours: 24, SelfHealReportOnly: true,
	}
	store := selfheal.NewReportStore(filepath.Join(dir, "trades.jsonl"))
	if err := store.Save(selfheal.Report{
		At: time.Now().UTC(),
		Actions: []selfheal.Action{
			{Check: "orphan-protective-order", Severity: "high", Detail: "AAPL stop resting with no position — cancelled", Repaired: true},
		},
		Errors: []string{"open orders unavailable: 429"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/selfheal")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var out SelfHealResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || !out.ReportOnly || out.EveryTicks != 6 {
		t.Fatalf("config not surfaced: %+v", out)
	}
	if len(out.Report.Actions) != 1 || !out.Report.Actions[0].Repaired {
		t.Fatalf("report lost: %+v", out.Report.Actions)
	}
	// A report with skipped checks must not read as clean on the wire:
	// Clean() is the bot's own verdict and the API must not re-derive a
	// friendlier one.
	if out.Report.Clean() {
		t.Fatal("a report with errors reads as clean")
	}

	// Fresh install: no file, empty report, still 200.
	s2 := newTestServer(t, t.TempDir())
	ts2 := httptest.NewServer(s2.routes())
	defer ts2.Close()
	r2, err := http.Get(ts2.URL + "/api/selfheal")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("cold start selfheal must be 200, got %d", r2.StatusCode)
	}
	var out2 SelfHealResponse
	if err := json.NewDecoder(r2.Body).Decode(&out2); err != nil {
		t.Fatal(err)
	}
	if len(out2.Report.Actions) != 0 {
		t.Fatalf("cold start reported actions: %+v", out2.Report.Actions)
	}
}

// The report's own posture outranks this process's config. An operator
// flipping SELFHEAL_REPORT_ONLY and restarting only the bot would
// otherwise leave the dashboard claiming the wrong thing about whether
// anything was allowed to cancel.
func TestSelfHealPrefersTheBotsPosture(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	// This API process believes it is in report-only mode with a 48h TTL.
	s.cfg = &config.Config{
		SelfHealEnabled: true, SelfHealReportOnly: true, SelfHealOrphanHours: 48,
	}
	store := selfheal.NewReportStore(filepath.Join(dir, "trades.jsonl"))
	if err := store.Save(selfheal.Report{
		At:      time.Now().UTC(),
		Posture: &selfheal.Posture{ReportOnly: false, HaltLiftEnabled: true, OrphanMaxAgeHours: 12},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/selfheal")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out SelfHealResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ReportOnly {
		t.Fatal("the dashboard claimed report-only while the bot was acting")
	}
	if !out.HaltLift {
		t.Fatal("the bot's halt-lift posture was dropped")
	}
	if out.OrphanHours != 12 {
		t.Fatalf("orphan age = %d, want the bot's 12", out.OrphanHours)
	}

	// With no posture recorded (an older report on disk), the API's own
	// config is the only thing left to show — and it must not silently
	// claim defaults.
	if err := os.WriteFile(filepath.Join(dir, "selfheal-report.json"),
		[]byte(`{"at":"2026-01-01T00:00:00Z","actions":[]}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	r2, err := http.Get(ts.URL + "/api/selfheal")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var out2 SelfHealResponse
	if err := json.NewDecoder(r2.Body).Decode(&out2); err != nil {
		t.Fatal(err)
	}
	if !out2.ReportOnly || out2.OrphanHours != 48 {
		t.Fatalf("without a posture the API must fall back to its own config: %+v", out2)
	}
}

// Both endpoints are read-only: a POST is a mistake, not a command.
func TestLearningAndSelfHealRejectWrites(t *testing.T) {
	s := newTestServer(t, t.TempDir())
	s.cfg = &config.Config{}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	for _, path := range []string{"/api/learning", "/api/selfheal"} {
		resp, err := http.Post(ts.URL+path, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: POST returned %d, want 405", path, resp.StatusCode)
		}
	}
}
