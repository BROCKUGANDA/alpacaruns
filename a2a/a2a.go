// Package a2a is the agent-to-agent bridge: a controlled ingress for
// signals produced by OTHER agents (peer processes, other models, a
// research pipeline) into this bot's execution path.
//
// The bridge exists because an inbound signal is untrusted input. It
// must not be able to place an order by itself, and it must not be
// able to place orders at all until an operator has ramped it on. The
// ramp is the mechanism:
//
//	off     signals are refused; nothing is queued, nothing is traded
//	shadow  signals are accepted, scored and journaled, but NO order
//	        is ever placed — the agent's proposals are compared against
//	        what the bot would have done
//	on      signals flow through the deterministic risk gate and the
//	        normal executor, exactly like the bot's own decisions
//
// Ramping up is earned, not switched: an agent only reaches "on" after
// it has produced ShadowPasses consecutive signals that the risk gate
// approved while in shadow. Ramping down is immediate and always
// available — that is the point of an off ramp.
package a2a

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mode is an agent's ramp position.
type Mode string

const (
	// ModeOff refuses every inbound signal.
	ModeOff Mode = "off"
	// ModeShadow accepts and evaluates signals without trading.
	ModeShadow Mode = "shadow"
	// ModeOn lets signals reach the risk gate and the executor.
	ModeOn Mode = "on"
)

// ValidMode reports whether m is a known ramp position.
func ValidMode(m Mode) bool {
	switch m {
	case ModeOff, ModeShadow, ModeOn:
		return true
	}
	return false
}

// Signal is one inbound proposal from a peer agent.
type Signal struct {
	// ID is the agent's idempotency key. A repeated ID is ignored.
	ID string `json:"id"`
	// Agent identifies the sending agent. It must be allow-listed.
	Agent string `json:"agent"`
	// Symbol is the venue symbol to trade.
	Symbol string `json:"symbol"`
	// Side is "buy" or "sell".
	Side string `json:"side"`
	// Qty is a share/coin count; Notional is a USD amount. Either one
	// must be set.
	Qty string `json:"qty,omitempty"`
	// Notional is the USD exposure the agent wants.
	Notional string `json:"notional,omitempty"`
	// Confidence is the agent's own confidence in the idea (0..1).
	Confidence float64 `json:"confidence"`
	// Reason is the agent's rationale (free text, truncated on ingest).
	Reason string `json:"reason,omitempty"`
	// ExpiresAt bounds how long the signal stays valid.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// ReceivedAt is stamped by the bridge on ingest.
	ReceivedAt time.Time `json:"received_at"`
}

// Validate checks the shape of an inbound signal.
func (s Signal) Validate(now time.Time) error {
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("signal id is required")
	}
	if strings.TrimSpace(s.Agent) == "" {
		return fmt.Errorf("signal agent is required")
	}
	if strings.TrimSpace(s.Symbol) == "" {
		return fmt.Errorf("signal symbol is required")
	}
	switch strings.ToLower(strings.TrimSpace(s.Side)) {
	case "buy", "sell":
	default:
		return fmt.Errorf("signal side must be buy or sell, got %q", s.Side)
	}
	if strings.TrimSpace(s.Qty) == "" && strings.TrimSpace(s.Notional) == "" {
		return fmt.Errorf("signal needs qty or notional")
	}
	if s.Confidence < 0 || s.Confidence > 1 {
		return fmt.Errorf("signal confidence must be in [0,1], got %g", s.Confidence)
	}
	if !s.ExpiresAt.IsZero() && !s.ExpiresAt.After(now) {
		return fmt.Errorf("signal expired at %s", s.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// AgentState is one agent's ramp position and counters.
type AgentState struct {
	// Agent is the agent id.
	Agent string `json:"agent"`
	// Mode is the current ramp position.
	Mode Mode `json:"mode"`
	// ShadowPasses counts consecutive shadow evaluations the risk gate
	// approved. It is what earns the right to ramp up.
	ShadowPasses int `json:"shadow_passes"`
	// ShadowFails counts rejected shadow evaluations.
	ShadowFails int `json:"shadow_fails"`
	// Accepted counts signals that passed the ingress checks.
	Accepted int `json:"accepted"`
	// Refused counts signals rejected at the ramp or by the allowlist.
	Refused int `json:"refused"`
	// Traded counts signals that reached the executor.
	Traded int `json:"traded"`
	// LastSignalAt is when the last signal was seen.
	LastSignalAt time.Time `json:"last_signal_at,omitempty"`
	// LastReason explains the most recent refusal or rejection.
	LastReason string `json:"last_reason,omitempty"`
}

// SeenEntry is one signal id's dedupe record.
//
// Stamping the payload alongside the id is what makes dedupe mean
// something: a peer that replays a used id with a DIFFERENT symbol,
// side or size is not sending the same signal twice, and dropping that
// silently hides either an authoring bug or an attack. Fingerprint is
// the hash of the fields that matter (see Fingerprint) — never the
// payload itself, so the state file holds nothing tradeable.
//
// A record whose Fingerprint is empty predates fingerprinting (or came
// from a state file written by an older binary): the bridge cannot tell
// whether the payload changed, so it keeps the old behaviour and treats
// the id as simply used.
type SeenEntry struct {
	// At is when the id was first accepted; the retention window prunes
	// on it.
	At time.Time `json:"at"`
	// Fingerprint identifies the payload that arrived with this id.
	Fingerprint string `json:"fp,omitempty"`
}

// UnmarshalJSON accepts both the current object form and the legacy
// form, where a seen id mapped straight to a timestamp. Without it an
// existing state file would fail to parse and the bridge would refuse
// everything after an upgrade.
func (e *SeenEntry) UnmarshalJSON(data []byte) error {
	var when time.Time
	if err := json.Unmarshal(data, &when); err == nil {
		e.At = when
		e.Fingerprint = ""
		return nil
	}
	type plain SeenEntry
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("seen entry: %w", err)
	}
	*e = SeenEntry(v)
	return nil
}

// State is the persisted bridge state.
type State struct {
	Version int                    `json:"version"`
	Agents  map[string]*AgentState `json:"agents"`
	// Seen maps a signal id to its dedupe record, so a replayed signal is
	// dropped instead of traded twice, and an id reused for a different
	// payload is refused instead of quietly swallowed.
	Seen map[string]SeenEntry `json:"seen"`
	// Windows holds the accepted-signal timestamps inside the rolling
	// rate-limit window, per agent. It is the quota's memory: the count
	// comes from those timestamps, not from a counter that only climbs.
	Windows map[string][]time.Time `json:"windows,omitempty"`
	// Pending holds the accepted-but-not-yet-evaluated signals. The bot
	// drains it once per tick; the dashboard API only reads it.
	Pending []Signal `json:"pending,omitempty"`
}

// Store persists the bridge state to disk. The bot and the dashboard
// API run as separate processes, so the ramp position has to live in a
// file both can read.
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore derives the state path from the trade-log path so both live
// together (data/trades.jsonl -> data/a2a-state.json).
func NewStore(tradeLogPath string) *Store {
	dir := filepath.Dir(tradeLogPath)
	return &Store{path: filepath.Join(dir, "a2a-state.json")}
}

// Path returns the backing file path.
func (s *Store) Path() string { return s.path }

// Load reads the state; a missing file yields an empty state.
func (s *Store) Load() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (*State, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{Version: 1, Agents: map[string]*AgentState{}, Seen: map[string]SeenEntry{}}, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if st.Agents == nil {
		st.Agents = map[string]*AgentState{}
	}
	if st.Seen == nil {
		st.Seen = map[string]SeenEntry{}
	}
	return &st, nil
}

// Save atomically writes the state file.
func (s *Store) Save(st *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(st)
}

func (s *Store) saveLocked(st *State) error {
	st.Version = 1
	if st.Agents == nil {
		st.Agents = map[string]*AgentState{}
	}
	if st.Seen == nil {
		st.Seen = map[string]SeenEntry{}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Mutate applies fn to the persisted state under the store lock and
// writes the result back. It is the only way to change the ramp
// position, so a concurrent reader never sees a half-applied change.
func (s *Store) Mutate(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return s.saveLocked(st)
}

// AgentStates returns a snapshot of every known agent, sorted by id.
func (s *Store) AgentStates() ([]AgentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]AgentState, 0, len(st.Agents))
	for _, a := range st.Agents {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out, nil
}

// agentState returns (creating if needed) the state for one agent. A
// new agent starts with an empty mode; the bridge fills in the default
// ramp position on first sight.
func agentState(st *State, id string) *AgentState {
	if a, ok := st.Agents[id]; ok {
		return a
	}
	a := &AgentState{Agent: id}
	st.Agents[id] = a
	return a
}

// pruneSeen drops signal ids older than the retention window so the
// dedupe map cannot grow without bound.
func pruneSeen(st *State, olderThan time.Time) {
	for id, entry := range st.Seen {
		if entry.At.Before(olderThan) {
			delete(st.Seen, id)
		}
	}
}

// windowHits returns how many of an agent's accepted signals still fall
// inside the rolling window, pruning the stamps that have aged out. The
// count is read off the stamps rather than a counter that only ever goes
// up, which is what makes the limit a window instead of a lifetime cap.
func windowHits(st *State, agent string, window time.Duration, now time.Time) int {
	hits := st.Windows[agent]
	if len(hits) == 0 {
		return 0
	}
	keep := hits[:0:0]
	for _, at := range hits {
		if now.Sub(at) < window {
			keep = append(keep, at)
		}
	}
	if st.Windows == nil {
		st.Windows = map[string][]time.Time{}
	}
	if len(keep) == 0 {
		delete(st.Windows, agent)
	} else {
		st.Windows[agent] = keep
	}
	return len(keep)
}

// stampAccepted records that an agent had a signal accepted at now, so
// the rolling window can count it.
func stampAccepted(st *State, agent string, now time.Time) {
	if st.Windows == nil {
		st.Windows = map[string][]time.Time{}
	}
	st.Windows[agent] = append(st.Windows[agent], now)
}
