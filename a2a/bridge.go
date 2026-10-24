package a2a

import (
	"fmt"
	"strings"
	"time"
)

// Config is the bridge's operating configuration.
type Config struct {
	// Enabled turns the ingress on. Off by default: an open signal
	// endpoint is an attack surface, so it is opt-in.
	Enabled bool
	// Token authenticates inbound signals (bearer). Empty means every
	// inbound signal is refused — there is no anonymous mode.
	Token string
	// Agents is the allow-list of agent ids. Empty means no agent may
	// post.
	Agents []string
	// DefaultMode is the ramp position a newly seen agent starts in.
	DefaultMode Mode
	// ShadowPasses is how many consecutive approved shadow evaluations
	// an agent needs before it may be ramped on.
	ShadowPasses int
	// MaxNotional is the per-signal USD cap.
	MaxNotional float64
	// MaxSignalsPerHour caps how many signals one agent may post per
	// hour.
	MaxSignalsPerHour int
	// SeenRetention is how long a signal id is remembered for dedupe.
	SeenRetention time.Duration
}

// Bridge accepts inbound signals, applies the ramp, and hands the
// accepted ones to the consumer.
type Bridge struct {
	cfg   Config
	store *Store
	now   func() time.Time
}

// NewBridge builds a bridge over a store.
func NewBridge(cfg Config, store *Store) *Bridge {
	if cfg.DefaultMode == "" {
		cfg.DefaultMode = ModeOff
	}
	if cfg.ShadowPasses <= 0 {
		cfg.ShadowPasses = 3
	}
	if cfg.MaxNotional <= 0 {
		cfg.MaxNotional = 10000
	}
	if cfg.MaxSignalsPerHour <= 0 {
		cfg.MaxSignalsPerHour = 60
	}
	if cfg.SeenRetention <= 0 {
		cfg.SeenRetention = 24 * time.Hour
	}
	return &Bridge{cfg: cfg, store: store, now: func() time.Time { return time.Now().UTC() }}
}

// Decision is the outcome of offering a signal to the bridge.
type Decision struct {
	// Accepted reports whether the signal was queued for the consumer.
	Accepted bool `json:"accepted"`
	// Mode is the agent's ramp position at the time of the decision.
	Mode Mode `json:"mode"`
	// Reason explains the outcome.
	Reason string `json:"reason"`
	// ShadowPasses is the agent's shadow progress (shadow mode only).
	ShadowPasses int `json:"shadow_passes,omitempty"`
	// ExpiresAt is when the queued signal stops being valid.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// Ingest validates a signal and applies the ramp. It never places an
// order: the accepted signal is queued for the bot's next tick, where
// it goes through the same risk gate as the bot's own decisions.
func (b *Bridge) Ingest(sig Signal) Decision {
	now := b.now()
	if !b.cfg.Enabled {
		return Decision{Accepted: false, Reason: "a2a bridge is disabled"}
	}
	if strings.TrimSpace(b.cfg.Token) == "" {
		return Decision{Accepted: false, Reason: "a2a bridge has no token configured; refusing every signal"}
	}
	if err := sig.Validate(now); err != nil {
		b.countRefusal(sig.Agent, err.Error())
		return Decision{Accepted: false, Reason: err.Error()}
	}
	if !b.allowed(sig.Agent) {
		b.countRefusal(sig.Agent, "agent is not allow-listed")
		return Decision{Accepted: false, Reason: "agent is not allow-listed"}
	}
	if b.cfg.MaxNotional > 0 {
		if n := parseNotional(sig); n > b.cfg.MaxNotional {
			reason := fmt.Sprintf("signal notional %.2f exceeds the bridge cap %.2f", n, b.cfg.MaxNotional)
			b.countRefusal(sig.Agent, reason)
			return Decision{Accepted: false, Reason: reason}
		}
	}

	var decision Decision
	err := b.store.Mutate(func(st *State) error {
		as := agentState(st, sig.Agent)
		as.LastSignalAt = now
		if as.Mode == "" {
			as.Mode = b.cfg.DefaultMode
		}
		if as.Mode == "" {
			as.Mode = ModeOff
		}
		// Idempotency: a replayed signal id is dropped.
		if _, seen := st.Seen[sig.ID]; seen {
			decision = Decision{Accepted: false, Mode: as.Mode, Reason: "duplicate signal id"}
			return nil
		}
		// Per-agent rate limit.
		if b.cfg.MaxSignalsPerHour > 0 && as.Accepted >= b.cfg.MaxSignalsPerHour {
			decision = Decision{Accepted: false, Mode: as.Mode,
				Reason: fmt.Sprintf("agent reached its hourly limit of %d signals", b.cfg.MaxSignalsPerHour)}
			return nil
		}
		st.Seen[sig.ID] = now
		pruneSeen(st, now.Add(-b.cfg.SeenRetention))
		as.Accepted++
		as.LastReason = ""
		switch as.Mode {
		case ModeShadow:
			exp := expiryOf(sig, now)
			st.Pending = append(st.Pending, queued(sig, exp))
			decision = Decision{Accepted: true, Mode: as.Mode,
				Reason:   "accepted in shadow mode; no order will be placed",
				ExpiresAt: exp}
		case ModeOn:
			exp := expiryOf(sig, now)
			st.Pending = append(st.Pending, queued(sig, exp))
			decision = Decision{Accepted: true, Mode: as.Mode,
				Reason:   "accepted; the signal will be gated by the risk gate on the next tick",
				ExpiresAt: exp}
		default:
			as.Refused++
			as.LastReason = "agent is ramped off"
			decision = Decision{Accepted: false, Mode: as.Mode, Reason: "agent is ramped off"}
		}
		return nil
	})
	if err != nil {
		return Decision{Accepted: false, Reason: "bridge state unavailable: " + err.Error()}
	}
	return decision
}

// queued stamps a signal with its ingest time and expiry for the
// pending queue.
func queued(sig Signal, exp time.Time) Signal {
	sig.ReceivedAt = b_now()
	sig.ExpiresAt = exp
	return sig
}

// b_now is the clock used when stamping queued signals.
func b_now() time.Time { return time.Now().UTC() }

// expiryOf returns the signal's expiry, defaulting to a short window so
// a stale idea cannot be traded hours later.
func expiryOf(sig Signal, now time.Time) time.Time {
	if !sig.ExpiresAt.IsZero() {
		return sig.ExpiresAt
	}
	return now.Add(15 * time.Minute)
}

// allowed reports whether the agent is on the allow-list.
func (b *Bridge) allowed(agent string) bool {
	want := strings.ToLower(strings.TrimSpace(agent))
	for _, a := range b.cfg.Agents {
		if strings.ToLower(strings.TrimSpace(a)) == want {
			return true
		}
	}
	return false
}

// countRefusal records a refusal against the agent's counters.
func (b *Bridge) countRefusal(agent, reason string) {
	if strings.TrimSpace(agent) == "" {
		return
	}
	_ = b.store.Mutate(func(st *State) error {
		as := agentState(st, agent)
		as.Refused++
		as.LastReason = reason
		return nil
	})
}

// RecordOutcome folds the result of evaluating a signal back into the
// agent's counters. In shadow mode an approved evaluation advances the
// shadow streak (which is what earns the right to ramp on); a rejected
// one resets it.
func (b *Bridge) RecordOutcome(agent string, approved bool, reason string) {
	_ = b.store.Mutate(func(st *State) error {
		as := agentState(st, agent)
		as.LastReason = reason
		mode := as.Mode
		if mode == "" {
			mode = b.cfg.DefaultMode
		}
		switch {
		case approved && mode == ModeShadow:
			as.ShadowPasses++
		case !approved && mode == ModeShadow:
			as.ShadowFails++
			as.ShadowPasses = 0
		case approved && mode == ModeOn:
			as.Traded++
		}
		return nil
	})
}

// Ramp moves an agent to a new ramp position. Ramping ON requires the
// agent to have earned it: ShadowPasses consecutive approved shadow
// evaluations. Ramping OFF or back to SHADOW is always allowed
// immediately — that is the off ramp, and it must never be gated.
func (b *Bridge) Ramp(agent string, mode Mode, force bool) (AgentState, error) {
	if !ValidMode(mode) {
		return AgentState{}, fmt.Errorf("unknown ramp mode %q", mode)
	}
	var out AgentState
	err := b.store.Mutate(func(st *State) error {
		as := agentState(st, agent)
		if as.Mode == "" {
			as.Mode = b.cfg.DefaultMode
		}
		switch mode {
		case ModeOn:
			if !force {
				if as.Mode != ModeShadow {
					return fmt.Errorf("agent %q must be in shadow before it can be ramped on", agent)
				}
				if as.ShadowPasses < b.cfg.ShadowPasses {
					return fmt.Errorf("agent %q has %d/%d shadow passes; it cannot be ramped on yet",
						agent, as.ShadowPasses, b.cfg.ShadowPasses)
				}
			}
			as.ShadowPasses = 0
		case ModeShadow:
			as.ShadowPasses = 0
		case ModeOff:
			as.ShadowPasses = 0
		}
		as.Mode = mode
		out = *as
		return nil
	})
	return out, err
}

// States returns every known agent's ramp state.
func (b *Bridge) States() ([]AgentState, error) { return b.store.AgentStates() }

// ModeOf returns an agent's current ramp position. An unknown agent is
// reported as off: an agent that has never been configured may not
// trade.
func (b *Bridge) ModeOf(agent string) (Mode, error) {
	states, err := b.store.AgentStates()
	if err != nil {
		return ModeOff, err
	}
	for _, a := range states {
		if strings.EqualFold(a.Agent, strings.TrimSpace(agent)) {
			if a.Mode == "" {
				return ModeOff, nil
			}
			return a.Mode, nil
		}
	}
	return ModeOff, nil
}

// Drain removes the pending signals and returns the ones that are still
// valid. Expired signals are dropped silently: a stale idea must never
// be traded because nobody was around to reject it.
func (b *Bridge) Drain(now time.Time) ([]Signal, error) {
	var out []Signal
	err := b.store.Mutate(func(st *State) error {
		if len(st.Pending) == 0 {
			return nil
		}
		keep := st.Pending[:0:0]
		for _, sig := range st.Pending {
			if !sig.ExpiresAt.IsZero() && !sig.ExpiresAt.After(now) {
				continue
			}
			keep = append(keep, sig)
		}
		out = append(out, keep...)
		st.Pending = keep
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseNotional estimates the signal's USD exposure. A notional
// signal uses its own value; a qty signal is estimated with the
// configured per-unit budget ceiling, so a qty-only signal can never
// silently exceed the cap.
func parseNotional(sig Signal) float64 {
	if n := strings.TrimSpace(sig.Notional); n != "" {
		if f, err := parseFloat(n); err == nil {
			return f
		}
	}
	return 0
}
