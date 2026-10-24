package api

// learning.go serves the two self-modifying subsystems to the dashboard:
// what the learner has concluded, and what the self-heal pass last found.
//
// Both read files the trading bot writes. This process must not
// recompute them: a dashboard that shows its own estimate of the
// adjustments would be displaying numbers no order was ever placed
// against.

import (
	"net/http"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/learning"
	"github.com/BROCKUGANDA/alpacaruns/selfheal"
)

// LearningResponse is GET /api/learning.
type LearningResponse struct {
	// Enabled is the operator's LEARN_ENABLED switch. The adjustments may
	// still be populated while it is false: the engine records evidence
	// it is not allowed to act on, which is what makes turning it on a
	// considered decision.
	Enabled bool `json:"enabled"`
	// Bounds are the limits the learner runs under, so the dashboard can
	// show that a shrink is bounded rather than open-ended.
	Bounds LearningBounds       `json:"bounds"`
	Adj    learning.Adjustments `json:"adjustments"`
}

// LearningBounds mirrors the engine's configured clamps.
type LearningBounds struct {
	MinTrades          int     `json:"min_trades"`
	EveryTrades        int     `json:"every_trades"`
	MinSamplesPerClass int     `json:"min_samples_per_class"`
	MinSamplesBucket   int     `json:"min_samples_per_bucket"`
	MinSizeMultiplier  float64 `json:"min_size_multiplier"`
	MaxTightenFactor   float64 `json:"max_tighten_factor"`
	QuarantineWinRate  float64 `json:"quarantine_win_rate"`
	QuarantineTrades   int     `json:"quarantine_trades"`
	ScoreBuckets       int     `json:"score_buckets"`
}

// SelfHealResponse is GET /api/selfheal.
type SelfHealResponse struct {
	Enabled     bool            `json:"enabled"`
	ReportOnly  bool            `json:"report_only"`
	HaltLift    bool            `json:"halt_lift_enabled"`
	OrphanHours int             `json:"orphan_max_age_hours"`
	EveryTicks  int             `json:"every_ticks"`
	Report      selfheal.Report `json:"report"`
}

// handleLearning returns the adjustments the trading bot is applying.
func (s *Server) handleLearning(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	adj, err := learning.NewStore(s.settings.TradeLog).Load()
	if err != nil {
		// The file is the bot's output. An unreadable one means the
		// dashboard would be showing nothing while the bot trades under
		// something — say so rather than render an empty table.
		writeError(w, http.StatusServiceUnavailable, "learning_state_unreadable", err.Error())
		return
	}
	bounds := learningBoundsOf(s.cfg)
	writeJSON(w, http.StatusOK, LearningResponse{
		Enabled: s.cfg != nil && s.cfg.LearnEnabled,
		Bounds:  bounds,
		Adj:     adj,
	})
}

// handleSelfHeal returns the last pass the trading bot ran.
func (s *Server) handleSelfHeal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	rep, err := selfheal.NewReportStore(s.settings.TradeLog).Load()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "selfheal_report_unreadable", err.Error())
		return
	}
	every := 0
	orphan := 0
	if s.cfg != nil {
		every = s.cfg.SelfHealEveryTicks
		orphan = s.cfg.SelfHealOrphanHours
	}
	resp := SelfHealResponse{
		Enabled:     s.cfg != nil && s.cfg.SelfHealEnabled,
		ReportOnly:  s.cfg != nil && s.cfg.SelfHealReportOnly,
		HaltLift:    s.cfg != nil && s.cfg.SelfHealLiftHalt,
		OrphanHours: orphan,
		EveryTicks:  every,
		Report:      rep,
	}
	// The report carries the posture the BOT had when it ran. This
	// process starts from its own environment, which can disagree after a
	// config change and a bot restart — and the dashboard must show what
	// acted on the account, not what this container happens to think.
	if p := rep.Posture; p != nil {
		resp.ReportOnly = p.ReportOnly
		resp.HaltLift = p.HaltLiftEnabled
		if p.OrphanMaxAgeHours > 0 {
			resp.OrphanHours = p.OrphanMaxAgeHours
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// learningBoundsOf maps the operator's config onto the display bounds.
// Unset values fall back to the engine's shipped defaults rather than
// zero, so a dashboard never reads "may shrink to 0x".
func learningBoundsOf(cfg *config.Config) LearningBounds {
	d := learning.DefaultConfig()
	if cfg == nil {
		return LearningBounds{
			MinTrades: d.MinTrades, EveryTrades: d.EveryTrades,
			MinSamplesPerClass: d.MinSamplesPerClass, MinSamplesBucket: d.MinSamplesPerBucket,
			MinSizeMultiplier: d.MinSizeMultiplier, MaxTightenFactor: d.MaxTightenFactor,
			QuarantineWinRate: d.QuarantineWinRate, QuarantineTrades: d.QuarantineTrades,
			ScoreBuckets: d.ScoreBuckets,
		}
	}
	pick := func(v, def float64) float64 {
		if v > 0 {
			return v
		}
		return def
	}
	pickI := func(v, def int) int {
		if v > 0 {
			return v
		}
		return def
	}
	return LearningBounds{
		MinTrades:          pickI(cfg.LearnMinTrades, d.MinTrades),
		EveryTrades:        pickI(cfg.LearnEveryTrades, d.EveryTrades),
		MinSamplesPerClass: pickI(cfg.LearnMinPerClass, d.MinSamplesPerClass),
		MinSamplesBucket:   d.MinSamplesPerBucket,
		MinSizeMultiplier:  pick(cfg.LearnMinSizeMult, d.MinSizeMultiplier),
		MaxTightenFactor:   pick(cfg.LearnMaxTightenMult, d.MaxTightenFactor),
		QuarantineWinRate:  pick(cfg.LearnQuarantineWinPct, d.QuarantineWinRate),
		QuarantineTrades:   pickI(cfg.LearnQuarantineTrades, d.QuarantineTrades),
		ScoreBuckets:       pickI(cfg.LearnScoreBuckets, d.ScoreBuckets),
	}
}
