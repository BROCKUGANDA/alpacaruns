// Package learning turns the bot's own closed trades into bounded
// adjustments to how it trades.
//
// It exists because a factor score is a hypothesis, and until a trade
// closes nobody knows whether the score meant anything. The same
// composite of 0.62 can be a good entry in one asset class and a
// consistent loser in another. Only realized outcomes can say which.
//
// The engine is deliberately ASYMMETRIC, and that is its central
// property:
//
//   - it may RAISE the score floor above the operator's threshold, never
//     lower it below it;
//   - it may SHRINK position size, never grow it past the configured
//     sizing;
//   - it may TIGHTEN a stop, never widen it;
//   - it may QUARANTINE a symbol that keeps losing, and release it once
//     recent trades say otherwise.
//
// Every knob it can touch therefore moves in the direction that reduces
// risk or increases selectivity. A mis-calibrated estimate, a regime
// change, or a run of bad luck can make the bot trade less; none of them
// can make it trade bigger, take looser entries, or hold a wider stop
// than the operator authorised. Learning is a brake, not an
// accelerator.
//
// It is also SLOW by construction: nothing changes until LEARN_MIN_TRADES
// round-trips exist, a change must clear its own evidence bar, and the
// adjustments are recomputed from a bounded sample window rather than
// accumulated, so it cannot drift somewhere the data never supported.
package learning

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Sample is one closed round-trip: what the bot thought, what it risked,
// and what actually happened.
type Sample struct {
	// Symbol the trade was in.
	Symbol string `json:"symbol"`
	// Class is the asset class the entry was routed through
	// (equity, etf, index, option, crypto). Expectancy differs by class
	// — a crypto scalp and an equity swing cannot share one floor.
	Class string `json:"class"`
	// Score is the composite that justified the ENTRY, not the
	// operator's threshold. This is what makes calibration possible:
	// without the number the bot acted on, outcomes cannot be mapped
	// back to decisions.
	Score float64 `json:"score"`
	// Pct is the signed return of the round-trip (0.035 = +3.5%).
	Pct float64 `json:"pct"`
	// PnL is the signed dollar result.
	PnL float64 `json:"pnl"`
	// Hold is how long the position was open.
	Hold time.Duration `json:"hold"`
	// Closed is when the round-trip finished.
	Closed time.Time `json:"closed"`
}

// Outcome reports whether the round-trip made money.
func (s Sample) Outcome() string {
	if s.PnL > 0 || s.Pct > 0 {
		return "win"
	}
	return "loss"
}

// Config bounds the learner.
type Config struct {
	// Enabled is the master switch. Off means the engine still records
	// evidence but applies nothing.
	Enabled bool
	// MinTrades is how many closed round-trips are needed before any
	// adjustment is made. Below it the answer is always "no change".
	MinTrades int
	// EveryTrades is how many new closed trades trigger a recomputation.
	EveryTrades int
	// MinSamplesPerBucket is the bar a score bucket must clear before it
	// can influence the floor.
	MinSamplesPerBucket int
	// MinSamplesPerClass is the bar an asset class must clear before its
	// sizing or stop may move.
	MinSamplesPerClass int
	// MinSizeMultiplier clamps how far sizing may shrink.
	MinSizeMultiplier float64
	// MaxTightenFactor clamps how far a stop may be tightened.
	MaxTightenFactor float64
	// QuarantineWinRate quarantines a symbol at or below this win rate
	// once it has QuarantineTrades samples.
	QuarantineWinRate float64
	// QuarantineTrades is the sample count needed to quarantine.
	QuarantineTrades int
	// ScoreBuckets is the number of equal-width buckets the score range
	// is divided into for calibration.
	ScoreBuckets int
}

// DefaultConfig is the shipped posture: conservative, slow, and off
// until an operator turns it on.
func DefaultConfig() Config {
	return Config{
		Enabled:             false,
		MinTrades:           12,
		EveryTrades:         5,
		MinSamplesPerBucket: 4,
		MinSamplesPerClass:  8,
		MinSizeMultiplier:   0.25,
		MaxTightenFactor:    0.75,
		QuarantineWinRate:   0.25,
		QuarantineTrades:    6,
		ScoreBuckets:        5,
	}
}

// Validate rejects a configuration that would make the learner unsafe —
// for example one that allowed growth rather than shrinkage.
func (c Config) Validate() error {
	switch {
	case c.MinTrades <= 0:
		return fmt.Errorf("LEARN_MIN_TRADES must be > 0, got %d", c.MinTrades)
	case c.EveryTrades <= 0:
		return fmt.Errorf("LEARN_EVERY_TRADES must be > 0, got %d", c.EveryTrades)
	case c.MinSizeMultiplier <= 0 || c.MinSizeMultiplier > 1:
		return fmt.Errorf("LEARN_MIN_SIZE_MULTIPLIER must be in (0,1], got %g (learning may only shrink, never grow)", c.MinSizeMultiplier)
	case c.MaxTightenFactor <= 0 || c.MaxTightenFactor > 1:
		return fmt.Errorf("LEARN_MAX_TIGHTEN_FACTOR must be in (0,1], got %g (learning may only tighten, never widen)", c.MaxTightenFactor)
	case c.ScoreBuckets < 2:
		return fmt.Errorf("LEARN_SCORE_BUCKETS must be >= 2, got %d", c.ScoreBuckets)
	}
	return nil
}

// ClassAdjustment is what the learner concluded about one asset class.
type ClassAdjustment struct {
	Class          string  `json:"class"`
	Samples        int     `json:"samples"`
	WinRate        float64 `json:"win_rate"`
	Expectancy     float64 `json:"expectancy_pct"` // mean signed % per trade
	SizeMultiplier float64 `json:"size_multiplier"`
	StopMultiplier float64 `json:"stop_multiplier"`
	// Reason states the evidence in one line, so the adjustment is
	// auditable rather than a magic number.
	Reason string `json:"reason"`
}

// ScoreBucket is one slice of the score range with its realized results.
// This is the calibration table: it says what a score actually bought.
type ScoreBucket struct {
	From    float64 `json:"from"`
	To      float64 `json:"to"`
	Samples int     `json:"samples"`
	WinRate float64 `json:"win_rate"`
	// Expectancy is the mean signed % return in this bucket.
	Expectancy float64 `json:"expectancy_pct"`
}

// Adjustments is the learner's output, consumed by the entry path.
type Adjustments struct {
	// ComputedAt is when this snapshot was produced.
	ComputedAt time.Time `json:"computed_at"`
	// SamplesSeen is the total closed trades behind it.
	SamplesSeen int `json:"samples_seen"`
	// Active is false until MinTrades samples exist; nothing is applied
	// while it is false.
	Active bool `json:"active"`
	// ScoreFloorAdd is added to the configured score floor. It is never
	// negative: the operator's floor is policy, and learning can only
	// raise the bar.
	ScoreFloorAdd float64 `json:"score_floor_add"`
	// SuggestedFloor is configured floor + ScoreFloorAdd, for display.
	SuggestedFloor float64 `json:"suggested_floor"`
	// Classes holds the per-asset-class conclusions.
	Classes map[string]ClassAdjustment `json:"classes"`
	// Quarantined lists symbols blocked from new entries.
	Quarantined []Quarantined `json:"quarantined,omitempty"`
	// Buckets is the calibration table (evidence, not just conclusion).
	Buckets []ScoreBucket `json:"buckets,omitempty"`
	// Notes records what changed this computation, for the journal.
	Notes []string `json:"notes,omitempty"`
}

// Quarantined names a blocked symbol and why.
type Quarantined struct {
	Symbol  string  `json:"symbol"`
	Samples int     `json:"samples"`
	WinRate float64 `json:"win_rate"`
	Reason  string  `json:"reason"`
}

// SizeMultiplierFor returns the bounded sizing factor for a class.
// Unknown classes and an inactive snapshot return 1.0 — no evidence
// means no opinion, and no opinion means do not touch the operator's
// sizing.
func (a Adjustments) SizeMultiplierFor(class string) float64 {
	if !a.Active {
		return 1.0
	}
	ca, ok := a.Classes[strings.ToLower(strings.TrimSpace(class))]
	if !ok {
		return 1.0
	}
	return safeUnit(ca.SizeMultiplier)
}

// StopMultiplierFor returns the bounded stop factor for a class, again
// clamped to (0,1] so a stop can only tighten.
func (a Adjustments) StopMultiplierFor(class string) float64 {
	if !a.Active {
		return 1.0
	}
	ca, ok := a.Classes[strings.ToLower(strings.TrimSpace(class))]
	if !ok {
		return 1.0
	}
	return safeUnit(ca.StopMultiplier)
}

// safeUnit accepts only a multiplier in (0,1] and nothing else. Written as
// an affirmative test rather than a rejection because NaN compares false
// against both bounds: `if m <= 0 || m > 1` lets NaN through, and a NaN
// multiplier would silently size a position to zero or blow the bracket
// math up. Anything unusable means "no opinion", which is 1.0.
func safeUnit(m float64) float64 {
	if m > 0 && m <= 1 && !math.IsInf(m, 0) {
		return m
	}
	return 1.0
}

// Quarantined reports whether a symbol is blocked from new entries.
func (a Adjustments) IsQuarantined(symbol string) (Quarantined, bool) {
	want := strings.ToUpper(strings.TrimSpace(symbol))
	for _, q := range a.Quarantined {
		if strings.ToUpper(strings.TrimSpace(q.Symbol)) == want {
			return q, true
		}
	}
	return Quarantined{}, false
}

// Engine computes Adjustments from samples. It is stateless apart from
// its configuration.
type Engine struct {
	cfg Config
}

// NewEngine validates the configuration and returns the engine.
func NewEngine(cfg Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Engine{cfg: cfg}, nil
}

// Config returns the engine's bounds.
func (e *Engine) Config() Config { return e.cfg }

// Calibrate computes the adjustments implied by samples.
//
// baseFloor is the operator's configured score floor. It is an input
// rather than something learned, and the returned floor can only be at
// or above it.
func (e *Engine) Calibrate(samples []Sample, baseFloor float64, now time.Time) Adjustments {
	out := Adjustments{
		ComputedAt:     now.UTC(),
		SamplesSeen:    len(samples),
		SuggestedFloor: baseFloor,
		Classes:        map[string]ClassAdjustment{},
	}
	if len(samples) < e.cfg.MinTrades {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"learning idle: %d closed trades, needs %d before any adjustment", len(samples), e.cfg.MinTrades))
		return out
	}
	if !e.cfg.Enabled {
		out.Notes = append(out.Notes, "learning configured off: evidence recorded, nothing applied")
		return out
	}
	out.Active = true

	// --- score calibration -------------------------------------------------
	buckets := e.bucketize(samples)
	out.Buckets = buckets
	add := e.floorUplift(buckets, baseFloor)
	if add > 0 {
		out.ScoreFloorAdd = add
		out.SuggestedFloor = baseFloor + add
		out.Notes = append(out.Notes, fmt.Sprintf(
			"score floor raised %.3f to %.3f: the %d lowest buckets lose money (%s)",
			add, out.SuggestedFloor, e.countFailing(buckets, baseFloor+add), summarize(buckets)))
	}

	// --- per-class sizing and stops ---------------------------------------
	for _, cls := range groupedByClass(samples) {
		if len(cls.samples) < e.cfg.MinSamplesPerClass {
			out.Classes[cls.class] = ClassAdjustment{
				Class: cls.class, Samples: len(cls.samples),
				WinRate: winRate(cls.samples), Expectancy: expectancy(cls.samples),
				SizeMultiplier: 1, StopMultiplier: 1,
				Reason: fmt.Sprintf("%d trades: below the %d-sample bar, no adjustment", len(cls.samples), e.cfg.MinSamplesPerClass),
			}
			continue
		}
		wr := winRate(cls.samples)
		exp := expectancy(cls.samples)
		size := shrinkFactor(wr, exp, e.cfg.MinSizeMultiplier)
		stop := stopFactor(cls.samples, wr, e.cfg.MaxTightenFactor)
		adj := ClassAdjustment{
			Class: cls.class, Samples: len(cls.samples), WinRate: wr, Expectancy: exp,
			SizeMultiplier: round4(size), StopMultiplier: round4(stop),
			Reason: fmt.Sprintf("win rate %.0f%% over %d trades, expectancy %+.3f%% per trade -> size x%.2f, stop x%.2f",
				100*wr, len(cls.samples), 100*exp, round4(size), round4(stop)),
		}
		out.Classes[cls.class] = adj
		if size < 1 || stop < 1 {
			out.Notes = append(out.Notes, fmt.Sprintf("class %s: %s", cls.class, adj.Reason))
		}
	}

	// --- quarantine -------------------------------------------------------
	for _, grp := range groupedByClass(samples) {
		bySym := map[string][]Sample{}
		for _, s := range grp.samples {
			bySym[strings.ToUpper(s.Symbol)] = append(bySym[strings.ToUpper(s.Symbol)], s)
		}
		syms := make([]string, 0, len(bySym))
		for s := range bySym {
			syms = append(syms, s)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			ss := bySym[sym]
			if len(ss) < e.cfg.QuarantineTrades {
				continue
			}
			wr := winRate(ss)
			if wr > e.cfg.QuarantineWinRate {
				continue
			}
			q := Quarantined{
				Symbol: sym, Samples: len(ss), WinRate: wr,
				Reason: fmt.Sprintf("win rate %.0f%% over %d closed trades (bar %.0f%%) — blocked for new entries",
					100*wr, len(ss), 100*e.cfg.QuarantineWinRate),
			}
			out.Quarantined = append(out.Quarantined, q)
			out.Notes = append(out.Notes, "quarantined "+sym+": "+q.Reason)
		}
	}
	return out
}

// bucketize splits the score range into equal-width buckets and computes
// each bucket's realized win rate and expectancy.
func (e *Engine) bucketize(samples []Sample) []ScoreBucket {
	n := e.cfg.ScoreBuckets
	buckets := make([]ScoreBucket, n)
	counts := make([]int, n)
	wins := make([]int, n)
	sumPct := make([]float64, n)
	for i := range buckets {
		buckets[i].From = float64(i) / float64(n)
		buckets[i].To = float64(i+1) / float64(n)
	}
	for _, s := range samples {
		// A fill that never carried a score (backfilled by order id, or
		// placed by hand) is UNATTRIBUTABLE: filing it into the 0.00
		// bucket would invent evidence that the lowest scores lose, and
		// the learner would then raise the floor to escape its own
		// fabrication. Only scored trades may calibrate.
		if s.Score <= 0 || math.IsNaN(s.Score) || math.IsInf(s.Score, 0) {
			continue
		}
		sc := s.Score
		if sc < 0 {
			sc = 0
		}
		if sc > 1 {
			sc = 0.999999
		}
		i := int(sc * float64(n))
		if i >= n {
			i = n - 1
		}
		counts[i]++
		sumPct[i] += s.Pct
		if s.PnL > 0 || s.Pct > 0 {
			wins[i]++
		}
	}
	for i := range buckets {
		buckets[i].Samples = counts[i]
		if counts[i] > 0 {
			buckets[i].WinRate = float64(wins[i]) / float64(counts[i])
			buckets[i].Expectancy = sumPct[i] / float64(counts[i])
		}
	}
	return buckets
}

// floorUplift raises the entry floor to the lowest bucket that has
// demonstrated non-negative expectancy. Entries scored below it have
// historically lost money, so the bot stops taking them.
//
// It refuses to conclude anything about a bucket it has barely seen, and
// it will never propose a floor below the operator's own.
func (e *Engine) floorUplift(buckets []ScoreBucket, baseFloor float64) float64 {
	// Find the lowest bucket boundary at or above the base floor from
	// which every remaining bucket with enough evidence is non-negative.
	var firstBad float64 = -1
	for _, b := range buckets {
		if b.To <= baseFloor {
			continue // below the operator's floor: already not tradable
		}
		if b.Samples < e.cfg.MinSamplesPerBucket {
			continue // not enough evidence to judge
		}
		if b.Expectancy < 0 {
			// Everything at or below this bucket's top has been losing.
			// Raise the bar to the top of this bucket and re-check the
			// higher ones.
			firstBad = b.To
		} else if firstBad >= 0 && b.From < firstBad {
			// A good bucket sits inside the bad span: the uplift cannot
			// jump over it, so the bar stops below it.
			firstBad = b.From
			break
		}
	}
	if firstBad < 0 {
		return 0
	}
	add := firstBad - baseFloor
	if add <= 0 {
		return 0
	}
	// Bound the move: a single recomputation may not seize the whole
	// score range, because that is indistinguishable from a bug and would
	// silently stop all trading.
	const maxSingleUplift = 0.15
	if add > maxSingleUplift {
		add = maxSingleUplift
	}
	return round4(add)
}

func (e *Engine) countFailing(buckets []ScoreBucket, floor float64) int {
	n := 0
	for _, b := range buckets {
		if b.To <= floor || b.Samples < e.cfg.MinSamplesPerBucket {
			continue
		}
		if b.Expectancy < 0 {
			n++
		}
	}
	return n
}

type classSamples struct {
	class   string
	samples []Sample
}

func groupedByClass(samples []Sample) []classSamples {
	byClass := map[string][]Sample{}
	for _, s := range samples {
		c := strings.ToLower(strings.TrimSpace(s.Class))
		if c == "" {
			c = "unknown"
		}
		byClass[c] = append(byClass[c], s)
	}
	keys := make([]string, 0, len(byClass))
	for c := range byClass {
		keys = append(keys, c)
	}
	sort.Strings(keys)
	out := make([]classSamples, 0, len(keys))
	for _, k := range keys {
		out = append(out, classSamples{class: k, samples: byClass[k]})
	}
	return out
}

// shrinkFactor maps realized quality to a sizing multiplier in
// [MinSizeMultiplier, 1]. It never exceeds 1: learning shrinks or leaves
// alone.
func shrinkFactor(winRate, expectancy, minMult float64) float64 {
	if winRate >= 0.55 && expectancy > 0 {
		return 1.0 // healthy: full configured size
	}
	// Linear in how far below break-even the class is, floored so a bad
	// class still trades something rather than silently vanishing.
	score := 1.0
	if winRate < 0.50 {
		score -= (0.50 - winRate) * 2.0
	}
	if expectancy < 0 {
		penalty := -expectancy * 6.0 // a -5%/trade class loses 0.30
		if penalty > 0.5 {
			penalty = 0.5
		}
		score -= penalty
	}
	if score < minMult {
		return minMult
	}
	if score > 1 {
		return 1
	}
	return score
}

// stopFactor tightens a stop only when stop-outs dominate the outcomes,
// which is the signature of a stop set wider than the noise it trades
// through. It never widens: a wider stop is more risk per trade, and
// that is the operator's call, not something to be learned into being.
func stopFactor(samples []Sample, winRate float64, maxTighten float64) float64 {
	if len(samples) == 0 {
		return 1.0
	}
	// A class that wins rarely and bleeds steadily is better held at a
	// tighter stop; one that wins often is left alone.
	if winRate >= 0.45 {
		return 1.0
	}
	shortfall := 0.45 - winRate // 0..0.45
	tighten := 1.0 - shortfall*1.5
	if tighten < maxTighten {
		return maxTighten
	}
	return tighten
}

func winRate(samples []Sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	w := 0
	for _, s := range samples {
		if s.PnL > 0 || s.Pct > 0 {
			w++
		}
	}
	return float64(w) / float64(len(samples))
}

func expectancy(samples []Sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += s.Pct
	}
	return sum / float64(len(samples))
}

func summarize(buckets []ScoreBucket) string {
	parts := make([]string, 0, len(buckets))
	for _, b := range buckets {
		if b.Samples == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%.2f-%.2f: %d trades, %+.2f%%",
			b.From, b.To, b.Samples, 100*b.Expectancy))
	}
	if len(parts) == 0 {
		return "no scored samples"
	}
	return strings.Join(parts, "; ")
}

func round4(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return math.Round(x*10000) / 10000
}
