package tools

// Session parsing is only as good as the payload shape it was written
// against, and /v2/calendar does not use one consistent time format. These
// tests carry the response the paper API actually returned, verbatim, because
// a fixture that invents a tidy shape is how a parser that fails on every row
// passed review — and a failed read reported "market closed" forever.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// calendarServer serves a fixed /calendar body and a trading client pointed at
// it, so session tests read the vendor's real payload instead of a shape this
// file invented.
func calendarServer(t *testing.T, body string) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/calendar", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	return srv, NewClient("k", "s", srv.URL, srv.URL)
}

// measuredCalendar is the live /v2/calendar response for 2026-10-25..27,
// captured from the paper API on 2026-10-25. Note the mixed formats:
// open/close as "HH:MM", session_open/session_close as "HHMM".
const measuredCalendar = `[
 {"close":"16:00","date":"2026-10-26","open":"09:30","session_close":"2000","session_open":"0400","settlement_date":"2026-10-27"},
 {"close":"13:00","date":"2026-11-27","open":"09:30","session_close":"1700","session_open":"0400","settlement_date":"2026-11-30"}
]`

func TestCalendarRowParsesTheMeasuredShapes(t *testing.T) {
	var raw []calendarDay
	if err := json.Unmarshal([]byte(measuredCalendar), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	et := time.FixedZone("ET", -4*3600)
	dw, err := raw[0].window(et)
	if err != nil {
		t.Fatalf("window on the measured row: %v", err)
	}
	if got := dw.ExtendedOpen.In(et).Format("15:04"); got != "04:00" {
		t.Fatalf("extended open = %s, want 04:00", got)
	}
	if got := dw.RegularOpen.In(et).Format("15:04"); got != "09:30" {
		t.Fatalf("regular open = %s, want 09:30", got)
	}
	if got := dw.RegularClose.In(et).Format("15:04"); got != "16:00" {
		t.Fatalf("regular close = %s, want 16:00", got)
	}
	if got := dw.ExtendedClose.In(et).Format("15:04"); got != "20:00" {
		t.Fatalf("extended close = %s, want 20:00", got)
	}
	// The second row is an early close (13:00 ET, the day after Thanksgiving):
	// its extended window must end at 17:00, not at an assumed 20:00. A bot
	// that hardcoded the bounds would place after-hours orders into a session
	// that closed four hours earlier.
	early, err := raw[1].window(et)
	if err != nil {
		t.Fatalf("window on the half day: %v", err)
	}
	if got := early.RegularClose.In(et).Format("15:04"); got != "13:00" {
		t.Fatalf("half-day close = %s, want 13:00", got)
	}
	if got := early.ExtendedClose.In(et).Format("15:04"); got != "17:00" {
		t.Fatalf("half-day extended close = %s, want 17:00", got)
	}
}

func TestParseCalendarTimeAcceptsBothVendorFormats(t *testing.T) {
	ok := map[string][2]int{"0400": {4, 0}, "09:30": {9, 30}, "16:00": {16, 0}, "2000": {20, 0}, "0000": {0, 0}}
	for in, want := range ok {
		h, m, err := parseCalendarTime(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if h != want[0] || m != want[1] {
			t.Fatalf("%q -> %02d:%02d, want %02d:%02d", in, h, m, want[0], want[1])
		}
	}
	for _, bad := range []string{"", "9:30", "09300", "25:00", "09:60", "abcd", "09-30", "4:00"} {
		if _, _, err := parseCalendarTime(bad); err == nil {
			t.Fatalf("%q parsed without error", bad)
		}
	}
}

// The exchange's own zone, not a fixed offset: the open is 09:30 ET whether
// that is -04:00 or -05:00. A host without a zone database (the scratch API
// image, before time/tzdata was embedded) resolves 09:30 as EST in July, an
// hour early, and the bot would trade the first hour of the session believing
// it was the second.
func TestSessionsFollowTheRealZoneNotAnOffset(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	if _, off := time.Date(2026, 7, 15, 12, 0, 0, 0, ny).Zone(); off != -4*3600 {
		t.Fatalf("July offset = %d, want -14400 (EDT)", off)
	}
	srv, client := calendarServer(t, `[
	 {"close":"16:00","date":"2026-07-15","open":"09:30","session_close":"2000","session_open":"0400"},
	 {"close":"16:00","date":"2026-12-15","open":"09:30","session_close":"2000","session_open":"0400"}
	]`)
	defer srv.Close()
	for _, tc := range []struct {
		day, clock string
		want       Session
	}{
		{"2026-07-15", "09:29", SessionPreMarket},
		{"2026-07-15", "09:30", SessionRegular},
		{"2026-12-15", "09:29", SessionPreMarket},
		{"2026-12-15", "09:30", SessionRegular},
	} {
		now, err := time.ParseInLocation("2006-01-02 15:04", tc.day+" "+tc.clock, ny)
		if err != nil {
			t.Fatal(err)
		}
		s := &Sessions{Client: client, Now: func() time.Time { return now }, Location: ny}
		got, _, err := s.Current(context.Background())
		if err != nil {
			t.Fatalf("%s %s: %v", tc.day, tc.clock, err)
		}
		if got != tc.want {
			t.Fatalf("%s %s -> %v, want %v", tc.day, tc.clock, got, tc.want)
		}
	}
}

// A session is decided against the day's own bounds, including a half day:
// 18:00 ET on an early-close day is closed, not after-hours.
func TestSessionsUseTheDaysOwnBounds(t *testing.T) {
	srv, client := calendarServer(t, measuredCalendar)
	defer srv.Close()
	et := time.FixedZone("ET", -4*3600)
	at := func(day, hhmm string) time.Time {
		tt, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hhmm, et)
		if err != nil {
			t.Fatal(err)
		}
		return tt
	}
	cases := []struct {
		day, clock string
		want       Session
	}{
		{"2026-10-26", "09:29", SessionPreMarket},
		{"2026-10-26", "09:30", SessionRegular},
		{"2026-10-26", "19:59", SessionAfterHours},
		{"2026-10-26", "20:00", SessionClosed},
		// The half day: extended close is 17:00, so 18:00 is after the day.
		{"2026-11-27", "12:59", SessionRegular},
		{"2026-11-27", "13:30", SessionAfterHours},
		{"2026-11-27", "16:59", SessionAfterHours},
		{"2026-11-27", "17:00", SessionClosed},
		// A date the calendar omits is not a trading day at all.
		{"2026-10-25", "05:00", SessionClosed},
	}
	for _, tc := range cases {
		s := &Sessions{Client: client, Now: func() time.Time { return at(tc.day, tc.clock) }, Location: et}
		got, _, err := s.Current(context.Background())
		if err != nil {
			t.Fatalf("%s %s: %v", tc.day, tc.clock, err)
		}
		if got != tc.want {
			t.Fatalf("%s %s -> %v, want %v", tc.day, tc.clock, got, tc.want)
		}
	}
}
