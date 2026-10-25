package main

// sessions.go: `alpacaruns sessions` — what the market is doing and what that
// permits, answered by the same code the trading loop runs rather than by a
// second implementation someone will read as authoritative.
//
// It exists because every rest-of-hours rule is invisible in normal operation:
// an entry is refused with one line in a log that rolls. Here an operator can
// ask, before the session, whether the calendar agrees with the clock, how old
// the quote the bot would trade against is, and what the route decided.

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

func cmdSessions(args []string) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	envFile := fs.String("env", ".env", "path to .env file")
	symbols := fs.String("symbols", "", "comma-separated symbols to probe (default: the equity universe)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*envFile)
	if err != nil {
		log.Printf("config: %v", err)
		return 1
	}
	set, err := strategy.LoadSettings(strategy.OsGetenv, cfg.PollSeconds)
	if err != nil {
		log.Printf("strategy settings: %v", err)
		return 1
	}
	ctx := context.Background()
	client := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	// The venue guard is not decoration: this command reads the calendar and
	// the quote feed, and an operator pointing it at live keys would be
	// looking at live numbers while believing they were paper.
	if _, err := client.VerifyVenue(ctx, tools.ExpectPaperVenue(cfg.AlpacaBaseURL)); err != nil {
		fmt.Fprintf(os.Stderr, "venue: %v\n", err)
		return 1
	}
	syms := splitList(*symbols)
	if len(syms) == 0 {
		syms = append(append([]string{}, set.EquitySymbols...), set.ETFSymbols...)
	}

	sessions := &tools.Sessions{Client: client}
	sess, day, err := sessions.Current(ctx)
	if err != nil {
		fmt.Printf("session: UNKNOWN — the calendar could not be read (%v)\n", err)
		fmt.Printf("  every entry is refused until it can be: the gate fails closed on an unknown session\n")
		return 1
	}
	fmt.Printf("session: %s (trading day %s: %v)\n", sess, day.Date, day.Trading)
	if day.Trading {
		et := day.ExtendedOpen.Location()
		fmt.Printf("  extended open %s, regular open %s, regular close %s, extended close %s (%s)\n",
			day.ExtendedOpen.In(et).Format("15:04"), day.RegularOpen.In(et).Format("15:04"),
			day.RegularClose.In(et).Format("15:04"), day.ExtendedClose.In(et).Format("15:04"), et)
	} else {
		// The zero times on a non-trading day are not bounds to print: an
		// operator reading "04:00-20:00" off a Sunday would trust a session
		// the exchange never published.
		fmt.Printf("  no session published for %s (weekend or holiday): nothing executes, and an order placed now rests until the next real session\n", day.Date)
	}
	// Both clocks: the exchange's, because every bound above is expressed in
	// it, and the host's, because that is what this process actually reads.
	fmt.Printf("  now %s / host %s | EXTENDED_HOURS=%v windows=%q spread cap %.1fbps quote age %s\n",
		time.Now().UTC().Format("2006-01-02 15:04 UTC"), time.Now().Format("15:04 MST"),
		cfg.ExtendedHours, cfg.ExtendedHoursWindows,
		cfg.ExtendedSpreadCapBps(), cfg.MaxQuoteAge())

	extWin, err := newExtendedWindows(cfg)
	if err != nil {
		fmt.Printf("  EXTENDED_HOURS_WINDOWS unusable: %v — extended entries are refused\n", err)
	}
	l := &autoLoop{cfg: cfg, client: client, sessions: sessions, extWin: extWin,
		exec: &strategy.Executor{Client: client, Validator: nopGate{}}}

	fmt.Printf("\n%-10s %-10s %10s %10s %8s %10s  %s\n",
		"SYMBOL", "ROUTE", "BID", "ASK", "SPREAD", "AGE", "WHY")
	for _, sym := range syms {
		r := l.routeEntry(ctx, sym)
		bid, ask, spread, age := 0.0, 0.0, "-", "-"
		why := r.why
		if r.quote.Valid() {
			bid, ask = r.quote.Bid, r.quote.Ask
			spread = fmt.Sprintf("%.1fbps", r.quote.SpreadBps())
			age = time.Since(r.quote.At).Truncate(time.Second).String()
		} else if r.kind == routeBlocked {
			// A blocked route never got as far as a quote, and the mark is the
			// interesting half of the answer: on a Sunday this is where the
			// operator sees that the "market" the bot would have traded
			// against is a print from two days ago. A live route is not
			// refused for it, so nothing is appended there.
			if q, qerr := l.exec.ExtendedMark(ctx, sym, cfg.MaxQuoteAge()); qerr == nil {
				bid, ask = q.Bid, q.Ask
				spread = fmt.Sprintf("%.1fbps", q.SpreadBps())
				age = time.Since(q.At).Truncate(time.Second).String()
			} else {
				why = fmt.Sprintf("%s; mark: %s", why, qerr)
			}
		}
		if why == "" {
			why = "—"
		}
		fmt.Printf("%-10s %-10s %10.2f %10.2f %8s %10s  %s\n",
			sym, routeName(r.kind), bid, ask, spread, age, why)
	}
	if sess.Extended() {
		fmt.Printf("\nThe extended session is open. Positions entered NOW carry no broker stop:\n" +
			"Alpaca refuses extended_hours on bracket and OCO orders, so protection is the\n" +
			"monitor's next pass plus whatever the next regular session's pair covers.\n")
	}
	return 0
}

// routeName renders the entry route for the table.
func routeName(k entryKind) string {
	switch k {
	case routeLive:
		return "live"
	case routeExtended:
		return "EXTENDED"
	case routePreOrder:
		return "pre-order"
	default:
		return "blocked"
	}
}

// nopGate satisfies the executor's validator field for a command that never
// places an order: routeEntry asks for a quote, not an approval.
type nopGate struct{}

func (nopGate) Validate(risk.Proposal) risk.Verdict { return risk.Verdict{Approved: true} }
