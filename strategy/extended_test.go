package strategy

// extended_test.go pins the rest-of-hours rules the broker enforces, and the
// one ordering only this code enforces: an exit that cannot be priced must not
// have cancelled its protection on the way to failing. The shapes are measured
// against the paper API (see extended.go); these tests are what stop them
// drifting back into guesses.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// etTest is a fixed -04:00 zone. The calendar rows carry naive wall times, so
// pinning the offset keeps a session test from flipping when the host's zone
// data changes.
var etTest = time.FixedZone("ET", -4*3600)

// calendarRow is one /v2/calendar day in the shape the API returns.
func calendarRow(date, extOpen, open, close, extClose string) map[string]any {
	return map[string]any{
		"date": date, "session_open": extOpen, "open": open,
		"close": close, "session_close": extClose,
	}
}

// atWall is a clock reading at HH:MM on the test date, in the fixed zone.
func atWall(t *testing.T, hhmm string) time.Time {
	t.Helper()
	tt, err := time.ParseInLocation("2006-01-02 15:04", "2026-10-26 "+hhmm, etTest)
	if err != nil {
		t.Fatalf("bad clock %q: %v", hhmm, err)
	}
	return tt
}

// snapQuote builds a stock snapshot with a two-sided touch printed at at. The
// timestamp is the point: an extended order is only as honest as the print it
// is priced from.
func snapQuote(bid, ask float64, at time.Time) map[string]any {
	return map[string]any{
		"latestQuote": map[string]any{"bp": bid, "ap": ask, "t": at.UTC().Format(time.RFC3339Nano)},
		"latestTrade": map[string]any{"p": (bid + ask) / 2, "t": at.UTC().Format(time.RFC3339Nano)},
	}
}

// fresh is "printed just now", which is what a live session looks like from
// the executor's point of view.
func fresh() time.Time { return time.Now().UTC().Add(-2 * time.Second) }

func TestExtendedMarkFreshAndStale(t *testing.T) {
	x, broker, _, _, _ := newExitSetup(t, nil)
	broker.snap = snapQuote(347.00, 347.10, fresh())

	q, err := x.ExtendedMark(context.Background(), "AAPL", 5*time.Minute)
	if err != nil {
		t.Fatalf("fresh quote refused: %v", err)
	}
	if q.Bid != 347.00 || q.Ask != 347.10 {
		t.Fatalf("touch = %.2f/%.2f", q.Bid, q.Ask)
	}
	if age := time.Since(q.At); age > time.Minute {
		t.Fatalf("quote stamp %s read as %s old", q.At, age)
	}

	// A print from a previous session is not a market: measured on a Sunday, a
	// weekend snapshot carried Friday's 20:00:03Z close and a 10% spread.
	broker.snap = snapQuote(347.00, 347.10, time.Now().UTC().Add(-6*time.Hour))
	if _, err := x.ExtendedMark(context.Background(), "AAPL", 5*time.Minute); err == nil {
		t.Fatal("stale quote accepted")
	} else if !strings.Contains(err.Error(), "old") {
		t.Fatalf("stale refusal should name the age, got %v", err)
	}

	// A stamp in the future is not freshness: time.Since goes negative and a
	// plain age comparison would read it as infinitely new.
	broker.snap = snapQuote(347.00, 347.10, time.Now().UTC().Add(time.Hour))
	if _, err := x.ExtendedMark(context.Background(), "AAPL", 5*time.Minute); err == nil ||
		!strings.Contains(err.Error(), "future") {
		t.Fatalf("future-dated quote accepted: %v", err)
	}

	// One-sided or crossed is unusable.
	broker.snap = map[string]any{"latestQuote": map[string]any{
		"bp": 0.0, "ap": 347.10, "t": fresh().UTC().Format(time.RFC3339)}}
	if _, err := x.ExtendedMark(context.Background(), "AAPL", 5*time.Minute); err == nil {
		t.Fatal("one-sided quote accepted")
	}
	broker.snap = snapQuote(347.90, 347.10, fresh()) // crossed
	if _, err := x.ExtendedMark(context.Background(), "AAPL", 5*time.Minute); err == nil {
		t.Fatal("crossed quote accepted")
	}

	// Crypto and options have no extended session at all.
	if _, err := x.ExtendedMark(context.Background(), "BTC/USD", 5*time.Minute); err == nil ||
		!strings.Contains(err.Error(), "24/7") {
		t.Fatalf("crypto should be refused as 24/7, got %v", err)
	}
	if _, err := x.ExtendedMark(context.Background(), "AAPL240119C00100000", 5*time.Minute); err == nil ||
		!strings.Contains(err.Error(), "option") {
		t.Fatalf("options should be refused, got %v", err)
	}

	// A snapshot endpoint that does not answer is a refusal, not a zero price.
	broker.snap = nil
	if _, err := x.ExtendedMark(context.Background(), "AAPL", 5*time.Minute); err == nil {
		t.Fatal("missing quote source accepted")
	}
}

// TestExecuteExtendedEntryShape is the measurement turned into a test: a
// marketable limit, tif=day, extended_hours=true, and NO bracket envelope —
// because the broker refuses the pair, and the code must not rediscover that
// at the wire every tick.
func TestExecuteExtendedEntryShape(t *testing.T) {
	x, broker, gate, _, st := newExitSetup(t, nil)
	at := fresh()
	broker.snap = snapQuote(347.00, 347.20, at)
	q := mustMark(t, x, "AAPL", 5*time.Minute)
	plan := EntryPlan{Symbol: "AAPL", Qty: 4, Price: 347.10, Budget: 4 * 347.10,
		Brackets: BracketLevels{Entry: 347.10, TakeProfit: 360.00, StopLoss: 335.00}, Score: 0.72}
	if err := x.ExecuteExtendedEntry(context.Background(), plan, q, 0.9); err != nil {
		t.Fatalf("extended entry: %v", err)
	}
	if broker.posted != 1 {
		t.Fatalf("posted %d orders, want 1", broker.posted)
	}
	body := broker.bodies[0]
	if body["type"] != "limit" || body["time_in_force"] != "day" {
		t.Fatalf("shape = %v/%v, want limit/day", body["type"], body["time_in_force"])
	}
	if ext, _ := body["extended_hours"].(bool); !ext {
		t.Fatalf("extended_hours missing from the wire body: %v", body)
	}
	for _, k := range []string{"order_class", "take_profit", "stop_loss"} {
		if _, ok := body[k]; ok {
			t.Fatalf("extended entry carried %q; the broker refuses protection on this order", k)
		}
	}
	lp, err := strconv.ParseFloat(body["limit_price"].(string), 64)
	if err != nil {
		t.Fatalf("limit_price %v: %v", body["limit_price"], err)
	}
	if lp <= 347.20 {
		t.Fatalf("limit %.4f does not cross the 347.20 ask", lp)
	}
	if len(gate.got) != 1 {
		t.Fatalf("gate saw %d proposals", len(gate.got))
	}
	p := gate.got[0]
	if !p.ExtendedHours || p.OrderType != "limit" || p.TimeInForce != "day" || p.OrderClass != "simple" {
		t.Fatalf("gate proposal does not describe the order: %+v", p)
	}
	// The levels ride to state so the monitor's protect pass rests the
	// regular-session pair at the prices the strategy chose.
	lv := st.Level("AAPL", "")
	if lv == nil || lv.TakeProfit != 360.00 || lv.StopLoss != 335.00 {
		t.Fatalf("levels not recorded for protection: %+v", lv)
	}
}

// The plan was sized at its mark. If the session has moved away from it, the
// trade the factors approved is not the trade on offer.
func TestExecuteExtendedEntryRefusesWhenPriceMoved(t *testing.T) {
	x, broker, _, _, _ := newExitSetup(t, nil)
	broker.snap = snapQuote(352.00, 352.20, fresh())
	q := mustMark(t, x, "AAPL", 5*time.Minute)
	plan := EntryPlan{Symbol: "AAPL", Qty: 4, Price: 347.10, Budget: 4 * 347.10,
		Brackets: BracketLevels{TakeProfit: 360, StopLoss: 335}}
	err := x.ExecuteExtendedEntry(context.Background(), plan, q, 0.9)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("want a price-move refusal, got %v", err)
	}
	if broker.posted != 0 {
		t.Fatal("placed an order after refusing the price")
	}
}

// Sizing follows the price actually paid, not the price the plan imagined: the
// dollar budget the gate approved is the dollar budget spent.
func TestExecuteExtendedEntryResizesToBudget(t *testing.T) {
	x, broker, _, _, _ := newExitSetup(t, nil)
	broker.snap = snapQuote(347.00, 347.20, fresh())
	q := mustMark(t, x, "AAPL", 5*time.Minute)
	// Budget for 4 shares at 347.10 = 1388.40; at a ~347.37 limit that is 3.
	plan := EntryPlan{Symbol: "AAPL", Qty: 4, Price: 347.10, Budget: 1388.40,
		Brackets: BracketLevels{TakeProfit: 360, StopLoss: 335}}
	if err := x.ExecuteExtendedEntry(context.Background(), plan, q, 0.9); err != nil {
		t.Fatalf("extended entry: %v", err)
	}
	if got := broker.bodies[0]["qty"]; got != "3" {
		t.Fatalf("qty = %v, want 3 (budget 1388.40 at a marketable limit)", got)
	}
}

// A budget that cannot buy one share at the session price is a refusal, not a
// zero-quantity order the broker will bounce.
func TestExecuteExtendedEntryRefusesSubShareBudget(t *testing.T) {
	x, broker, _, _, _ := newExitSetup(t, nil)
	broker.snap = snapQuote(347.00, 347.20, fresh())
	q := mustMark(t, x, "AAPL", 5*time.Minute)
	plan := EntryPlan{Symbol: "AAPL", Qty: 0, Price: 347.10, Budget: 50,
		Brackets: BracketLevels{TakeProfit: 360, StopLoss: 335}}
	if err := x.ExecuteExtendedEntry(context.Background(), plan, q, 0.9); err == nil ||
		!strings.Contains(err.Error(), "whole share") {
		t.Fatalf("want a cannot-buy-one-share refusal, got %v", err)
	}
	if broker.posted != 0 {
		t.Fatal("placed a zero-quantity order")
	}
}

// Options and crypto never reach this path, even if a caller tries.
func TestExecuteExtendedEntryRefusesNonEquities(t *testing.T) {
	x, broker, _, _, _ := newExitSetup(t, nil)
	broker.snap = snapQuote(347.00, 347.20, fresh())
	q := mustMark(t, x, "AAPL", 5*time.Minute)
	for _, sym := range []string{"BTC/USD", "AAPL240119C00100000"} {
		plan := EntryPlan{Symbol: sym, Qty: 1, Price: 347.10, Budget: 347.10,
			Brackets: BracketLevels{TakeProfit: 360, StopLoss: 335}, Crypto: strings.Contains(sym, "/")}
		err := x.ExecuteExtendedEntry(context.Background(), plan, q, 0.9)
		if err == nil {
			t.Fatalf("%s accepted an extended entry", sym)
		}
	}
	if broker.posted != 0 {
		t.Fatal("placed an order after refusing it")
	}
}

// TestExtendedExitShapes pins how a protective exit is expressed per session.
func TestExtendedExitShapes(t *testing.T) {
	row := calendarRow("2026-10-26", "0400", "09:30", "16:00", "2000")
	cases := []struct {
		name      string
		clock     string
		wantType  string
		wantTIF   string
		wantExt   bool
		wantPrice bool
	}{
		{"regular session is a market order", "10:00", "market", "gtc", false, false},
		{"pre-market crosses the bid as a limit", "05:00", "limit", "day", true, true},
		{"after-hours crosses the bid as a limit", "17:00", "limit", "day", true, true},
		// Between sessions nothing can execute. Resting a market order until
		// the bell is NOT the fallback: the exit path cancels protection
		// first, so a resting sell would face the open with no floor.
		{"night keeps the plain shape and does not act", "21:00", "market", "gtc", false, false},
		{"before the extended open keeps the plain shape", "03:00", "market", "gtc", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := atWall(t, tc.clock)
			x, broker, _, _, _ := newExitSetup(t, []map[string]any{pos("AAPL", "5", "347")})
			broker.calendar = []map[string]any{row}
			broker.snap = snapQuote(347.00, 347.20, fresh())
			x.ExtendedExits = &ExtendedExit{
				Sessions:    &tools.Sessions{Client: x.Client, Now: func() time.Time { return now }, Location: etTest},
				MaxQuoteAge: 5 * time.Minute,
			}
			req, extended, err := x.exitShape(context.Background(), "AAPL", "sell", "5")
			if err != nil {
				t.Fatalf("exitShape: %v", err)
			}
			if req.Type != tc.wantType || req.TimeInForce != tc.wantTIF || extended != tc.wantExt {
				t.Fatalf("shape = %s/%s extended=%v, want %s/%s extended=%v",
					req.Type, req.TimeInForce, extended, tc.wantType, tc.wantTIF, tc.wantExt)
			}
			if tc.wantPrice {
				if req.LimitPrice == "" || !req.ExtendedHours {
					t.Fatalf("extended exit needs a marketable limit: %+v", req)
				}
				p, _ := strconv.ParseFloat(req.LimitPrice, 64)
				if p >= 347.00 {
					t.Fatalf("sell limit %.4f does not cross the 347.00 bid", p)
				}
			} else if req.ExtendedHours {
				t.Fatalf("non-extended exit carries extended_hours: %+v", req)
			}
		})
	}
}

// The ordering only we enforce: an extended exit whose quote cannot be read
// must fail BEFORE the protective pair is cancelled. Cancel-first would strip
// the floor to buy an exit that never happens.
func TestExecuteReduceRefusesBeforeCancellingProtection(t *testing.T) {
	now := atWall(t, "17:00")
	x, broker, _, _, _ := newExitSetup(t, []map[string]any{pos("AAPL", "5", "347")})
	broker.calendar = []map[string]any{calendarRow("2026-10-26", "0400", "09:30", "16:00", "2000")}
	// No snapshot at all: the data endpoint 404s, so the exit cannot be priced.
	x.ExtendedExits = &ExtendedExit{
		Sessions:    &tools.Sessions{Client: x.Client, Now: func() time.Time { return now }, Location: etTest},
		MaxQuoteAge: 5 * time.Minute,
	}
	broker.resting = []map[string]any{{
		"id": "prot-1", "symbol": "AAPL", "side": "sell", "qty": "5",
		"status": "accepted", "order_class": "oco", "group_role": "take_profit",
	}}
	done, err := x.ExecuteReduce(context.Background(), "AAPL", "stop breach")
	if err == nil {
		t.Fatal("exit succeeded without a price")
	}
	if done {
		t.Fatal("reported an exit that never happened")
	}
	for _, ev := range broker.log() {
		if strings.HasPrefix(ev, "DELETE") {
			t.Fatalf("protection was cancelled before the failed exit: %v", broker.log())
		}
	}
	if !strings.Contains(err.Error(), "before touching protection") {
		t.Fatalf("error should name the ordering it protected, got %v", err)
	}
}

// A working extended exit: protection cleared, then a marketable extended
// limit sold — the sequence the broker's refusals force.
func TestExecuteReduceExtendedExitCancelsThenSellsLimit(t *testing.T) {
	now := atWall(t, "17:00")
	x, broker, gate, _, _ := newExitSetup(t, []map[string]any{pos("AAPL", "5", "347")})
	broker.calendar = []map[string]any{calendarRow("2026-10-26", "0400", "09:30", "16:00", "2000")}
	broker.snap = snapQuote(347.00, 347.20, fresh())
	x.ExtendedExits = &ExtendedExit{
		Sessions:    &tools.Sessions{Client: x.Client, Now: func() time.Time { return now }, Location: etTest},
		MaxQuoteAge: 5 * time.Minute,
	}
	broker.resting = []map[string]any{{
		"id": "prot-1", "symbol": "AAPL", "side": "sell", "qty": "5",
		"status": "accepted", "order_class": "oco", "group_role": "take_profit",
	}}
	if _, err := x.ExecuteReduce(context.Background(), "AAPL", "stop breach"); err != nil {
		t.Fatalf("ExecuteReduce: %v", err)
	}
	events := broker.log()
	deleteAt, postAt := -1, -1
	for i, ev := range events {
		if strings.HasPrefix(ev, "DELETE") && deleteAt < 0 {
			deleteAt = i
		}
		if strings.HasPrefix(ev, "POST") && postAt < 0 {
			postAt = i
		}
	}
	if deleteAt < 0 || postAt < 0 || deleteAt > postAt {
		t.Fatalf("want cancel-then-exit, got %v", events)
	}
	body := broker.bodies[0]
	if body["type"] != "limit" || body["time_in_force"] != "day" {
		t.Fatalf("extended exit shape = %v/%v", body["type"], body["time_in_force"])
	}
	if ext, _ := body["extended_hours"].(bool); !ext {
		t.Fatal("extended exit missing extended_hours")
	}
	// The gate must see the shape it is approving: an approval of a market
	// order that bought a limit order is an approval nobody can audit.
	if len(gate.got) == 0 || !gate.got[len(gate.got)-1].ExtendedHours {
		t.Fatalf("gate proposal did not carry the extended shape: %+v", gate.got)
	}
}

// An unreadable calendar is not a licence to guess: exitShape keeps the
// regular-session shape and lets the risk gate refuse, rather than this layer
// inventing a session.
func TestExtendedExitUnknownSessionKeepsPlainShape(t *testing.T) {
	now := atWall(t, "17:00")
	x, broker, _, _, _ := newExitSetup(t, []map[string]any{pos("AAPL", "5", "347")})
	broker.calendar = nil // 404 on /calendar
	x.ExtendedExits = &ExtendedExit{
		Sessions:    &tools.Sessions{Client: x.Client, Now: func() time.Time { return now }, Location: etTest},
		MaxQuoteAge: 5 * time.Minute,
	}
	req, extended, err := x.exitShape(context.Background(), "AAPL", "sell", "5")
	if err != nil {
		t.Fatalf("unknown session should not error at this layer: %v", err)
	}
	if extended || req.Type != "market" {
		t.Fatalf("unknown session produced %+v extended=%v", req, extended)
	}
}

// A holiday is answered by the calendar, not the weekday: Tuesday 05:00 with
// no calendar row is closed, and nothing may be placed into it.
func TestSessionsHolidayIsClosed(t *testing.T) {
	now := atWall(t, "05:00")
	x, broker, _, _, _ := newExitSetup(t, nil)
	broker.calendar = []map[string]any{} // the exchange published no day
	s := &tools.Sessions{Client: x.Client, Now: func() time.Time { return now }, Location: etTest}
	sess, day, err := s.Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if sess != tools.SessionClosed {
		t.Fatalf("holiday session = %v, want closed", sess)
	}
	if day.Trading {
		t.Fatal("a day absent from the calendar reported Trading=true")
	}
}

// TestSessionsBoundaries walks one day's four states plus the edges, against a
// calendar row the paper API actually returned (04:00 / 09:30 / 16:00 / 20:00).
func TestSessionsBoundaries(t *testing.T) {
	row := calendarRow("2026-10-26", "0400", "09:30", "16:00", "2000")
	cases := []struct {
		clock string
		want  tools.Session
	}{
		{"03:59", tools.SessionClosed},
		{"04:00", tools.SessionPreMarket},
		{"09:29", tools.SessionPreMarket},
		{"09:30", tools.SessionRegular},
		{"15:59", tools.SessionRegular},
		{"16:00", tools.SessionAfterHours},
		{"19:59", tools.SessionAfterHours},
		{"20:00", tools.SessionClosed},
		{"23:30", tools.SessionClosed},
	}
	for _, tc := range cases {
		t.Run(tc.clock, func(t *testing.T) {
			now := atWall(t, tc.clock)
			x, broker, _, _, _ := newExitSetup(t, nil)
			broker.calendar = []map[string]any{row}
			s := &tools.Sessions{Client: x.Client, Now: func() time.Time { return now }, Location: etTest}
			sess, _, err := s.Current(context.Background())
			if err != nil {
				t.Fatalf("Current: %v", err)
			}
			if sess != tc.want {
				t.Fatalf("%s -> %v, want %v", tc.clock, sess, tc.want)
			}
		})
	}
}

// A calendar outage reports unknown rather than closed: "we could not read it"
// and "it is shut" lead to different behaviour in the route, and collapsing
// them would either stop the bot on a blip or trade it on a holiday.
func TestSessionsCalendarOutageIsUnknown(t *testing.T) {
	now := atWall(t, "10:00")
	x, broker, _, _, _ := newExitSetup(t, nil)
	broker.calendar = nil
	s := &tools.Sessions{Client: x.Client, Now: func() time.Time { return now }, Location: etTest}
	sess, _, err := s.Current(context.Background())
	if err == nil {
		t.Fatal("outage reported no error")
	}
	if sess != tools.SessionUnknown {
		t.Fatalf("session = %v, want unknown", sess)
	}
}

// TestSessionClockFailsClosed pins the gate adapter: an unreadable calendar
// must not answer "market open" the way the old clock adapter did, and a
// readable one must answer each question the calendar says.
func TestSessionClockFailsClosed(t *testing.T) {
	x, broker, _, _, _ := newExitSetup(t, nil)
	now := atWall(t, "05:00")
	s := &tools.Sessions{Client: x.Client, Now: func() time.Time { return now }, Location: etTest}
	c := risk.SessionClock(s)
	if c.MarketOpen() {
		t.Fatal("unreadable calendar reported an open market")
	}
	if c.SessionOpen(risk.Proposal{Symbol: "AAPL"}) {
		t.Fatal("unreadable calendar reported an open session")
	}
	if c.SessionKnown() {
		t.Fatal("unreadable calendar claimed to know the session")
	}

	broker.calendar = []map[string]any{calendarRow("2026-10-26", "0400", "09:30", "16:00", "2000")}
	// A fresh source: the outage above is remembered briefly on purpose, and
	// this case is about the answer once the calendar can be read.
	c2 := risk.SessionClock(&tools.Sessions{Client: x.Client, Now: func() time.Time { return now }, Location: etTest})
	if c2.MarketOpen() {
		t.Fatal("05:00 is not the regular session")
	}
	if !c2.SessionOpen(risk.Proposal{Symbol: "AAPL"}) {
		t.Fatal("05:00 on a trading day is the extended session")
	}
	if !c2.SessionKnown() {
		t.Fatal("a readable calendar should be known")
	}
}

func mustMark(t *testing.T, x *Executor, symbol string, age time.Duration) ExtendedQuote {
	t.Helper()
	q, err := x.ExtendedMark(context.Background(), symbol, age)
	if err != nil {
		t.Fatalf("mark %s: %v", symbol, err)
	}
	return q
}
