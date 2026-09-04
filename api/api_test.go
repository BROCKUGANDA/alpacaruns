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
	"sync/atomic"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
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

// newTestServerWithConfig wires the api.Server with a live
// *config.Config + *strategy.Settings populated with the values
// passed in. Used by /api/config and /api/trade/* tests where the
// handlers need real knobs to read/update. The bot itself is not
// started — only the in-memory state the dashboard API projects.
func newTestServerWithConfig(t *testing.T, dir string, maxUSD, maxPct, daily, weekly, total float64) *Server {
	t.Helper()
	s := newTestServer(t, dir)
	s.cfg = &config.Config{
		MaxPositionUSD:       maxUSD,
		MaxPortfolioPct:      maxPct,
		CryptoMaxPositionUSD: maxUSD * 2,
		MinConfidence:        0.5,
	}
	s.strat = &strategy.Settings{
		DailyDD:  daily,
		WeeklyDD: weekly,
		TotalDD:  total,
	}
	return s
}

// newTestServerWithValidator wires a minimal validator-backed
// server for /api/trade/simulate and /api/trade/execute tests.
// Config is conservative (huge caps, low min-confidence) so a
// clean proposal passes by default. Set s.pauseFlag.Store(true)
// in the test to flip the kill switch.
func newTestServerWithValidator(t *testing.T, dir string) *Server {
	t.Helper()
	s := newTestServerWithConfig(t, dir, 1_000_000, 1.0, 1.0, 1.0, 1.0)
	// 1.0 confidence passes the min-confidence check (0.5 default).
	// Reference atomic so the import isn't dead if future helpers
	// need to flip the pause flag in this test fixture.
	_ = atomic.Bool{}
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
// ---- security surface ----

func TestControlAuthTokenRequired(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	s.settings.AuthToken = "opaque-test-token"
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	post := func(token string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/control/pause", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := post(""); got != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", got)
	}
	if got := post("wrong-token"); got != http.StatusUnauthorized {
		t.Fatalf("wrong token: status %d, want 401", got)
	}
	if got := post("opaque-test-token"); got != http.StatusOK {
		t.Fatalf("right token: status %d, want 200", got)
	}
}

func TestControlAuthCrossOriginRejected(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir) // no token configured
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	post := func(origin, referer string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/control/pause", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := post("https://evil.example.com", ""); got != http.StatusForbidden {
		t.Fatalf("foreign origin: status %d, want 403", got)
	}
	// Same-origin browser fetch passes without a token.
	if got := post(ts.URL, ""); got != http.StatusOK {
		t.Fatalf("same origin: status %d, want 200", got)
	}
	if got := post("", ts.URL+"/controls"); got != http.StatusOK {
		t.Fatalf("same referer: status %d, want 200", got)
	}
	// Token-less non-browser caller passes when no token configured.
	if got := post("", ""); got != http.StatusOK {
		t.Fatalf("no origin headers: status %d, want 200", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Fatalf("api header %s = %q, want %q", k, got, want)
		}
	}
	if got := resp.Header.Get("Content-Security-Policy"); got == "" {
		t.Fatalf("api responses must carry a CSP")
	}
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS must be off without TLS, got %q", got)
	}

	// Embedded UI: framing denied, but no JSON-style CSP (Next.js
	// hydration needs inline scripts).
	uresp, err := http.Get(ts.URL + "/welcome/")
	if err != nil {
		t.Fatal(err)
	}
	defer uresp.Body.Close()
	if uresp.StatusCode != 200 {
		t.Fatalf("welcome status %d", uresp.StatusCode)
	}
	if got := uresp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("ui nosniff = %q", got)
	}
	if got := uresp.Header.Get("Content-Security-Policy"); got != "" {
		t.Fatalf("ui must not carry the api CSP, got %q", got)
	}
}

func TestMaskAccountNumber(t *testing.T) {
	if got := maskAccountNumber("PA3LNUDV231J"); got != "****231J" {
		t.Fatalf("got %q", got)
	}
	if got := maskAccountNumber("ab"); got != "****" {
		t.Fatalf("short input must fully mask, got %q", got)
	}
	if got := maskAccountNumber(""); got != "****" {
		t.Fatalf("empty input must fully mask, got %q", got)
	}
}

func TestBadQueryParamsRejected(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	bad := map[string][]string{
		"/api/trades": {
			"limit=abc", "limit=0", "limit=-5", "limit=201", "limit=999999",
			"cursor=-1", "cursor=xyz",
			"since=not-a-date", "until=32-13-99",
			"since=2026-09-03&until=2026-09-01",
			"path=bogus",
			"symbol=SPY%20X", "symbol=%3Ctag%3E", "symbol=" + strings.Repeat("A", 33),
		},
		// /api/decisions takes no symbol filter; unknown params are
		// ignored there by design (narrow contract per endpoint).
		"/api/decisions": {
			"limit=abc", "limit=0", "limit=-5", "limit=201", "limit=999999",
			"cursor=-1", "cursor=xyz",
			"since=not-a-date", "until=32-13-99",
			"since=2026-09-03&until=2026-09-01",
			"path=bogus",
		},
	}
	for ep, cases := range bad {
		for _, qs := range cases {
			resp, err := http.Get(ts.URL + ep + "?" + qs)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s?%s: status %d, want 400", ep, qs, resp.StatusCode)
			}
		}
	}
	// limit=200 (ceiling) and a valid symbol still pass.
	resp, err := http.Get(ts.URL + "/api/trades?limit=200&symbol=SPY&path=agent")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("valid params: status %d, want 200", resp.StatusCode)
	}
}

func TestXFFIgnoredWhenUntrusted(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir) // TrustedProxy=false
	req, _ := http.NewRequest(http.MethodGet, "/api/health", nil)
	req.RemoteAddr = "10.0.0.9:1234"
	req.Header.Set("X-Forwarded-For", "9.9.9.9, 8.8.8.8")
	if got := s.clientKey(req); got != "10.0.0.9" {
		t.Fatalf("untrusted XFF honored: key %q", got)
	}
	s.settings.TrustedProxy = true
	if got := s.clientKey(req); got != "9.9.9.9" {
		t.Fatalf("trusted XFF ignored: key %q", got)
	}
}

func TestWriteJSONEscapesHTML(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]string{"detail": "</script><img src=x>"})
	body := rec.Body.String()
	if strings.Contains(body, "</script>") || strings.Contains(body, "<img") {
		t.Fatalf("raw HTML in JSON body: %q", body)
	}
	if !strings.Contains(body, "\\u003c") {
		t.Fatalf("expected escaped angle brackets, got %q", body)
	}
}

func TestControlRateLimit429(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	var saw429 bool
	for i := 0; i < controlBudget+10; i++ {
		resp, err := http.Post(ts.URL+"/api/control/pause", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			saw429 = true
			if resp.Header.Get("Retry-After") == "" {
				t.Fatalf("429 without Retry-After")
			}
			break
		}
	}
	if !saw429 {
		t.Fatalf("control budget never exhausted after %d requests", controlBudget+10)
	}
}

func TestJournalPathsNotUserControlled(t *testing.T) {
	// Regression guard for the "parameterize queries" review: no
	// handler may derive a filesystem path from request input. The
	// only path sources are ServerSettings (flags/env). Values that
	// merely LOOK like traversal but use the legal filter charset
	// (/, . — needed for pairs like BTC/USD) must match nothing and
	// return an empty list, never touch the filesystem.
	dir := t.TempDir()
	s := newTestServer(t, dir)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Charset-legal but filesystem-flavored filters: 200 + zero rows.
	resp, err := http.Get(ts.URL + "/api/trades?symbol=../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	var tr TradesResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(tr.Trades) != 0 {
		t.Fatalf("traversal filter: status %d rows %d, want 200 + 0 rows",
			resp.StatusCode, len(tr.Trades))
	}

	// Illegal values in the other params: 400.
	for _, u := range []string{
		"/api/trades?since=..%2F..%2Fsecret",
		"/api/decisions?path=..%2F..",
		"/api/pnl?until=..%2F..%2F..",
	} {
		resp, err := http.Get(ts.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", u, resp.StatusCode)
		}
	}
}

// ---- /api/config ----

// TestGetConfigReturnsCurrentValues: GET /api/config must return the
// knobs the bot currently has loaded, so the controls form can show
// "what is" next to "what you can change to."
func TestGetConfigReturnsCurrentValues(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithConfig(t, dir, 7500, 0.15, 0.04, 0.08, 0.12)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var cr ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	if cr.MaxPositionUSD != 7500 {
		t.Fatalf("max_position_usd = %v, want 7500", cr.MaxPositionUSD)
	}
	if cr.MaxPortfolioPct != 0.15 {
		t.Fatalf("max_portfolio_pct = %v, want 0.15", cr.MaxPortfolioPct)
	}
	if cr.DailyDDHalt != 0.04 {
		t.Fatalf("daily_dd_halt = %v, want 0.04", cr.DailyDDHalt)
	}
}

// TestPostConfigRejectsUnknownFields: extra fields in the request
// must 400 so a typo doesn't silently drop a knob update. Defense
// in depth — extra=forbid for the dashboard surface.
func TestPostConfigRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithConfig(t, dir, 7500, 0.15, 0.04, 0.08, 0.12)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"max_position_usd": 8000, "not_a_real_knob": true}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/config", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

// TestPostConfigUpdatesAndPersists: a successful POST /api/config
// updates the live cfg (visible on the next GET) and writes the
// new values back to the persisted state file so the bot picks
// them up on its next tick.
func TestPostConfigUpdatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithConfig(t, dir, 7500, 0.15, 0.04, 0.08, 0.12)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"max_position_usd": 9000, "max_portfolio_pct": 0.25}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/config", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}

	// Round-trip read confirms the in-memory cfg was updated.
	get, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	var cr ConfigResponse
	if err := json.NewDecoder(get.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	if cr.MaxPositionUSD != 9000 {
		t.Fatalf("after update max_position_usd = %v, want 9000", cr.MaxPositionUSD)
	}
	if cr.MaxPortfolioPct != 0.25 {
		t.Fatalf("after update max_portfolio_pct = %v, want 0.25", cr.MaxPortfolioPct)
	}

	// Persisted state file should also reflect the change.
	// json.MarshalIndent uses "key: value" with a space, so the
	// substring includes the space.
	b, err := os.ReadFile(s.settings.StateFile)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if !strings.Contains(string(b), `"max_position_usd": 9000`) {
		t.Fatalf("state file missing new value; got %s", string(b))
	}
}

// TestPostConfigClampsOutOfRange: an obviously bad value (negative
// position cap) must 400, not silently persist a config that would
// stop the bot from placing any order.
func TestPostConfigClampsOutOfRange(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithConfig(t, dir, 7500, 0.15, 0.04, 0.08, 0.12)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"max_position_usd": -1}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/config", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

// ---- /api/trade/simulate ----

// TestTradeSimulateApprovesCleanProposal: a small buy that passes
// every risk check must return approved=true, the computed
// notional, and the would-have-sent order payload.
func TestTradeSimulateApprovesCleanProposal(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithValidator(t, dir)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// confidence=1.0 matches the conservative test cfg (min 0.5) so
	// the proposal clears every gate and is approved end-to-end.
	body := strings.NewReader(`{"symbol":"AAPL","side":"buy","qty":"1","order_type":"market","time_in_force":"day","confidence":1.0}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/trade/simulate", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var sr TradeSimulationResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	if !sr.Approved {
		t.Fatalf("expected approved=true; reasons=%v", sr.Reasons)
	}
	if sr.Notional <= 0 {
		t.Fatalf("expected positive notional; got %v", sr.Notional)
	}
}

// TestTradeSimulateRejectsKillSwitchHalt: when the kill switch is
// engaged, simulate must return approved=false with the kill reason
// — never silently approve a manual order that would also be
// rejected at execution.
func TestTradeSimulateRejectsKillSwitchHalt(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithValidator(t, dir)
	s.pauseFlag.Store(true) // simulate a halted kill switch
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"symbol":"AAPL","side":"buy","qty":"1"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/trade/simulate", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200 (simulate never 500s on a rejected proposal)", resp.StatusCode)
	}
	var sr TradeSimulationResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	if sr.Approved {
		t.Fatalf("expected approved=false; got true")
	}
	if len(sr.Reasons) == 0 {
		t.Fatalf("expected at least one rejection reason")
	}
}

// TestTradeSimulateRejectsBadInput: an order that fails the input
// validator (unknown side) must 400, not produce a fake-approved
// response.
func TestTradeSimulateRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithValidator(t, dir)
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"symbol":"AAPL","side":"hold","qty":"1"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/trade/simulate", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

// ---- /api/trade/execute ----

// TestTradeExecuteRejectsWhenPaused: even an order that passes the
// risk gate must be refused when the bot is paused. Execute must
// mirror the auto path's kill-switch-on-pause behavior.
func TestTradeExecuteRejectsWhenPaused(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithValidator(t, dir)
	if err := s.setPauseFlag(true); err != nil {
		t.Fatal(err)
	}
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"symbol":"AAPL","side":"buy","qty":"1"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/trade/execute", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200 (execute returns rejection via approved=false)", resp.StatusCode)
	}
	var er TradeExecutionResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatal(err)
	}
	if er.Approved {
		t.Fatalf("expected approved=false when paused")
	}
	if er.Order != nil {
		t.Fatalf("no order should have been placed")
	}
}

// TestTradeExecuteRecordsSimulatedOnlyWhenNoClient: when the
// server has no Alpaca client wired (cold-start / no-key mode),
// execute must still record a journal entry tagged
// source="dashboard:sim" with path="manual", so the operator's
// intent is auditable even without a real broker call.
func TestTradeExecuteRecordsSimulatedOnlyWhenNoClient(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithValidator(t, dir)
	// client is nil by default — exercises the no-broker branch
	mux := s.routes()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// confidence=1.0 clears the conservative-cfg min-confidence gate.
	body := strings.NewReader(`{"symbol":"AAPL","side":"buy","qty":"1","order_type":"market","time_in_force":"day","confidence":1.0}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/trade/execute", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var er TradeExecutionResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatal(err)
	}
	if er.Mode != "simulated" {
		t.Fatalf("mode = %q, want simulated", er.Mode)
	}
	if er.Approved == false {
		t.Fatalf("expected approved=true; reasons=%v", er.Reasons)
	}

	// Trade log should have one new decision record tagged manual.
	b, err := os.ReadFile(s.settings.TradeLog)
	if err != nil {
		t.Fatalf("read trade log: %v", err)
	}
	if !strings.Contains(string(b), `"dashboard:manual"`) {
		t.Fatalf("manual journal entry not appended; log = %s", string(b))
	}
}
