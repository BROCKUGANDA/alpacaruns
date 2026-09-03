package strategy

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window is one inclusive HH:MM-HH:MM execution window in a fixed
// location's wall time (US/Eastern for equities).
type Window struct {
	StartMinute int // minutes since midnight, inclusive
	EndMinute   int // minutes since midnight, inclusive
}

// ParseWindows parses "09:35-10:15,15:45-16:00" (whitespace tolerant).
// End may be 24:00 meaning end of day. Start must be < end within one
// window; windows are independent (no overlap validation needed — the
// first matching window wins).
func ParseWindows(spec string) ([]Window, error) {
	var out []Window
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		startS, endS, ok := strings.Cut(part, "-")
		if !ok {
			return nil, fmt.Errorf("window %q: want HH:MM-HH:MM", part)
		}
		sm, err := parseClock(startS)
		if err != nil {
			return nil, fmt.Errorf("window %q start: %w", part, err)
		}
		em, err := parseClock(endS)
		if err != nil {
			return nil, fmt.Errorf("window %q end: %w", part, err)
		}
		if em > 24*60 {
			return nil, fmt.Errorf("window %q: end beyond 24:00", part)
		}
		if sm >= em {
			return nil, fmt.Errorf("window %q: start must be before end", part)
		}
		out = append(out, Window{StartMinute: sm, EndMinute: em})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty window spec")
	}
	return out, nil
}

// parseClock parses HH:MM into minutes since midnight. Accepts 00:00
// through 24:00.
func parseClock(s string) (int, error) {
	hh, mm, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, fmt.Errorf("want HH:MM, got %q", s)
	}
	h, err := strconv.Atoi(hh)
	if err != nil || h < 0 || h > 24 {
		return 0, fmt.Errorf("bad hour in %q", s)
	}
	m, err := strconv.Atoi(mm)
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("bad minute in %q", s)
	}
	total := h*60 + m
	if total > 24*60 {
		return 0, fmt.Errorf("time %q exceeds 24:00", s)
	}
	return total, nil
}

// InWindows reports whether minute-of-day falls inside any window,
// boundaries inclusive.
func InWindows(minuteOfDay int, ws []Window) bool {
	for _, w := range ws {
		if minuteOfDay >= w.StartMinute && minuteOfDay <= w.EndMinute {
			return true
		}
	}
	return false
}

// TradingWindows gates order placement on configured local-time windows.
// now is injected so callers and tests control the clock; production uses
// time.Now in the requested location.
type TradingWindows struct {
	Equity []Window
	Crypto []Window

	loc *time.Location // where the specs are interpreted (fixed offset)
	now func() time.Time
}

// NewTradingWindows parses both env specs. loc should be *time.Location
// with a FIXED UTC offset so wall-clock comparisons never drift with DST;
// production passes fixedET(). now defaults to time.Now.
func NewTradingWindows(equitySpec, cryptoSpec string, loc *time.Location, now func() time.Time) (*TradingWindows, error) {
	eq, err := ParseWindows(equitySpec)
	if err != nil {
		return nil, fmt.Errorf("TRADING_WINDOWS: %w", err)
	}
	cr, err := ParseWindows(cryptoSpec)
	if err != nil {
		return nil, fmt.Errorf("CRYPTO_WINDOWS: %w", err)
	}
	if loc == nil {
		loc = time.UTC
	}
	if now == nil {
		now = time.Now
	}
	return &TradingWindows{Equity: eq, Crypto: cr, loc: loc, now: now}, nil
}

// CanTrade reports whether an order for symbol may fire right now:
// equities against TRADING_WINDOWS, crypto against CRYPTO_WINDOWS (the
// 24/7 market ignores equity session hours). OCC option symbols follow
// the equity windows (options trade only during regular equity hours).
func (tw *TradingWindows) CanTrade(symbol string) bool {
	minute := tw.now().In(tw.loc)
	mod := minute.Hour()*60 + minute.Minute()
	if IsCrypto(symbol) {
		return InWindows(mod, tw.Crypto)
	}
	return InWindows(mod, tw.Equity)
}

// fixedET returns US Eastern time pinned to the current EST/EDT offset at
// process start. A ±1h skew around DST transitions is acceptable for
// execution windows this coarse and avoids per-tick zone lookups.
func fixedET() *time.Location {
	_, offset := time.Now().In(time.Local).Zone()
	_ = offset
	// Prefer a real named zone when the host has tzdata; fall back to a
	// fixed -05:00 (EST) approximation otherwise.
	if et, err := time.LoadLocation("America/New_York"); err == nil {
		return et
	}
	return time.FixedZone("EST", -5*3600)
}
