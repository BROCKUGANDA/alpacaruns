package a2a

import (
	"path/filepath"
	"testing"
)

// Config with one peer that has its own credential and one that does not.
func peerBoundConfig() Config {
	return Config{
		Enabled:     true,
		Token:       "group-token",
		Agents:      []string{"research", "momentum"},
		DefaultMode: ModeOn,
		PeerTokens:  map[string]string{"research": "research-token"},
	}
}

// TestAuthenticatePeerTokenIsTheOnlyWayInForItsAgent is the impersonation
// fix at the credential layer.
func TestAuthenticatePeerTokenIsTheOnlyWayInForItsAgent(t *testing.T) {
	cfg := peerBoundConfig()
	cases := []struct {
		name   string
		agent  string
		bearer string
		want   bool
	}{
		{"peer's own token", "research", "research-token", true},
		{"group token may not impersonate a bound agent", "research", "group-token", false},
		{"another peer's token", "research", "someone-elses-token", false},
		{"wrong token", "research", "research-tokes", false},
		{"empty bearer", "research", "", false},
		{"bound agent, trimmed id and bearer", "  research  ", " research-token ", true},
		{"agent id is matched case-insensitively", "RESEARCH", "research-token", true},
		{"unbound agent keeps the group token", "momentum", "group-token", true},
		{"unbound agent rejects a peer token", "momentum", "research-token", false},
		{"unlisted agent is not this layer's business", "stranger", "group-token", true},
		{"no credential at all", "research", "   ", false},
	}
	for _, tc := range cases {
		if got := cfg.Authenticate(tc.agent, tc.bearer); got != tc.want {
			t.Errorf("%s: Authenticate(%q, %q) = %v, want %v", tc.name, tc.agent, tc.bearer, got, tc.want)
		}
	}
}

// TestAuthenticateWithoutPeerTokensIsTodaysBehaviour proves the existing
// deployment (one shared bridge token, no per-peer entries) is unchanged.
func TestAuthenticateWithoutPeerTokensIsTodaysBehaviour(t *testing.T) {
	cfg := Config{Token: "group-token", Agents: []string{"research"}}
	if !cfg.Authenticate("research", "group-token") {
		t.Fatal("the group token must still authenticate every allow-listed agent")
	}
	if !cfg.Authenticate("any-agent-at-all", "group-token") {
		t.Fatal("with no peer tokens configured nothing may change about authentication")
	}
	if cfg.Authenticate("research", "nope") {
		t.Fatal("a wrong token must never authenticate")
	}
	if (Config{}).Authenticate("research", "") {
		t.Fatal("a bridge with no credential authenticates nothing")
	}
}

// TestBlankPeerEntryDoesNotLockTheAgentOut: an empty per-peer token is not
// a credential. It must not become a secret nobody can match, and it must
// not silently grant the group token to an agent that was meant to be
// bound — that is why the entry is reported as unbound rather than half-
// configured.
func TestBlankPeerEntryDoesNotLockTheAgentOut(t *testing.T) {
	cfg := Config{Token: "group-token", PeerTokens: map[string]string{"research": "   "}}
	if !cfg.Authenticate("research", "group-token") {
		t.Fatal("a blank peer entry must fall back to the group token")
	}
	if cfg.Authenticate("research", "") {
		t.Fatal("an empty bearer never authenticates")
	}
}

// TestMatchAnyToken covers the pre-body gate: it decides "is this caller
// a peer at all", so it must accept either credential class and reject
// anything else.
func TestMatchAnyToken(t *testing.T) {
	cfg := peerBoundConfig()
	for bearer, want := range map[string]bool{
		"group-token":      true,
		"research-token":   true,
		"research-token ":  true,
		"wrong":            false,
		"":                 false,
		"group-tokenx":     false,
		" xresearch-token": false,
	} {
		if got := cfg.MatchAnyToken(bearer); got != want {
			t.Errorf("MatchAnyToken(%q) = %v, want %v", bearer, got, want)
		}
	}
	// A bridge whose only credentials are peer tokens still recognises
	// them, and nothing else.
	onlyPeers := Config{PeerTokens: map[string]string{"research": "research-token"}}
	if !onlyPeers.MatchAnyToken("research-token") {
		t.Fatal("peer tokens alone are credentials")
	}
	if onlyPeers.MatchAnyToken("group-token") {
		t.Fatal("there is no group token to match")
	}
	if (Config{}).MatchAnyToken("anything") {
		t.Fatal("a bridge with no credentials accepts nothing")
	}
}

// TestBridgeMatchTokenReadsTheBridgesOwnConfig keeps the HTTP layer and
// the ingress answering from one configuration.
func TestBridgeMatchTokenReadsTheBridgesOwnConfig(t *testing.T) {
	dir := t.TempDir()
	b := NewBridge(peerBoundConfig(), NewStore(filepath.Join(dir, "trades.jsonl")))
	if !b.MatchToken("research", "research-token") {
		t.Fatal("the peer's own token must authenticate it")
	}
	if b.MatchToken("research", "group-token") {
		t.Fatal("the group token must not impersonate a bound agent")
	}
	if !b.MatchToken("momentum", "group-token") {
		t.Fatal("an unbound agent keeps using the group token")
	}
}

// TestIngestRunsOnPeerTokensAlone: hasCredential must count per-peer
// tokens as credentials, otherwise a peer-token-only bridge would refuse
// every signal with "no token configured". (Who may speak for which
// agent is settled at the HTTP ingress; the bridge's own gate is that it
// has a credential at all.)
func TestIngestRunsOnPeerTokensAlone(t *testing.T) {
	cfg := peerBoundConfig()
	cfg.Token = ""
	b := testBridge(t, cfg)
	dec := b.Ingest(sampleSignal())
	if dec.Reason == "a2a bridge has no token configured; refusing every signal" {
		t.Fatalf("peer tokens are credentials too: %+v", dec)
	}
	if !dec.Accepted {
		t.Fatalf("an allow-listed agent must still be ingested when only peer tokens are set: %+v", dec)
	}
}

// TestIngestRefusesWhenNoCredentialExistsAtAll keeps the fail-safe
// default: no group token and no peer tokens means nothing is accepted.
func TestIngestRefusesWhenNoCredentialExistsAtAll(t *testing.T) {
	b := testBridge(t, Config{Enabled: true, Agents: []string{"research"}, DefaultMode: ModeOn})
	if dec := b.Ingest(sampleSignal()); dec.Accepted {
		t.Fatal("a bridge with no credential must refuse every signal")
	}
}
