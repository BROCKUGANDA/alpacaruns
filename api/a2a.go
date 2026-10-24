package api

import (
	"crypto/subtle"
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
	// is an open order pipeline.
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
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
func a2aConfig(cfg *config.Config) a2a.Config {
	if cfg == nil {
		return a2a.Config{}
	}
	return a2a.Config{
		Enabled:         cfg.A2AEnabled,
		Token:           cfg.A2AToken,
		Agents:          cfg.A2AAgents,
		DefaultMode:     a2a.Mode(cfg.A2AMode),
		ShadowPasses:    cfg.A2AShadowPasses,
		MaxNotional:     cfg.A2AMaxNotional,
		MaxSignalsPerHour: 60,
	}
}

// parseRFC3339 parses an RFC3339 timestamp.
func parseRFC3339(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, strings.TrimSpace(s))
}

// validA2AToken compares the request's bearer token against the
// bridge's own token. The bridge has its own credential: peer agents
// must not need the dashboard's operator token, and the dashboard's
// token must not be handed to another system.
func (s *Server) validA2AToken(r *http.Request) bool {
	want := strings.TrimSpace(s.a2aCfg.Token)
	if want == "" {
		return false
	}
	h := r.Header.Get("Authorization")
	if h == "" {
		return false
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "bearer") {
		return false
	}
	got := strings.TrimSpace(parts[1])
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
