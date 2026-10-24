package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/a2a"
)

// newA2ATestServer wires a server with the bridge enabled and one
// allow-listed agent.
func newA2ATestServer(t *testing.T, dir string, mode a2a.Mode) *Server {
	t.Helper()
	s := newTestServer(t, dir)
	s.a2aCfg = a2a.Config{
		Enabled:           true,
		Token:             "a2a-secret",
		Agents:            []string{"research"},
		DefaultMode:       mode,
		ShadowPasses:      3,
		MaxNotional:       10000,
		MaxSignalsPerHour: 60,
	}
	s.bridge = a2a.NewBridge(s.a2aCfg, a2a.NewStore(filepath.Join(dir, "trades.jsonl")))
	return s
}

// TestA2AEnabledEndpoint proves the dashboard can tell whether the
// bridge is on without guessing.
func TestA2AEnabledEndpoint(t *testing.T) {
	s := newA2ATestServer(t, t.TempDir(), a2a.ModeOff)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/a2a")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var out A2AEnabled
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Enabled {
		t.Fatal("bridge must report as enabled")
	}
}

// TestA2ASignalRequiresToken proves the ingress is authenticated.
func TestA2ASignalRequiresToken(t *testing.T) {
	s := newA2ATestServer(t, t.TempDir(), a2a.ModeOn)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	body := `{"id":"s1","agent":"research","symbol":"AAPL","side":"buy","qty":"1","confidence":0.8}`
	// No token.
	resp, err := http.Post(ts.URL+"/api/a2a/signals", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 without a token", resp.StatusCode)
	}
	// Wrong token.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/a2a/signals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 with a wrong token", resp2.StatusCode)
	}
	// Correct token.
	req3, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/a2a/signals", strings.NewReader(body))
	req3.Header.Set("Authorization", "Bearer a2a-secret")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("status %d, want 200 with the right token", resp3.StatusCode)
	}
	var out A2ASignalResponse
	if err := json.NewDecoder(resp3.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Accepted {
		t.Fatalf("signal must be accepted in on-mode: %+v", out)
	}
}

// TestA2ASignalRefusesUnlistedAgent proves the allow-list is enforced
// at the HTTP surface.
func TestA2ASignalRefusesUnlistedAgent(t *testing.T) {
	s := newA2ATestServer(t, t.TempDir(), a2a.ModeOn)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	body := `{"id":"s2","agent":"stranger","symbol":"AAPL","side":"buy","qty":"1","confidence":0.8}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/a2a/signals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer a2a-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200 (the refusal is in the body)", resp.StatusCode)
	}
	var out A2ASignalResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Accepted {
		t.Fatal("an unlisted agent must not be accepted")
	}
}

// TestA2ARampEndpoint proves the ramp can be driven from the API and
// that the shadow-pass requirement is enforced there too.
func TestA2ARampEndpoint(t *testing.T) {
	s := newA2ATestServer(t, t.TempDir(), a2a.ModeShadow)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	// Straight to on: refused, the agent has not earned it.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/a2a/agents/research/ramp",
		strings.NewReader(`{"mode":"on"}`))
	req.Header.Set("Authorization", "Bearer a2a-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (ramp-on refused without shadow passes)", resp.StatusCode)
	}

	// Shadow -> off always works.
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/a2a/agents/research/ramp",
		strings.NewReader(`{"mode":"off"}`))
	req2.Header.Set("Authorization", "Bearer a2a-secret")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("status %d, want 200 (ramp-off must always work)", resp2.StatusCode)
	}
	var state a2a.AgentState
	if err := json.NewDecoder(resp2.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.Mode != a2a.ModeOff {
		t.Fatalf("mode = %s, want off", state.Mode)
	}
}

// TestA2AAgentsEndpoint proves the agent list is readable.
func TestA2AAgentsEndpoint(t *testing.T) {
	s := newA2ATestServer(t, t.TempDir(), a2a.ModeShadow)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/a2a/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var out A2AAgentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Agents == nil {
		t.Fatal("agents must be an empty array, never null")
	}
}

// TestA2ADisabledRefusesSignals proves a disabled bridge does not
// accept anything, even with the right token.
func TestA2ADisabledRefusesSignals(t *testing.T) {
	s := newTestServer(t, t.TempDir())
	s.a2aCfg = a2a.Config{Enabled: false}
	s.bridge = a2a.NewBridge(s.a2aCfg, a2a.NewStore(filepath.Join(t.TempDir(), "trades.jsonl")))
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/a2a/signals",
		strings.NewReader(`{"id":"s3","agent":"research","symbol":"AAPL","side":"buy","qty":"1","confidence":0.8}`))
	req.Header.Set("Authorization", "Bearer a2a-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 when the bridge is disabled", resp.StatusCode)
	}
	_ = time.Now
}
