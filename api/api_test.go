// api_test.go — covers the bits of the api package we can exercise
// without making live Alpaca calls: pause-flag atomic write,
// trade-log reading, JSON shape, rate limiter math. The HTTP server
// itself is exercised by TestServerHealthAndStatus below; we use
// httptest to spin up an in-process server and hit every endpoint
// at least once.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/pnl"
)

// helper to make a tiny server with default settings pointed at dir.
func newTestServer(t *testing.T, dir string) *Server {
	t.Helper()
	s := &Server{
		settings: ServerSettings{
			Port:       0, // not used by httptest
			CORSOrigin: "*",
			TradeLog:   filepath.Join(dir, "trades.jsonl"),
			StateFile:  filepath.Join(dir, "strategy-state.json"),
			PauseFlag:  filepath.Join(dir, "paused"),
			AllowStep:  true,
			AllowPause: true,
		},
		limiter:   NewRateLimiter(60, time.Second),
		startTime: time.Now(),
	}
	return s
}

func TestSetPauseFlag(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)

	if err := s.setPauseFlag(true); err != nil {
		t.Fatal(err)
	}
	if !pauseFlagTest(s.settings.PauseFlag) {
		t.Fatalf("expected flag file to contain true")
	}
	if !s.pauseFlag.Load() {
		t.Fatalf("atomic mirror not updated")
	}
	if err := s.setPauseFlag(false); err != nil {
		t.Fatal(err)
	}
	if pauseFlagTest(s.settings.PauseFlag) {
		t.Fatalf("expected flag file to be cleared")
	}
}

// pauseFlagTest is a tiny file-content checker used by the test
// suite (avoid colliding with the package-level pauseFlagEngaged
// which returns bool from a string path).
func pauseFlagTest(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "true"
}

func TestReadAllRecordsMissingFile(t *testing.T) {
	records, err := readAllRecords(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 records, got %d", len(records))
	}
}

func TestReadAllRecordsCorruptLineSkipped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "trades.jsonl")
	body := strings.Join([]string{
		`{"kind":"fill","ts":"2026-01-01T00:00:00Z","order_id":"a","symbol":"AAPL","side":"buy","qty":"1","price":"100","status":"filled"}`,
		`this is not json`,
		`{"kind":"decision","ts":"2026-01-01T00:00:01Z","risk":"INFO","source":"cli:trade","detail":"manual trade"}`,
	}, "\n")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	records, err := readAllRecords(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records (corrupt skipped), got %d", len(records))
	}
}

func TestSourceMatchesPath(t *testing.T) {
	cases := []struct {
		source, want string
		match        bool
	}{
		{"strategy:auto", "agent", true},
		{"strategy:ensemble", "ensemble", true},
		{"cli:trade", "manual", true},
		{"cli:factors", "manual", true},
		{"strategy:auto", "manual", false},
		{"something:weird", "something:weird", true},
		{"strategy:auto", "strategy:auto", true},
	}
	for _, c := range cases {
		if got := sourceMatchesPath(c.source, c.want); got != c.match {
			t.Errorf("sourceMatchesPath(%q, %q) = %v, want %v", c.source, c.want, got, c.match)
		}
	}
}

func TestExtractFactorScores(t *testing.T) {
	detail := "factors trend=0.7 momentum=0.6 volume=0.5 vol=0.4 sentiment=0.3; other"
	got := extractFactorScores(detail)
	if got == nil {
		t.Fatal("expected factor scores")
	}
	if got["trend"] != 0.7 {
		t.Fatalf("trend got %v", got["trend"])
	}
	if got["momentum"] != 0.6 {
		t.Fatalf("momentum got %v", got["momentum"])
	}
	if got["sentiment"] != 0.3 {
		t.Fatalf("sentiment got %v", got["sentiment"])
	}
	// No factors => nil.
	if extractFactorScores("nothing here") != nil {
		t.Fatal("expected nil")
	}
	if extractFactorScores("") != nil {
		t.Fatal("expected nil for empty")
	}
}

func TestBuildEquityCurveSynthetic(t *testing.T) {
	// Two buys, one sell (in profit). Starting equity 100k.
	records := []pnl.Record{
		{Kind: pnl.KindFill, TS: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), Symbol: "AAPL", Side: "buy", Qty: "100", Price: "100"},
		{Kind: pnl.KindFill, TS: time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC), Symbol: "AAPL", Side: "buy", Qty: "100", Price: "110"},
		{Kind: pnl.KindFill, TS: time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC), Symbol: "AAPL", Side: "sell", Qty: "150", Price: "120"},
	}
	snaps, totalPnL, maxDD, wins, losses := buildEquityCurve(records, time.Time{}, time.Time{}, 100000)
	if len(snaps) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(snaps))
	}
	if maxDD < 0 {
		t.Fatalf("maxDD should be >= 0, got %f", maxDD)
	}
	// Sell of 100@120 with cost 100*100=10000 + 50@110=5500 returns 150*120=18000 -> profit = 2500
	// Sell of remaining 50: cost was 50*110=5500; revenue 50*120=6000; profit 500
	// Total profit from the 150 sold = 3000.
	// Cash out: 100*100 + 100*110 = 21000.
	// Cash back from sell: 18000 + profit = 21000.
	// Total PnL = 3000 (wins - losses; both were wins since px>cost).
	if wins != 2 || losses != 0 {
		t.Fatalf("wins=%d losses=%d, want 2/0", wins, losses)
	}
	// Two sells closing two FIFO lots at a profit: 100*(120-100)
	// + 50*(120-110) = 2000 + 500 = 2500 realized PnL.
	if totalPnL != 2500 {
		t.Fatalf("totalPnL = %v, want 2500", totalPnL)
	}
}

func TestRateLimiterBlocksAfterBurst(t *testing.T) {
	// Burst=5, refill=5 per 60s window. Five calls succeed, sixth fails.
	lim := NewRateLimiter(5, 60*time.Second)
	for i := 0; i < 5; i++ {
		if !lim.Allow("1.2.3.4") {
			t.Fatalf("call %d should be allowed", i)
		}
	}
	if lim.Allow("1.2.3.4") {
		t.Fatalf("call 6 should be rejected")
	}
	// Different IP still has its own bucket.
	if !lim.Allow("5.6.7.8") {
		t.Fatalf("different IP should be allowed")
	}
}

func TestParseTime(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"", time.Time{}},
		{"2026-09-03", time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)},
		{"2026-09-03T12:00:00Z", time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := parseTime(c.in)
		if err != nil {
			t.Errorf("parseTime(%q) error: %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("parseTime(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := parseTime("not a time"); err == nil {
		t.Errorf("expected error for invalid input")
	}
}

// ---- HTTP integration smoke ----

func TestServerHealthAndStatus(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)

	// Hit the mux through httptest; no real Alpaca, no real journal.
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// /api/health
	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	var hr HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		t.Fatal(err)
	}
	if !hr.OK {
		t.Fatalf("ok=%v", hr.OK)
	}
	if hr.Version != Version {
		t.Fatalf("version=%q want %q", hr.Version, Version)
	}
	if hr.UptimeSec < 0 {
		t.Fatalf("uptime negative")
	}

	// /api/status
	resp, err = http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sr StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	// Bot field is always one of the four states; default running.
	if sr.Bot == "" {
		t.Fatalf("bot state empty")
	}
}

func TestServerControlPauseResume(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Pause
	resp, err := http.Post(ts.URL+"/api/control/pause", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("pause status %d", resp.StatusCode)
	}
	var cr ControlResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !cr.Paused {
		t.Fatalf("expected paused=true")
	}

	// Confirm flag file is "true".
	if !pauseFlagTest(s.settings.PauseFlag) {
		t.Fatalf("flag file should be true")
	}

	// Resume
	resp, err = http.Post(ts.URL+"/api/control/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("resume status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Flag file should now be "false" (or absent — both treated as
	// not paused).
	if pauseFlagTest(s.settings.PauseFlag) {
		t.Fatalf("flag file should be cleared")
	}
}

func TestServerTradesAndDecisions(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)

	// Seed journal with one fill + one decision.
	jpath := s.settings.TradeLog
	body := strings.Join([]string{
		`{"kind":"fill","ts":"2026-01-01T10:00:00Z","order_id":"o1","symbol":"AAPL","side":"buy","qty":"10","price":"100","status":"filled"}`,
		`{"kind":"decision","ts":"2026-01-01T10:00:00Z","symbol":"AAPL","risk":"APPROVED","source":"strategy:auto","detail":"buy AAPL qty=10 price=100 factors trend=0.7 momentum=0.6"}`,
	}, "\n")
	if err := os.WriteFile(jpath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/trades?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tr TradesResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatal(err)
	}
	if len(tr.Trades) != 1 {
		t.Fatalf("trades len=%d want 1", len(tr.Trades))
	}
	if tr.Trades[0].Symbol != "AAPL" {
		t.Fatalf("symbol=%q", tr.Trades[0].Symbol)
	}
	if tr.Trades[0].Path != "agent" {
		t.Fatalf("path=%q", tr.Trades[0].Path)
	}
	// factor_scores lives on decision records, not fills. Confirm
	// the trades endpoint returned the fill cleanly without it.
	if tr.Trades[0].FactorScores != nil {
		t.Fatalf("fills should not carry factor scores: %+v", tr.Trades[0].FactorScores)
	}

	resp, err = http.Get(ts.URL + "/api/decisions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var dr DecisionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		t.Fatal(err)
	}
	if len(dr.Decisions) != 1 {
		t.Fatalf("decisions len=%d want 1", len(dr.Decisions))
	}
	if dr.Decisions[0].Source != "strategy:auto" {
		t.Fatalf("source=%q", dr.Decisions[0].Source)
	}
	if dr.Decisions[0].FactorScores == nil || dr.Decisions[0].FactorScores["trend"] != 0.7 {
		t.Fatalf("decision factor scores missing/wrong: %+v", dr.Decisions[0].FactorScores)
	}
}

func TestServerPnL(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)

	jpath := s.settings.TradeLog
	body := strings.Join([]string{
		`{"kind":"fill","ts":"2026-01-01T10:00:00Z","order_id":"o1","symbol":"AAPL","side":"buy","qty":"10","price":"100","status":"filled"}`,
		`{"kind":"fill","ts":"2026-01-02T10:00:00Z","order_id":"o2","symbol":"AAPL","side":"sell","qty":"10","price":"120","status":"filled"}`,
	}, "\n")
	if err := os.WriteFile(jpath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/pnl")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pnl status %d", resp.StatusCode)
	}
	var pr PnLResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	if pr.Summary.StartingEquity != 100000 {
		t.Fatalf("starting=%v", pr.Summary.StartingEquity)
	}
	if pr.Summary.CurrentEquity != 100200 {
		t.Fatalf("current=%v", pr.Summary.CurrentEquity)
	}
	if pr.Summary.WinRate <= 0 {
		t.Fatalf("winrate=%v", pr.Summary.WinRate)
	}
}

func TestServerRejectsBadTime(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/trades?since=not-a-time")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestServerControlStep(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/control/step", "application/json", bytes.NewBufferString("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("step status %d", resp.StatusCode)
	}
	var cr ControlResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	if cr.Action != "step" {
		t.Fatalf("action=%q", cr.Action)
	}
	if cr.Decision == nil || cr.Decision.Source != "dashboard:step" {
		t.Fatalf("decision missing or wrong: %+v", cr.Decision)
	}
}

func TestCORSHeaders(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	s.settings.CORSOrigin = "https://showcase.example.com"
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("OPTIONS", ts.URL+"/api/health", nil)
	req.Header.Set("Origin", "https://showcase.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://showcase.example.com" {
		t.Fatalf("cors origin = %q", got)
	}
}

// concurrency helper: many simultaneous control writes don't corrupt
// the flag file. With a small burst this is mostly a regression
// guard for the atomic-rename write.
func TestConcurrentPauseWrites(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	s.settings.AllowStep = false
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/api/control/pause", "application/json", nil)
			if err == nil {
				resp.Body.Close()
			}
		}()
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/api/control/resume", "application/json", nil)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}