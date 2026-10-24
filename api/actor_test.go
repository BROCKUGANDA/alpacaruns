package api

// actor_test.go: named operator tokens give attribution and per-name
// revocation without rotating everyone's credential.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/config"
)

func actorServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t, t.TempDir())
	s.settings.AuthToken = "shared"
	s.cfg = &config.Config{DashboardTokens: map[string]string{
		"alice": "alice-token",
		"bob":   "bob-token",
	}}
	return s
}

func withBearer(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://dash.local/api/control/pause", nil)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

func TestActorForNamedTokens(t *testing.T) {
	s := actorServer(t)
	for _, tc := range []struct{ tok, want string }{
		{"alice-token", "alice"},
		{"bob-token", "bob"},
		{"shared", "operator"}, // the unnamed fallback still works
		{"nonsense", ""},
		{"", ""},
	} {
		if got := s.actorFor(withBearer(tc.tok)); got != tc.want {
			t.Errorf("token %q resolved to %q, want %q", tc.tok, got, tc.want)
		}
	}
}

// Revoking one operator is deleting one entry; the others keep working.
func TestRevokingOneActorDoesNotAffectOthers(t *testing.T) {
	s := actorServer(t)
	delete(s.cfg.DashboardTokens, "bob")
	if s.actorFor(withBearer("bob-token")) != "" {
		t.Fatal("a revoked operator token still authenticates")
	}
	if s.actorFor(withBearer("alice-token")) != "alice" {
		t.Fatal("revoking bob broke alice")
	}
	if !s.validBearer(withBearer("alice-token")) {
		t.Fatal("named token not accepted by the control guard")
	}
}

// A same-origin browser session is a different fact from a named operator,
// and must be reported as such rather than guessed at.
func TestActingActorDistinguishesSameOrigin(t *testing.T) {
	s := actorServer(t)
	r := httptest.NewRequest(http.MethodPost, "http://dash.local/api/control/pause", nil)
	r.Host = "dash.local"
	r.Header.Set("Origin", "https://dash.local")
	if got := s.actingActor(r); got != "same-origin" {
		t.Fatalf("actingActor = %q, want same-origin", got)
	}
	if got := s.actingActor(withBearer("alice-token")); got != "alice" {
		t.Fatalf("actingActor = %q, want alice", got)
	}
}
