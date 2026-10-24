package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/a2a"
	"github.com/BROCKUGANDA/alpacaruns/config"
)

// A2AEnabled reports whether the agent-to-agent bridge is configured.
type A2AEnabled struct {
	Enabled bool `json:"enabled"`
}

// A2ASignalRequest is the body of POST /api/a2a/signals.
type A2ASignalRequest struct {
	ID         string  `json:"id"`
	Agent      string  `json:"agent"`
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`
	Qty        string  `json:"qty,omitempty"`
	Notional   string  `json:"notional,omitempty"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason,omitempty"`
	ExpiresAt  string  `json:"expires_at,omitempty"`
}

// A2ASignalResponse is the answer to an inbound signal.
type A2ASignalResponse struct {
	Accepted bool   `json:"accepted"`
	Mode     string `json:"mode"`
	Reason   string `json:"reason"`
}

// A2AAgentsResponse lists the agents and their ramp positions.
type A2AAgentsResponse struct {
	Agents []a2a.AgentState `json:"agents"`
}

// A2ARampRequest is the body of POST /api/a2a/agents/{id}/ramp.
type A2ARampRequest struct {
	Mode string `json:"mode"`
	// Force bypasses the shadow-pass requirement. It exists for the
	// operator who explicitly wants to go live; the bot's own ticks
	// never set it.
	Force bool `json:"force,omitempty"`
}

// handleA2AEnabled reports whether the bridge accepts signals.
func (s *Server) handleA2AEnabled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	writeJSON(w, http.StatusOK, A2AEnabled{Enabled: s.a2aCfg.Enabled})
}

// handleA2ASignal is the bridge ingress: a peer agent posts a signal
// and gets back whether the bridge took it.
func (s *Server) handleA2ASignal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	if !s.a2aCfg.Enabled {
		writeError(w, http.StatusServiceUnavailable, "a2a_disabled", "the agent-to-agent bridge is disabled")
		return
	}
	// The bearer token is mandatory: an unauthenticated signal endpoint
	// is an open order pipeline. This first gate only asks "is this
	// caller a peer at all" — the group token or any peer's own token —
	// so an unknown token is refused before a single byte of the body is
	// parsed.
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "a2a signals require a bearer token")
		return
	}
	if !s.validA2AToken(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid a2a token")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", "could not read the signal body")
		return
	}
	var req A2ASignalRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "signal body is not valid JSON")
		return
	}
	// An agent id in the body is a CLAIM, not an identity. Now that the
	// claim is known, check it against the credential that is allowed to
	// make it: a peer with its own token may only be spoken for by that
	// token, so one shared bridge token can no longer post as an agent
	// ramped on.
	bearer, _ := bearerToken(r)
	if !s.validA2AIdentity(req.Agent, bearer) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "a2a token does not belong to this agent")
		return
	}
	sig := a2a.Signal{
		ID:         strings.TrimSpace(req.ID),
		Agent:      strings.TrimSpace(req.Agent),
		Symbol:     strings.TrimSpace(req.Symbol),
		Side:       strings.ToLower(strings.TrimSpace(req.Side)),
		Qty:        strings.TrimSpace(req.Qty),
		Notional:   strings.TrimSpace(req.Notional),
		Confidence: req.Confidence,
		Reason:     truncateText(req.Reason, 500),
	}
	if exp := strings.TrimSpace(req.ExpiresAt); exp != "" {
		if t, err := parseRFC3339(exp); err == nil {
			sig.ExpiresAt = t
		}
	}
	dec := s.bridge.Ingest(sig)
	writeJSON(w, http.StatusOK, A2ASignalResponse{
		Accepted: dec.Accepted, Mode: string(dec.Mode), Reason: dec.Reason,
	})
}

// handleA2AAgents lists the agents and their ramp positions.
func (s *Server) handleA2AAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	states, err := s.bridge.States()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "a2a_unavailable", err.Error())
		return
	}
	if states == nil {
		states = []a2a.AgentState{}
	}
	writeJSON(w, http.StatusOK, A2AAgentsResponse{Agents: states})
}

// handleA2ARamp moves an agent along the ramp: off -> shadow -> on.
func (s *Server) handleA2ARamp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	agent := strings.TrimSpace(r.PathValue("id"))
	if agent == "" {
		writeError(w, http.StatusBadRequest, "bad_param", "agent id is required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", "could not read the ramp body")
		return
	}
	var req A2ARampRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "ramp body is not valid JSON")
		return
	}
	mode := a2a.Mode(strings.ToLower(strings.TrimSpace(req.Mode)))
	state, err := s.bridge.Ramp(agent, mode, req.Force)
	if err != nil {
		writeError(w, http.StatusBadRequest, "ramp_rejected", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// truncateText shortens a string to at most n characters.
func truncateText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// a2aConfig builds the bridge configuration from the loaded config.
//
// Two fields are deliberately not read from config here:
//   - MaxPending: a2a.NewBridge supplies the default cap, so every bridge
//     construction path (this API and the bot's loop) gets the same queue
//     bound without each caller having to remember it.
//   - PeerTokens: per-peer credentials would need an env/config key, and
//     config parsing belongs to another workstream. Until that key
//     exists, PeerTokens stays nil and the bridge keeps today's
//     single-token behaviour. Set it here once the config carries it —
//     the enforcement in validA2AIdentity already works either way.
func a2aConfig(cfg *config.Config) a2a.Config {
	if cfg == nil {
		return a2a.Config{}
	}
	return a2a.Config{
		Enabled:           cfg.A2AEnabled,
		Token:             cfg.A2AToken,
		Agents:            cfg.A2AAgents,
		DefaultMode:       a2a.Mode(cfg.A2AMode),
		ShadowPasses:      cfg.A2AShadowPasses,
		MaxNotional:       cfg.A2AMaxNotional,
		MaxSignalsPerHour: 60,
		// Peer tokens bind a credential to one agent id, so a shared
		// group token cannot post as an agent ramped ON.
		PeerTokens: cfg.A2APeerTokens,
	}
}

// parseRFC3339 parses an RFC3339 timestamp.
func parseRFC3339(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, strings.TrimSpace(s))
}

// validA2AToken reports whether the request carries ANY credential the
// bridge accepts: the shared group token, or one peer's own token. It is
// the pre-body gate, so it deliberately does not decide which agent the
// caller may speak for — validA2AIdentity does that once the body is
// known.
func (s *Server) validA2AToken(r *http.Request) bool {
	bearer, ok := bearerToken(r)
	if !ok {
		return false
	}
	return s.a2aCfg.MatchAnyToken(bearer)
}

// validA2AIdentity binds the sender to the credential. The claimed agent
// is checked against the bridge's configuration (a2a.Config.Authenticate):
// an agent that has a peer token of its own may only be posted as with
// that token, and the shared group token is refused for it. When no
// per-peer tokens are configured the group token still carries every
// allow-listed agent, which is today's behaviour.
func (s *Server) validA2AIdentity(agent, bearer string) bool {
	return s.a2aCfg.Authenticate(agent, bearer)
}

// bearerToken pulls the bearer credential out of the Authorization
// header. ok is false when the header is missing, not a bearer scheme,
// or carries an empty token.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "bearer") {
		return "", false
	}
	got := strings.TrimSpace(parts[1])
	if got == "" {
		return "", false
	}
	return got, true
}
