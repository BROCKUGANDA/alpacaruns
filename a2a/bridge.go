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
	// hour. It is a rolling window, not a lifetime counter: the count is
	// taken from the timestamps kept in SignalWindow.
	MaxSignalsPerHour int
	// SignalWindow is how long an accepted signal keeps counting towards
	// MaxSignalsPerHour. Zero means one hour.
	SignalWindow time.Duration
	// MaxPending caps the queue of accepted-but-not-yet-evaluated
	// signals. Without a cap a peer that floods the ingress grows the
	// state file forever. Zero means the constructor's default.
	MaxPending int
	// SeenRetention is how long a signal id is remembered for dedupe.
	SeenRetention time.Duration
	// PeerTokens binds a credential to a single agent id: agent -> that
	// peer's own bearer token.
	//
	// The bridge token authenticates the SENDER, so an agent id asserted
	// in the body may not be the whole story: with one shared token any
	// holder could post as any allow-listed agent, including one ramped
	// on. When an agent has an entry here, only its own token may post as
	// it and the group Token is refused for that id. Agents with no entry
	// keep using Token, so a deployment that has only ever used one
	// shared token behaves exactly as before.
	PeerTokens map[string]string
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
	if cfg.SignalWindow <= 0 {
		cfg.SignalWindow = time.Hour
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 100
	}
	if cfg.SeenRetention <= 0 {
		cfg.SeenRetention = 24 * time.Hour
	}
	return &Bridge{cfg: cfg, store: store, now: func() time.Time { return time.Now().UTC() }}
}

// window returns the rolling window the per-agent quota counts inside.
func (b *Bridge) window() time.Duration {
	if b.cfg.SignalWindow > 0 {
		return b.cfg.SignalWindow
	}
	return time.Hour
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
	if !b.cfg.hasCredential() {
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
		n, known := parseNotional(sig)
		if !known {
			// The bridge has no market-data client and must not gain
			// one: sizing is the risk gate's job. So a qty-only signal
			// has no notional this layer can check, and an exposure it
			// cannot evaluate is an exposure it must not accept.
			reason := fmt.Sprintf("signal prices itself by qty only, so its notional cannot be checked against the bridge cap %.2f; send a notional", b.cfg.MaxNotional)
			b.countRefusal(sig.Agent, reason)
			return Decision{Accepted: false, Reason: reason}
		}
		if n > b.cfg.MaxNotional {
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
		// Idempotency: a replayed id is dropped, and an id reused with a
		// different payload is refused out loud.
		fp := Fingerprint(sig)
		if prev, seen := st.Seen[sig.ID]; seen {
			if prev.Fingerprint != "" && prev.Fingerprint != fp {
				reason := "signal id reused with a different payload"
				markRefusal(as, reason)
				decision = Decision{Accepted: false, Mode: as.Mode, Reason: reason}
				return nil
			}
			decision = Decision{Accepted: false, Mode: as.Mode, Reason: "duplicate signal id"}
			return nil
		}
		// Per-agent rate limit, counted inside a rolling window.
		if b.cfg.MaxSignalsPerHour > 0 {
			window := b.window()
			if hits := windowHits(st, sig.Agent, window, now); hits >= b.cfg.MaxSignalsPerHour {
				reason := fmt.Sprintf("agent has %d signals inside its rolling %s window (limit %d)",
					hits, window, b.cfg.MaxSignalsPerHour)
				markRefusal(as, reason)
				decision = Decision{Accepted: false, Mode: as.Mode, Reason: reason}
				return nil
			}
		}
		// Queue depth: the consumer must be able to keep up, and the
		// state file must not grow without bound. Nothing already
		// queued is dropped to make room — the newcomer is refused.
		if b.cfg.MaxPending > 0 && len(st.Pending) >= b.cfg.MaxPending {
			reason := fmt.Sprintf("the bridge queue is full (%d signals waiting, cap %d); nothing was queued",
				len(st.Pending), b.cfg.MaxPending)
			markRefusal(as, reason)
			decision = Decision{Accepted: false, Mode: as.Mode, Reason: reason}
			return nil
		}
		st.Seen[sig.ID] = SeenEntry{At: now, Fingerprint: fp}
		pruneSeen(st, now.Add(-b.cfg.SeenRetention))
		stampAccepted(st, sig.Agent, now)
		as.Accepted++
		as.LastReason = ""
		switch as.Mode {
		case ModeShadow:
			exp := expiryOf(sig, now)
			st.Pending = append(st.Pending, queued(sig, exp, now))
			decision = Decision{Accepted: true, Mode: as.Mode,
				Reason:    "accepted in shadow mode; no order will be placed",
				ExpiresAt: exp}
		case ModeOn:
			exp := expiryOf(sig, now)
			st.Pending = append(st.Pending, queued(sig, exp, now))
			decision = Decision{Accepted: true, Mode: as.Mode,
				Reason:    "accepted; the signal will be gated by the risk gate on the next tick",
				ExpiresAt: exp}
		default:
			markRefusal(as, "agent is ramped off")
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
// pending queue. The bridge's own clock is used so a test (or a replay)
// that moves time moves the stamps with it.
func queued(sig Signal, exp, now time.Time) Signal {
	sig.ReceivedAt = now
	sig.ExpiresAt = exp
	return sig
}

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
		markRefusal(agentState(st, agent), reason)
		return nil
	})
}

// markRefusal counts a refusal on an agent state the caller already
// holds. It is the same bookkeeping countRefusal does, for the paths
// that are already inside a store mutation — the refusal is recorded
// whether it is caught before or during the write.
func markRefusal(as *AgentState, reason string) {
	as.Refused++
	as.LastReason = reason
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

// Drain returns the pending signals that are still valid and drops the
// expired ones: a stale idea must never be traded because nobody was
// around to reject it.
//
// Note the current contract — a signal that is still valid is handed to
// the consumer AND left in the queue, so it is offered again on the next
// tick until it ages out. That is at-least-once delivery, and it is why
// MaxPending counts waiting signals as capacity the bridge has already
// spent. Changing it is a delivery-semantics decision outside the ingress
// hardening done here.
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

// Ack removes the named signals from the pending queue once the consumer
// has finished evaluating them.
//
// Drain is at-least-once: a valid signal stays queued until it expires, so
// without an ack a signal from an agent ramped ON is re-evaluated on EVERY
// tick for its whole lifetime — a 15-minute signal at a 5-minute poll is
// three entries, and the shadow streak advances three times for one idea.
// Acknowledging after evaluation is what turns "the bridge delivered this"
// into "the bot did something with this, once".
//
// A signal the consumer never reaches (crash before the ack) is delivered
// again, which is why the execution path keys its broker intent to the
// signal id: the retry is then refused by the broker as a duplicate rather
// than becoming a second order.
func (b *Bridge) Ack(ids ...string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = struct{}{}
		}
	}
	removed := 0
	err := b.store.Mutate(func(st *State) error {
		if len(st.Pending) == 0 || len(want) == 0 {
			return nil
		}
		keep := make([]Signal, 0, len(st.Pending))
		for _, sig := range st.Pending {
			if _, hit := want[strings.TrimSpace(sig.ID)]; hit {
				removed++
				continue
			}
			keep = append(keep, sig)
		}
		st.Pending = keep
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// parseNotional returns the signal's USD exposure and whether it is
// knowable here at all.
//
// A notional signal states its own exposure, so it is checked against
// the bridge cap. A qty-only signal is NOT quietly read as zero: the
// bridge has no market-data client (and must not gain one — sizing and
// pricing are the risk gate's job), so its exposure cannot be computed
// and known comes back false. Ingest refuses such a signal whenever a
// cap is configured, rather than wave an unmeasurable exposure through.
func parseNotional(sig Signal) (float64, bool) {
	if n := strings.TrimSpace(sig.Notional); n != "" {
		if f, err := parseFloat(n); err == nil {
			return f, true
		}
	}
	return 0, false
}
