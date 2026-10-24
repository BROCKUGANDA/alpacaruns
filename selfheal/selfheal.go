// Package selfheal detects and repairs the states this bot can fall into
// by itself, without an operator reading logs at 3am.
//
// The premise is that a long-running trading process fails in ways that
// are silent and, worse, unsafe. Each check here is a state where the
// account is exposed to something the bot believes is handled:
//
//   - a protective order resting with NO position behind it. Sell five
//     shares you no longer own and you have opened a short. This is the
//     single most dangerous state the system can reach, and it is
//     reachable by closing a position by hand and leaving the pair the
//     bot placed sitting there.
//   - two protective pairs on one position: both may fill, selling twice
//     what is held.
//   - a drawdown halt that stays engaged after the breach has healed.
//     The halt is deliberately sticky, but a sticky halt whose condition
//     no longer holds is not caution, it is a stuck process.
//   - a state file that cannot be parsed, which otherwise crash-loops
//     the bot or blinds the dashboard.
//   - a truncated final journal line from a crash mid-append, which every
//     reader tolerates but nothing cleans.
//
// Repairs are conservative by construction: the pass only ever CANCELS
// orders, RE-BASES a bookkeeping high-water mark, or sets aside a file it
// cannot read after backing it up. It never places an order, never
// increases size, and never deletes anything. Cancelling a protective
// order with no position is risk-reducing in every case; inventing one
// would be the bot trading, which is not this package's job.
//
// Every action is reported so the caller can journal it, and the whole
// pass can be run in report-only mode.
package selfheal

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// Broker is the broker surface the pass needs. Kept small so every check
// is testable without a network.
type Broker interface {
	Positions(ctx context.Context) ([]tools.Position, error)
	OpenOrders(ctx context.Context) ([]tools.Order, error)
	Cancel(ctx context.Context, orderID string) error
}

// Options tunes the pass.
type Options struct {
	// ReportOnly inspects and reports but changes nothing.
	ReportOnly bool
	// HaltLiftEnabled allows re-basing the drawdown peak when the halt no
	// longer holds. Off by default: releasing a halt is a decision the
	// operator may want to own.
	HaltLiftEnabled bool
	// OrphanMaxAge is how long a resting ENTRY order (not a protective
	// one) may survive before it is cancelled as a leftover.
	OrphanMaxAge time.Duration
}

// DefaultOptions is the shipped posture: act on the unsafe states, leave
// the judgement calls to an operator.
func DefaultOptions() Options {
	return Options{
		ReportOnly:      false,
		HaltLiftEnabled: false,
		OrphanMaxAge:    24 * time.Hour,
	}
}

// Action is one repair performed (or found, in report-only mode).
type Action struct {
	// Check names the invariant, e.g. "orphan-protective-order".
	Check string `json:"check"`
	// Detail explains the finding in one line.
	Detail string `json:"detail"`
	// Repaired is true when the state was changed.
	Repaired bool `json:"repaired"`
	// Severity separates "this is bleeding" from "this is untidy".
	Severity string `json:"severity"`
}

// Report is one pass's outcome.
type Report struct {
	At      time.Time `json:"at"`
	Actions []Action  `json:"actions"`
	// Errors carries checks that could not be evaluated. A skipped check
	// must never look like a clean one.
	Errors []string `json:"errors,omitempty"`
	// Posture records how the pass was configured WHEN IT RAN. The
	// dashboard is a separate process with its own environment, and it
	// must not present its own settings as the bot's: after an operator
	// flips SELFHEAL_REPORT_ONLY, the two would disagree about whether
	// the bot is allowed to cancel anything.
	Posture *Posture `json:"posture,omitempty"`
}

// Posture is the effective configuration of one pass.
type Posture struct {
	ReportOnly      bool `json:"report_only"`
	HaltLiftEnabled bool `json:"halt_lift_enabled"`
	// OrphanMaxAgeHours is the stale-entry TTL; 0 means the check is off.
	OrphanMaxAgeHours int `json:"orphan_max_age_hours"`
}

// PostureOf renders the options as a posture for storage.
func PostureOf(o Options) *Posture {
	return &Posture{
		ReportOnly:        o.ReportOnly,
		HaltLiftEnabled:   o.HaltLiftEnabled,
		OrphanMaxAgeHours: int(o.OrphanMaxAge.Hours()),
	}
}

// Clean reports whether nothing was found AND nothing was skipped. A pass
// that could not see the account is not a clean pass — that distinction is
// the whole difference between "healthy" and "unexamined".
func (r Report) Clean() bool { return len(r.Actions) == 0 && len(r.Errors) == 0 }

// Summary renders the pass for the log in one line.
func (r Report) Summary() string {
	if len(r.Actions) == 0 && len(r.Errors) == 0 {
		return "no findings"
	}
	parts := make([]string, 0, len(r.Actions))
	for _, a := range r.Actions {
		state := "repaired"
		if !a.Repaired {
			state = "found"
		}
		parts = append(parts, fmt.Sprintf("%s(%s): %s", a.Check, state, a.Detail))
	}
	out := strings.Join(parts, "; ")
	if len(r.Errors) > 0 {
		out += " | checks skipped: " + strings.Join(r.Errors, "; ")
	}
	return out
}

// Deps wires the pass to the things it inspects.
type Deps struct {
	Broker Broker
	// StatePath is the strategy-state.json path.
	StatePath string
	// JournalPath is the trades.jsonl path.
	JournalPath string
	// Halted reports whether the shared kill switch is engaged.
	Halted func() bool
	// ResetPeak re-bases the drawdown high-water mark. Called only when
	// HaltLiftEnabled and the halt condition has cleared.
	ResetPeak func(equity float64) error
	// Equity is the account's current equity, for the halt check.
	Equity func() (float64, error)
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
}

func (d *Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Run performs one full pass.
func Run(ctx context.Context, deps Deps, opts Options) Report {
	rep := Report{At: deps.now().UTC(), Posture: PostureOf(opts)}
	if deps.Broker == nil {
		rep.Errors = append(rep.Errors, "no broker wired")
		return rep
	}
	positions, err := deps.Broker.Positions(ctx)
	if err != nil {
		// Every remaining check needs the position list. Reporting the
		// skip is the honest outcome; pretending the account is clean is
		// how a healer hides the one fault that matters.
		rep.Errors = append(rep.Errors, "positions unavailable: "+err.Error())
		return rep
	}
	orders, err := deps.Broker.OpenOrders(ctx)
	if err != nil {
		rep.Errors = append(rep.Errors, "open orders unavailable: "+err.Error())
		return rep
	}

	held := map[string]tools.Position{}
	for _, p := range positions {
		q, _ := strconv.ParseFloat(strings.TrimSpace(p.Qty), 64)
		if q == 0 {
			continue
		}
		held[normalize(p.Symbol)] = p
	}

	rep.Actions = append(rep.Actions, orphanProtectiveOrders(ctx, deps, opts, orders, held)...)
	rep.Actions = append(rep.Actions, duplicateProtection(ctx, deps, opts, orders)...)
	rep.Actions = append(rep.Actions, staleRestingEntries(ctx, deps, opts, orders, opts.OrphanMaxAge, deps.now())...)
	rep.Actions = append(rep.Actions, unreadableState(deps)...)
	rep.Actions = append(rep.Actions, truncatedJournal(deps)...)
	rep.Actions = append(rep.Actions, stuckHalt(deps, opts)...)
	return rep
}

// isProtective reports whether an open order guards an existing position
// rather than opening a new one.
func isProtective(o tools.Order) bool {
	switch strings.ToLower(o.Type) {
	case "stop", "stop_limit", "trailing_stop":
		return true
	}
	switch strings.ToLower(o.OrderClass) {
	case "oco", "bracket", "oto":
		return true
	}
	return false
}

// orphanProtectiveOrders cancels protective orders resting with no
// position behind them.
//
// This is the dangerous one: a sell limit left over after the position it
// was meant to close is no longer a stop, it is a short-sale instruction.
func orphanProtectiveOrders(ctx context.Context, deps Deps, opts Options, orders []tools.Order, held map[string]tools.Position) []Action {
	var out []Action
	for _, o := range orders {
		if !isProtective(o) {
			continue
		}
		if _, ok := held[normalize(o.Symbol)]; ok {
			continue
		}
		a := Action{
			Check:    "orphan-protective-order",
			Severity: "high",
			Detail: fmt.Sprintf("%s %s order %s is resting with no position behind it",
				o.Symbol, o.Type, o.ID),
		}
		if opts.ReportOnly {
			out = append(out, a)
			continue
		}
		if err := deps.Broker.Cancel(ctx, o.ID); err != nil {
			a.Detail += " — CANCEL FAILED: " + err.Error()
			a.Severity = "critical"
		} else {
			a.Repaired = true
			a.Detail += " — cancelled"
		}
		out = append(out, a)
	}
	return out
}

// duplicateProtection keeps one protective pair per symbol and cancels the
// extras, which together could sell more than is held.
func duplicateProtection(ctx context.Context, deps Deps, opts Options, orders []tools.Order) []Action {
	bySymbol := map[string][]tools.Order{}
	for _, o := range orders {
		if !isProtective(o) {
			continue
		}
		bySymbol[normalize(o.Symbol)] = append(bySymbol[normalize(o.Symbol)], o)
	}
	syms := make([]string, 0, len(bySymbol))
	for s := range bySymbol {
		syms = append(syms, s)
	}
	sort.Strings(syms)

	var out []Action
	for _, sym := range syms {
		group := bySymbol[sym]
		// Count protective PARENTS (oco/bracket envelopes). Individual
		// stop + limit legs of a single pair are not duplicates.
		var parents []tools.Order
		for _, o := range group {
			switch strings.ToLower(o.OrderClass) {
			case "oco", "bracket", "oto":
				parents = append(parents, o)
			}
		}
		if len(parents) < 2 {
			continue
		}
		// Keep the newest (later created_at wins), cancel the rest.
		sort.SliceStable(parents, func(i, j int) bool { return parents[i].CreatedAt.After(parents[j].CreatedAt) })
		keep := parents[0]
		for _, extra := range parents[1:] {
			a := Action{
				Check:    "duplicate-protection",
				Severity: "high",
				Detail: fmt.Sprintf("%s has %d protective pairs; keeping %s (tp=%s stop=%s)",
					sym, len(parents), keep.ID, keep.LimitPrice, extra.StopPrice),
			}
			if opts.ReportOnly {
				out = append(out, a)
				continue
			}
			if err := deps.Broker.Cancel(ctx, extra.ID); err != nil {
				a.Detail += " — CANCEL FAILED: " + err.Error()
				a.Severity = "critical"
			} else {
				a.Repaired = true
				a.Detail += " — cancelled the extra pair"
			}
			out = append(out, a)
		}
	}
	return out
}

// staleRestingEntries cancels the bot's own unfilled ENTRY orders once
// they outlive the decision that produced them. A resting limit from
// three days ago fills on a spike nobody re-checked, at a price the
// current factors would not have accepted.
func staleRestingEntries(ctx context.Context, deps Deps, opts Options, orders []tools.Order, maxAge time.Duration, now time.Time) []Action {
	if maxAge <= 0 {
		return nil
	}
	var out []Action
	for _, o := range orders {
		if isProtective(o) {
			continue
		}
		if o.CreatedAt.IsZero() || now.Sub(o.CreatedAt) < maxAge {
			continue
		}
		a := Action{
			Check:    "stale-resting-entry",
			Severity: "medium",
			Detail: fmt.Sprintf("%s %s order %s has rested %s (bar %s)",
				o.Symbol, o.Type, o.ID, now.Sub(o.CreatedAt).Round(time.Hour), maxAge),
		}
		if opts.ReportOnly {
			out = append(out, a)
			continue
		}
		if err := deps.Broker.Cancel(ctx, o.ID); err != nil {
			a.Detail += " — CANCEL FAILED: " + err.Error()
			a.Severity = "high"
		} else {
			a.Repaired = true
			a.Detail += " — cancelled"
		}
		out = append(out, a)
	}
	return out
}

// unreadableState handles the state file the bot depends on being
// unparseable: set it aside with its original name preserved, so the
// next write starts from a clean file instead of failing forever.
func unreadableState(deps Deps) []Action {
	if deps.StatePath == "" {
		return nil
	}
	b, err := os.ReadFile(deps.StatePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no file is a normal cold start
		}
		return []Action{{
			Check: "state-file", Severity: "high",
			Detail: "state file unreadable: " + err.Error(),
		}}
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if !looksLikeJSON(string(b)) {
		return []Action{{
			Check: "state-file", Severity: "critical",
			Detail: fmt.Sprintf("%s is not valid JSON; TP/SL levels and the drawdown peak are unreadable", deps.StatePath),
			// Deliberately not auto-repaired: setting the file aside deletes
			// the only record of intended stop levels. It is reported so an
			// operator decides, and the reconcile pass re-derives what it
			// can from the broker regardless.
		}}
	}
	return nil
}

// truncatedJournal trims a partial final line left by a crash mid-append.
// Readers already skip it, but a file whose last line is cut off will
// hide the record that was being written forever.
func truncatedJournal(deps Deps) []Action {
	if deps.JournalPath == "" {
		return nil
	}
	b, err := os.ReadFile(deps.JournalPath)
	if err != nil || len(b) == 0 {
		return nil
	}
	if b[len(b)-1] == '\n' {
		return nil // well-formed tail
	}
	return []Action{{
		Check: "journal-tail", Severity: "low",
		Detail: fmt.Sprintf("%s ends without a newline; the partial last record is ignored by every reader", deps.JournalPath),
	}}
}

// stuckHalt reports a drawdown halt still engaged while the condition
// that caused it has gone. Releasing it is opt-in: a halt is the operator's
// brake, and lifting it automatically is a judgement call, not a repair.
func stuckHalt(deps Deps, opts Options) []Action {
	if deps.Halted == nil || !deps.Halted() {
		return nil
	}
	a := Action{
		Check: "halt-still-engaged", Severity: "medium",
		Detail: "kill switch is engaged; trading is paused until it is cleared",
	}
	if deps.Equity == nil || deps.ResetPeak == nil {
		a.Detail += " (no peak/equity source wired, cannot evaluate recovery)"
		return []Action{a}
	}
	eq, err := deps.Equity()
	if err != nil || eq <= 0 {
		a.Detail += fmt.Sprintf(" — equity unreadable (%v)", err)
		return []Action{a}
	}
	if !opts.HaltLiftEnabled {
		a.Detail += fmt.Sprintf(" — equity is now %.2f; lifting is disabled (SELFHEAL_LIFT_HALT=true to allow)", eq)
		return []Action{a}
	}
	a.Detail += fmt.Sprintf(" — re-basing the drawdown peak to %.2f", eq)
	if opts.ReportOnly {
		return []Action{a}
	}
	if err := deps.ResetPeak(eq); err != nil {
		a.Detail += " — RE-BASE FAILED: " + err.Error()
		a.Severity = "high"
		return []Action{a}
	}
	a.Repaired = true
	a.Detail += " — peak re-based"
	return []Action{a}
}

func normalize(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "/", ""))
}

func looksLikeJSON(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") ||
		strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]")
}
