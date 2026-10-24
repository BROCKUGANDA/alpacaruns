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

// postA2ASignal posts a signal with the given bearer token (empty sends
// no Authorization header at all) and returns the status plus whatever
// body came back.
func postA2ASignal(t *testing.T, base, bearer, body string) (int, A2ASignalResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/a2a/signals", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out A2ASignalResponse
	// A 401 carries the error envelope rather than this shape; the status
	// is the assertion, so a decode miss is not one.
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// acceptedCounts reads one agent's ingress counters out of the bridge.
func acceptedCounts(t *testing.T, b *a2a.Bridge, agent string) a2a.AgentState {
	t.Helper()
	states, err := b.States()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range states {
		if a.Agent == agent {
			return a
		}
	}
	return a2a.AgentState{}
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

	body := `{"id":"s1","agent":"research","symbol":"AAPL","side":"buy","qty":"1","notional":"50","confidence":0.8}`
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
	body := `{"id":"s2","agent":"stranger","symbol":"AAPL","side":"buy","qty":"1","notional":"50","confidence":0.8}`
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

// newPeerBoundA2AServer wires a shared group token AND a per-peer
// credential for "research". "momentum" is allow-listed but has no
// credential of its own — the state a real deployment passes through
// while peers are being onboarded. settings.AuthToken is the
// operator/dashboard credential, which is what the ramp endpoint guards
// with.
func newPeerBoundA2AServer(t *testing.T, dir string, mode a2a.Mode) *Server {
	t.Helper()
	s := newTestServer(t, dir)
	s.settings.AuthToken = "operator-token"
	s.a2aCfg = a2a.Config{
		Enabled:           true,
		Token:             "a2a-group",
		Agents:            []string{"research", "momentum"},
		DefaultMode:       mode,
		ShadowPasses:      3,
		MaxNotional:       10000,
		MaxSignalsPerHour: 60,
		PeerTokens:        map[string]string{"research": "research-token"},
	}
	s.bridge = a2a.NewBridge(s.a2aCfg, a2a.NewStore(filepath.Join(dir, "trades.jsonl")))
	return s
}

// TestA2ASignalPeerTokenBindsSenderIdentity is the impersonation fix at
// the HTTP surface: one shared bridge token used to let any holder post as
// any allow-listed agent, including one ramped on.
func TestA2ASignalPeerTokenBindsSenderIdentity(t *testing.T) {
	dir := t.TempDir()
	s := newPeerBoundA2AServer(t, dir, a2a.ModeOn)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	// research may only be spoken for by its own token.
	code, out := postA2ASignal(t, ts.URL, "research-token",
		`{"id":"r1","agent":"research","symbol":"AAPL","side":"buy","notional":"100","confidence":0.8}`)
	if code != http.StatusOK || !out.Accepted {
		t.Fatalf("the peer's own token must authenticate it: status %d %+v", code, out)
	}
	if out.Mode != string(a2a.ModeOn) {
		t.Fatalf("mode = %q, want on", out.Mode)
	}
	// The group token may NOT impersonate a bound agent.
	if code, out = postA2ASignal(t, ts.URL, "a2a-group",
		`{"id":"r2","agent":"research","symbol":"AAPL","side":"buy","notional":"100","confidence":0.8}`); code != http.StatusUnauthorized {
		t.Fatalf("group token impersonating a bound agent: status %d want 401 (%+v)", code, out)
	}
	// An unknown token is refused before the body is even read.
	if code, _ = postA2ASignal(t, ts.URL, "guess-me",
		`{"id":"r3","agent":"research","symbol":"AAPL","side":"buy","notional":"100","confidence":0.8}`); code != http.StatusUnauthorized {
		t.Fatalf("unknown token: status %d want 401", code)
	}
	// No token at all.
	if code, _ = postA2ASignal(t, ts.URL, "",
		`{"id":"r4","agent":"research","symbol":"AAPL","side":"buy","notional":"100","confidence":0.8}`); code != http.StatusUnauthorized {
		t.Fatalf("missing token: status %d want 401", code)
	}

	// An allow-listed agent with no credential of its own keeps using the
	// group token — that is today's deployment, unchanged.
	if code, out = postA2ASignal(t, ts.URL, "a2a-group",
		`{"id":"m1","agent":"momentum","symbol":"MSFT","side":"buy","notional":"100","confidence":0.8}`); code != http.StatusOK || !out.Accepted {
		t.Fatalf("unbound agent with the group token must pass: status %d %+v", code, out)
	}
	// One peer's credential does not extend to another agent id.
	if code, _ = postA2ASignal(t, ts.URL, "research-token",
		`{"id":"m2","agent":"momentum","symbol":"MSFT","side":"buy","notional":"100","confidence":0.8}`); code != http.StatusUnauthorized {
		t.Fatalf("research's token must not speak for momentum: status %d want 401", code)
	}

	// An agent that is not allow-listed is still refused by the bridge —
	// with a 200 and the refusal in the body, as before.
	code, out = postA2ASignal(t, ts.URL, "a2a-group",
		`{"id":"x1","agent":"stranger","symbol":"AAPL","side":"buy","notional":"100","confidence":0.8}`)
	if code != http.StatusOK || out.Accepted {
		t.Fatalf("an unlisted agent must be refused in the body: status %d %+v", code, out)
	}
	if out.Reason != "agent is not allow-listed" {
		t.Fatalf("reason = %q, want the allow-list refusal", out.Reason)
	}
	// A peer credential claimed for an unlisted id is refused outright.
	if code, _ = postA2ASignal(t, ts.URL, "research-token",
		`{"id":"x2","agent":"stranger","symbol":"AAPL","side":"buy","notional":"100","confidence":0.8}`); code != http.StatusUnauthorized {
		t.Fatalf("research's token may not claim a stranger id: status %d want 401", code)
	}

	// The refused probes must not have reached the bridge at all: no
	// accepted signal, and no journalled refusal either — the 401 paths
	// never touch the state file.
	if st := acceptedCounts(t, s.bridge, "research"); st.Accepted != 1 || st.Refused != 0 {
		t.Fatalf("research counters = %+v, want accepted 1 / refused 0", st)
	}
	if st := acceptedCounts(t, s.bridge, "momentum"); st.Accepted != 1 || st.Refused != 0 {
		t.Fatalf("momentum counters = %+v, want accepted 1 / refused 0", st)
	}
	if st := acceptedCounts(t, s.bridge, "stranger"); st.Accepted != 0 || st.Refused != 1 {
		t.Fatalf("stranger counters = %+v, want accepted 0 / refused 1", st)
	}
	// Nothing from a refused-by-credential post was queued.
	pending, err := s.bridge.Drain(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, sig := range pending {
		ids[sig.ID] = true
	}
	for _, bad := range []string{"r2", "r3", "r4", "m2", "x1", "x2"} {
		if ids[bad] {
			t.Fatalf("signal %s was queued despite failing authentication", bad)
		}
	}
	for _, good := range []string{"r1", "m1"} {
		if !ids[good] {
			t.Fatalf("authenticated signal %s must still be queued", good)
		}
	}
}

// TestA2ASignalGroupTokenUnchangedWithoutPeerTokens pins the compatibility
// requirement: with PeerTokens empty, the shared token still carries every
// allow-listed agent, so the running deployment does not break.
func TestA2ASignalGroupTokenUnchangedWithoutPeerTokens(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	s.settings.AuthToken = "operator-token"
	s.a2aCfg = a2a.Config{
		Enabled:           true,
		Token:             "a2a-group",
		Agents:            []string{"research", "momentum"},
		DefaultMode:       a2a.ModeOn,
		MaxNotional:       10000,
		MaxSignalsPerHour: 60,
	}
	s.bridge = a2a.NewBridge(s.a2aCfg, a2a.NewStore(filepath.Join(dir, "trades.jsonl")))
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	for _, tc := range []struct{ id, agent string }{
		{"legacy-1", "research"},
		{"legacy-2", "momentum"},
	} {
		code, out := postA2ASignal(t, ts.URL, "a2a-group",
			`{"id":"`+tc.id+`","agent":"`+tc.agent+`","symbol":"AAPL","side":"buy","notional":"100","confidence":0.8}`)
		if code != http.StatusOK || !out.Accepted {
			t.Fatalf("%s with the group token: status %d %+v, want 200 accepted", tc.agent, code, out)
		}
	}
	if code, _ := postA2ASignal(t, ts.URL, "not-the-token",
		`{"id":"legacy-9","agent":"research","symbol":"AAPL","side":"buy","notional":"100","confidence":0.8}`); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token must still be refused: status %d", code)
	}
}

// TestA2ARampKeepsOperatorCredential proves the peer and group bridge
// tokens cannot move the ramp: that endpoint stays on the dashboard's
// credential path.
func TestA2ARampKeepsOperatorCredential(t *testing.T) {
	s := newPeerBoundA2AServer(t, t.TempDir(), a2a.ModeShadow)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	post := func(bearer string) int {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/a2a/agents/research/ramp",
			strings.NewReader(`{"mode":"off"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("research-token"); code != http.StatusUnauthorized {
		t.Fatalf("a peer token must not drive the ramp: status %d want 401", code)
	}
	if code := post("a2a-group"); code != http.StatusUnauthorized {
		t.Fatalf("the bridge token must not drive the ramp: status %d want 401", code)
	}
	if code := post("operator-token"); code != http.StatusOK {
		t.Fatalf("the operator credential must still drive the ramp: status %d want 200", code)
	}
	if mode, err := s.bridge.ModeOf("research"); err != nil || mode != a2a.ModeOff {
		t.Fatalf("ramp = %s (err %v), want off", mode, err)
	}
}

// TestA2ASignalQtyOnlyRefusedUnderCap proves the notional bypass is closed
// at the HTTP surface too: a qty with no notional cannot be evaluated
// here, so it is refused instead of queued.
func TestA2ASignalQtyOnlyRefusedUnderCap(t *testing.T) {
	s := newA2ATestServer(t, t.TempDir(), a2a.ModeOn)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	code, out := postA2ASignal(t, ts.URL, "a2a-secret",
		`{"id":"q1","agent":"research","symbol":"TSLA","side":"buy","qty":"5000000","confidence":0.9}`)
	if code != http.StatusOK {
		t.Fatalf("status %d want 200 (the refusal belongs to the bridge)", code)
	}
	if out.Accepted {
		t.Fatalf("a qty-only signal must not slip past the notional cap: %+v", out)
	}
	if !strings.Contains(out.Reason, "notional") {
		t.Fatalf("reason = %q, want the uncheckable-exposure refusal", out.Reason)
	}

	if _, out = postA2ASignal(t, ts.URL, "a2a-secret",
		`{"id":"q2","agent":"research","symbol":"TSLA","side":"buy","notional":"99999","confidence":0.9}`); out.Accepted {
		t.Fatalf("notional above the cap must be refused: %+v", out)
	}
	if _, out = postA2ASignal(t, ts.URL, "a2a-secret",
		`{"id":"q3","agent":"research","symbol":"TSLA","side":"buy","notional":"900","confidence":0.9}`); !out.Accepted {
		t.Fatalf("notional under the cap must be accepted: %+v", out)
	}
}

// TestA2ASignalIdReuseWithAlteredPayload is the ingress-level proof that a
// spent id cannot be repointed at a different order.
func TestA2ASignalIdReuseWithAlteredPayload(t *testing.T) {
	s := newA2ATestServer(t, t.TempDir(), a2a.ModeOn)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	if _, out := postA2ASignal(t, ts.URL, "a2a-secret",
		`{"id":"dup1","agent":"research","symbol":"AAPL","side":"buy","notional":"100","confidence":0.8}`); !out.Accepted {
		t.Fatalf("the first signal must be accepted: %+v", out)
	}
	code, out := postA2ASignal(t, ts.URL, "a2a-secret",
		`{"id":"dup1","agent":"research","symbol":"NVDA","side":"buy","notional":"9000","confidence":0.8}`)
	if code != http.StatusOK {
		t.Fatalf("status %d want 200", code)
	}
	if out.Accepted {
		t.Fatalf("a reused id must never be accepted: %+v", out)
	}
	if out.Reason != "signal id reused with a different payload" {
		t.Fatalf("reason = %q, want the reuse refusal", out.Reason)
	}
	// The identical replay keeps its old reason.
	if _, out = postA2ASignal(t, ts.URL, "a2a-secret",
		`{"id":"dup1","agent":"research","symbol":"AAPL","side":"buy","notional":"100","confidence":0.8}`); out.Reason != "duplicate signal id" {
		t.Fatalf("reason = %q, want duplicate signal id", out.Reason)
	}
	st := acceptedCounts(t, s.bridge, "research")
	if st.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1 (only the first signal counts)", st.Accepted)
	}
	if st.Refused != 1 {
		t.Fatalf("refused = %d, want 1 (the reuse attempt is counted)", st.Refused)
	}
}
