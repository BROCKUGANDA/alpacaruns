package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	// Embedded so the exchange zone exists on every host this runs on. The
	// API image is `scratch` with nothing but the binary and a CA bundle, and
	// the bot's own unit may start on a minimal base: without tzdata
	// LoadLocation("America/New_York") fails and the fallback is a fixed
	// EST offset, which puts every session boundary an hour off for roughly
	// half the year — an hour that lands squarely on the open and the close.
	_ "time/tzdata"
)

// Session is the US equity market state at an instant, resolved from
// Alpaca's own calendar rather than a hardcoded wall-clock guess.
type Session int

const (
	// SessionUnknown means the calendar or clock could not be read. Every
	// caller treats it as "do not trade": the alternative — assuming open —
	// puts resting orders into a market whose state we do not know, and
	// assuming closed silently turns the bot off during an outage.
	SessionUnknown Session = iota
	// SessionClosed is outside every session: night, weekend, or holiday.
	SessionClosed
	// SessionPreMarket is between the extended open (04:00 ET) and the
	// regular open.
	SessionPreMarket
	// SessionRegular is the open-to-close auction session.
	SessionRegular
	// SessionAfterHours is between the regular close and the extended close
	// (20:00 ET).
	SessionAfterHours
)

func (s Session) String() string {
	switch s {
	case SessionClosed:
		return "closed"
	case SessionPreMarket:
		return "pre-market"
	case SessionRegular:
		return "regular"
	case SessionAfterHours:
		return "after-hours"
	default:
		return "unknown"
	}
}

// Extended reports whether this is one of the rest-of-hours sessions.
func (s Session) Extended() bool { return s == SessionPreMarket || s == SessionAfterHours }

// DayWindow is one trading day as the broker's calendar describes it.
type DayWindow struct {
	Date          string // YYYY-MM-DD in the exchange's own timezone
	Trading       bool   // false = holiday/weekend: the day is not on the calendar
	ExtendedOpen  time.Time
	RegularOpen   time.Time
	RegularClose  time.Time
	ExtendedClose time.Time
}

// calendarDay is the raw /v2/calendar row. Measured response:
//
//	{"close":"16:00","date":"2026-10-26","open":"09:30",
//	 "session_close":"2000","session_open":"0400","settlement_date":"2026-10-27"}
//
// There is no day_type field on this endpoint, so "is today a trading day"
// is answered by the row's PRESENCE, not by a flag: Alpaca omits weekends
// and market holidays entirely.
type calendarDay struct {
	Date           string `json:"date"`
	Open           string `json:"open"`
	Close          string `json:"close"`
	SessionOpen    string `json:"session_open"`
	SessionClose   string `json:"session_close"`
	SettlementDate string `json:"settlement_date"`
}

// Calendar returns the trading days in [start, end] (inclusive, YYYY-MM-DD in
// exchange time), read as US/Eastern wall clock. A day absent from the result
// is not a trading day.
func (c *Client) Calendar(ctx context.Context, start, end string) ([]DayWindow, error) {
	return c.CalendarIn(ctx, start, end, etLocation)
}

// CalendarIn is Calendar with the interpreting zone supplied by the caller.
// The row carries naive HH:MM times with no offset, so the zone is not a
// display detail — it decides when the session starts. Sessions passes its own
// Location, which keeps an injected clock and an injected zone from describing
// two different days: a caller that sets Location and reads rows in a
// package-level default silently compares the clock of one zone to the bounds
// of another.
func (c *Client) CalendarIn(ctx context.Context, start, end string, loc *time.Location) ([]DayWindow, error) {
	if loc == nil {
		loc = etLocation
	}
	q := url.Values{}
	q.Set("start", start)
	q.Set("end", end)
	var raw []calendarDay
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/calendar", q, nil, &raw); err != nil {
		return nil, fmt.Errorf("alpaca calendar %s..%s: %w", start, end, err)
	}
	out := make([]DayWindow, 0, len(raw))
	for _, d := range raw {
		dw, err := d.window(loc)
		if err != nil {
			return nil, err
		}
		out = append(out, dw)
	}
	return out, nil
}

// window converts one calendar row into absolute instants. The row gives
// naive HH:MM times against a YYYY-MM-DD date, all in exchange time, so they
// are parsed in loc and only then become real instants.
func (d calendarDay) window(loc *time.Location) (DayWindow, error) {
	day, err := time.ParseInLocation("2006-01-02", d.Date, loc)
	if err != nil {
		return DayWindow{}, fmt.Errorf("calendar date %q: %w", d.Date, err)
	}
	at := func(s string) (time.Time, error) {
		h, m, err := parseCalendarTime(s)
		if err != nil {
			return time.Time{}, err
		}
		return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc), nil
	}
	dw := DayWindow{Date: d.Date, Trading: true}
	// session_open/session_close are the EXTENDED bounds; open/close the
	// regular ones. A row missing the extended bounds still describes a
	// regular session, so fall back to the regular ones rather than
	// inventing 04:00/20:00 — an invented bound is a claim about the venue.
	if dw.RegularOpen, err = at(d.Open); err != nil {
		return dw, err
	}
	if dw.RegularClose, err = at(d.Close); err != nil {
		return dw, err
	}
	if dw.ExtendedOpen, err = at(orDefaultStr(d.SessionOpen, d.Open)); err != nil {
		return dw, err
	}
	if dw.ExtendedClose, err = at(orDefaultStr(d.SessionClose, d.Close)); err != nil {
		return dw, err
	}
	return dw, nil
}

// parseCalendarTime reads one calendar bound. The API mixes two formats in
// the SAME row — measured from /v2/calendar:
//
//	{"close":"16:00","date":"2026-10-26","open":"09:30",
//	 "session_close":"2000","session_open":"0400","settlement_date":"2026-10-27"}
//
// regular open/close arrive as "HH:MM", the extended bounds as "HHMM".
// Accepting only one shape fails every row, and a caller that treats a failed
// read as "closed" then reports a market that never opens — which is exactly
// what this code did until the live endpoint contradicted the fixture.
func parseCalendarTime(s string) (hour, minute int, err error) {
	s = strings.TrimSpace(s)
	var hs, ms string
	switch len(s) {
	case 4:
		hs, ms = s[0:2], s[2:4]
	case 5:
		var ok bool
		if hs, ms, ok = strings.Cut(s, ":"); !ok || len(hs) != 2 || len(ms) != 2 {
			return 0, 0, fmt.Errorf("calendar time %q: want HHMM or HH:MM", s)
		}
	default:
		return 0, 0, fmt.Errorf("calendar time %q: want HHMM or HH:MM", s)
	}
	h, err := strconv.Atoi(hs)
	if err != nil || h > 24 {
		return 0, 0, fmt.Errorf("calendar time %q: bad hour", s)
	}
	m, err := strconv.Atoi(ms)
	if err != nil || m > 59 {
		return 0, 0, fmt.Errorf("calendar time %q: bad minute", s)
	}
	return h, m, nil
}

// Sessions resolves the current market session from the broker's calendar,
// caching the three-day window it spans. It exists because a wall-clock
// approximation of "extended hours" (weekdays 04:00-20:00) is wrong on every
// market holiday and every half day, and those are exactly the days a bot
// left running unattended finds an empty book.
type Sessions struct {
	Client *Client
	// Now is injected for tests; nil means time.Now.
	Now func() time.Time
	// Location the calendar's naive times belong to; nil means US/Eastern.
	Location *time.Location

	mu       sync.Mutex
	cachedAt time.Time
	window   map[string]DayWindow
	fetched  time.Time
	err      error
}

// dayCacheTTL bounds how long a fetched calendar window is reused. One day
// at most: the window is re-fetched when the ET date changes anyway, and the
// TTL covers a process that starts late in a day and rolls over.
const dayCacheTTL = 6 * time.Hour

// Current returns the session at the source's clock reading, plus the
// trading-day window that produced it. A calendar failure returns
// SessionUnknown with the error; callers must fail closed.
func (s *Sessions) Current(ctx context.Context) (Session, DayWindow, error) {
	if s == nil || s.Client == nil {
		return SessionUnknown, DayWindow{}, fmt.Errorf("no session source wired")
	}
	now := s.now()
	loc := s.location()
	etNow := now.In(loc)
	key := etNow.Format("2006-01-02")

	days, err := s.days(ctx, loc, etNow)
	if err != nil {
		return SessionUnknown, DayWindow{}, err
	}
	day, ok := days[key]
	if !ok {
		// Today is not on the calendar: weekend or holiday. Nothing
		// executes, extended or otherwise, and an order placed now would
		// sit until the next real session.
		return SessionClosed, DayWindow{Date: key, Trading: false}, nil
	}
	switch {
	case etNow.Before(day.ExtendedOpen):
		return SessionClosed, day, nil
	case etNow.Before(day.RegularOpen):
		return SessionPreMarket, day, nil
	case etNow.Before(day.RegularClose):
		return SessionRegular, day, nil
	case etNow.Before(day.ExtendedClose):
		return SessionAfterHours, day, nil
	default:
		return SessionClosed, day, nil
	}
}

// days returns the cached calendar window, refetching when the requested
// date is missing or the cache is old. The window is fetched one day
// backwards and forwards so the boundary dates resolve without a second call.
func (s *Sessions) days(ctx context.Context, loc *time.Location, etNow time.Time) (map[string]DayWindow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := etNow.Format("2006-01-02")
	if s.window != nil {
		if _, ok := s.window[key]; ok {
			if time.Since(s.fetched) < dayCacheTTL {
				return s.window, nil
			}
		}
	}
	if s.err != nil && time.Since(s.fetched) < 30*time.Second {
		return nil, s.err
	}
	start := etNow.AddDate(0, 0, -1).Format("2006-01-02")
	end := etNow.AddDate(0, 0, 1).Format("2006-01-02")
	days, err := s.Client.CalendarIn(ctx, start, end, loc)
	s.fetched = time.Now()
	if err != nil {
		// Remember the failure briefly so a broker outage does not turn
		// every tick into a fresh HTTP call, but do not cache it past a
		// few seconds — the session answer must come back as soon as the
		// calendar does.
		s.err = err
		return nil, err
	}
	s.err = nil
	m := make(map[string]DayWindow, len(days))
	for _, d := range days {
		m[d.Date] = d
	}
	s.window = m
	return m, nil
}

func (s *Sessions) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sessions) location() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return etLocation
}

// etLocation is US/Eastern, which is where every Alpaca session bound is
// expressed. A host without tzdata falls back to a fixed EST offset; the
// fallback is deliberately not a no-location answer, because "we do not know
// what time it is" would disable the market-hours gate entirely.
var etLocation = func() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.FixedZone("EST", -5*3600)
}()
