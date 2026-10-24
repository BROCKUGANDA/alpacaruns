package api

// security_origin_test.go pins the same-origin decision and the venue
// proof. Both were fail-open in ways no other test covered.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A caller could previously make any Origin look same-origin by sending a
// matching X-Forwarded-Host. That header is not forbidden to scripts, so the
// check compared the caller's claim against the caller's other claim.
func TestSameOriginIgnoresForwardedHost(t *testing.T) {
	s := newTestServer(t, t.TempDir())
	s.settings.TrustedProxy = true
	s.settings.CORSOrigin = "https://run.example.com"

	req := httptest.NewRequest(http.MethodPost, "http://api:8090/api/control/pause", nil)
	req.Host = "api:8090"
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	if s.sameOrigin(req) {
		t.Fatal("a spoofed X-Forwarded-Host made an attacker's Origin same-origin")
	}
}

func TestSameOriginAcceptsRealRequestHost(t *testing.T) {
	s := newTestServer(t, t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "http://dash.local/api/control/pause", nil)
	req.Host = "dash.local"
	req.Header.Set("Origin", "https://dash.local")
	if !s.sameOrigin(req) {
		t.Fatal("same host was rejected")
	}
	// Port differences are not a different origin.
	req.Header.Set("Origin", "http://dash.local:9999")
	if !s.sameOrigin(req) {
		t.Fatal("port difference rejected")
	}
}

// Behind a proxy that rewrites Host, the public host is the configured CORS
// origin — server state the caller cannot forge.
func TestSameOriginUsesConfiguredOriginBehindProxy(t *testing.T) {
	s := newTestServer(t, t.TempDir())
	s.settings.TrustedProxy = true
	s.settings.CORSOrigin = "https://run.example.com"

	req := httptest.NewRequest(http.MethodPost, "http://api:8090/x", nil)
	req.Host = "api:8090" // the proxy rewrote it
	req.Header.Set("Origin", "https://run.example.com")
	if !s.sameOrigin(req) {
		t.Fatal("the configured public origin was rejected behind the proxy")
	}

	// The same request without TrustedProxy must NOT be accepted: the host
	// genuinely differs and nothing vouches for the proxy.
	s.settings.TrustedProxy = false
	if s.sameOrigin(req) {
		t.Fatal("public origin accepted without a trusted proxy")
	}
	// A wildcard CORS origin is not an authority.
	s.settings.TrustedProxy = true
	s.settings.CORSOrigin = "*"
	if s.sameOrigin(req) {
		t.Fatal("a wildcard CORS origin authorised a same-origin decision")
	}
}

// With no risk config loaded, the manual-order endpoint used to return
// Approved from a gate that examined nothing.
func TestManualExecuteRefusesWithoutRiskConfig(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)
	// Deliberately no cfg/strat: the "cold start" the old code approved in.
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	body := `{"symbol":"AAPL","side":"buy","qty":"1"}`
	resp, err := http.Post(ts.URL+"/api/trade/execute", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Approved bool     `json:"approved"`
		Reasons  []string `json:"reasons"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Approved {
		t.Fatal("an order was approved by a validator that was never wired")
	}
	if len(out.Reasons) == 0 {
		t.Fatal("the refusal carried no reason")
	}
}
