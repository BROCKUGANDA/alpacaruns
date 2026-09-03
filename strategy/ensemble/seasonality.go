package ensemble

import (
	"context"
	"fmt"
	"time"
)

// seasonality.go: calendar-tilt voice. Turn-of-month (last trading
// session of the month + first 3 sessions of the next) plus mild
// day-of-week tilts for ETFs. Outputs WEAK signals only — the gater's
// MIN_ENSEMBLE_CONFIDENCE floor (default 0.55) means a seasonal tilt can
// never alone trigger a trade; it only nudges an already-close vote.

const (
	tomLastSessions = 1 // last trading session(s) of month in the window
	tomFirstDays    = 3 // first sessions of next month
	seasonalConf    = 0.45
)

// SeasonalityExpert is the calendar voice.
type SeasonalityExpert struct{}

// Name implements Expert.
func (*SeasonalityExpert) Name() string { return "seasonality" }

// Evaluate emits a weak-confidence Buy inside turn-of-month windows and
// a day-of-week tilt (Mon/Wed positive, Fri negative for ETFs).
func (*SeasonalityExpert) Evaluate(_ context.Context, symbol string, data MarketData) ([]Signal, error) {
	now := data.Now
	if now.IsZero() {
		now = time.Now()
	}

	if d := turnOfMonthDay(now); d > 0 {
		return []Signal{{Symbol: symbol, Action: ActionBuy, Confidence: seasonalConf,
			Regime: RegimeCalendarOnly,
			Reason: fmt.Sprintf("seasonality: turn-of-month day %d tilt", d)}}, nil
	}

	switch now.Weekday() {
	case time.Monday, time.Wednesday:
		return []Signal{{Symbol: symbol, Action: ActionBuy, Confidence: 0.40,
			Regime: RegimeCalendarOnly,
			Reason: fmt.Sprintf("seasonality: %s mild positive tilt", now.Weekday())}}, nil
	case time.Friday:
		return []Signal{holdSig(symbol, "seasonality: Friday mild negative tilt")}, nil
	default:
		return []Signal{holdSig(symbol, "seasonality: neutral day")}, nil
	}
}

// turnOfMonthDay returns 1..4 when now sits in the TOM window
// (last session of the month = 1, first three of next month = 2..4);
// 0 otherwise. Calendar approximation: it does not know holidays, so
// "last trading day" is approximated as "last weekday".
func turnOfMonthDay(now time.Time) int {
	y, m, d := now.Date()
	daysInMonth := time.Date(y, m+1, 0, 0, 0, 0, 0, now.Location()).Day()

	if !isWeekday(now) {
		return 0
	}
	if daysInMonth-d < tomLastSessions { // within last session(s)
		return 1
	}
	if d <= tomFirstDays && isWeekday(time.Date(y, m, d, 12, 0, 0, 0, now.Location())) {
		return 1 + d
	}
	return 0
}

func isWeekday(t time.Time) bool {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	return true
}

var _ Expert = (*SeasonalityExpert)(nil)
