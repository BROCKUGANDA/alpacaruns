// Server-mode helpers for 24/7 operation: P/L reporting (pl subcommand),
// trade-journal lifecycle, graceful shutdown and restart reconciliation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/options"
	"google.golang.org/adk/agent"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// openJournal opens the configured trade log, creating it on first use.
func openJournal(cfg *config.Config) (*pnl.Journal, error) {
	return pnl.Open(cfg.TradeLog)
}

// backfillFills pulls recent closed orders from Alpaca and journals any
// fills not already recorded. This makes the local log self-healing: even
// if a cycle crashed between placement and journaling, the next pl/monitor
// run recovers the fills from the broker.
func backfillFills(ctx context.Context, c *tools.Client, j *pnl.Journal, since time.Time) (int, error) {
	orders, err := c.ListOrders(ctx, "closed", 500)
	if err != nil {
		return 0, fmt.Errorf("list orders: %w", err)
	}
	n := 0
	for _, o := range orders {
		if o.FilledQty == "" || o.FilledQty == "0" || j.KnownOrder(o.ID) {
			continue
		}
		price := strings.TrimSpace(o.FilledAvgPrice)
		if price == "" || price == "0" {
			continue // unfilled or missing mark; skip rather than poison the book
		}
		ts := o.CreatedAt
		if !since.IsZero() && ts.Before(since) {
			continue
		}
		rec := pnl.Record{
			Kind:    pnl.KindFill,
			OrderID: o.ID,
			Symbol:  o.Symbol,
			Side:    strings.ToLower(o.Side),
			Qty:     o.FilledQty,
			Price:   price,
			Status:  o.Status,
			TS:      ts,
		}
		if err := j.Append(rec); err != nil {
			return n, fmt.Errorf("append backfill: %w", err)
		}
		n++
	}
	return n, nil
}

// reconcile compares local FIFO books against live Alpaca positions,
// journals the snapshot plus any drift, and returns the drift list.
func reconcile(ctx context.Context, c *tools.Client, j *pnl.Journal) ([]pnl.Drift, error) {
	fills, err := j.Fills(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("read trade log: %w", err)
	}
	st := pnl.Compute(fills)
	local := map[string]float64{}
	for _, s := range st.Symbols {
		local[s.Symbol] = s.OpenQty
	}
	pos, err := c.GetPositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("get positions: %w", err)
	}
	broker := map[string]float64{}
	brokerRaw := map[string]string{}
	for _, p := range pos {
		q, _ := strconv.ParseFloat(p.Qty, 64)
		side := 1.0
		if p.Side == "short" {
			side = -1
		}
		broker[p.Symbol] += side * math.Abs(q)
		brokerRaw[p.Symbol] = fmt.Sprintf("%s@%s", p.Qty, p.AvgEntry)
	}
	drift := pnl.ComparePositions(local, broker)
	acct, err := c.GetAccount(ctx)
	equity := ""
	if err == nil && acct != nil {
		equity = acct.Equity
	}
	rec := pnl.Record{
		Kind:   pnl.KindReconcile,
		Broker: brokerRaw,
		Drift:  driftLines(drift),
		Equity: equity,
		Source: "boot",
	}
	if len(local) > 0 || len(broker) > 0 {
		b, _ := json.Marshal(local)
		rec.Detail = string(b)
	}
	if err := j.Append(rec); err != nil {
		return drift, fmt.Errorf("append reconcile: %w", err)
	}
	return drift, nil
}

func driftLines(drift []pnl.Drift) []string {
	var out []string
	for _, d := range drift {
		out = append(out, fmt.Sprintf("%s: local %.4f vs broker %.4f", d.Symbol, d.LocalQty, d.BrokerQty))
	}
	return out
}

// cmdTrade implements `alpacaruns trade`: a deterministic manual one-shot
// order that goes through the exact same risk validator as the LLM path
// (agents.NewValidator) but bypasses the agent graph entirely.
func cmdTrade(args []string) int {
	p, code := parseTradeArgs(args)
	if code != 0 {
		return code
	}

	cfg, err := config.Load(p.envFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trade: config: %v\n", err)
		return 1
	}

	symbol := strings.ToUpper(strings.TrimSpace(p.symbol))
	if p.occ != "" {
		symbol = strings.ToUpper(strings.TrimSpace(p.occ))
	}
	isOption := tools.IsOCCSymbol(symbol)
	side := strings.ToLower(strings.TrimSpace(p.side))
	orderType := "market"
	limit := strings.TrimSpace(p.limit)
	if limit != "" {
		orderType = "limit"
		if f, err := strconv.ParseFloat(limit, 64); err != nil || f <= 0 {
			fmt.Fprintf(os.Stderr, "trade: invalid --limit %q\n", limit)
			return 2
		}
	}
	tif := strings.ToLower(strings.TrimSpace(p.tif))
	if tif == "" {
		tif = "gtc"
	}
	switch tif {
	case "gtc", "day", "ioc", "fop":
	default:
		fmt.Fprintf(os.Stderr, "trade: invalid --tif %q (want gtc|day|ioc|fop)\n", tif)
		return 2
	}
	if isOption {
		switch tif {
		case "day", "gtc":
		default:
			fmt.Fprintf(os.Stderr, "trade: options orders require --tif day|gtc, got %q\n", tif)
			return 2
		}
	}
	switch side {
	case "buy", "sell":
	default:
		fmt.Fprintf(os.Stderr, "trade: invalid --side %q (want buy|sell)\n", side)
		return 2
	}
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "trade: --symbol (or --occ) is required")
		return 2
	}
	q := strings.TrimSpace(p.qty)
	n := strings.TrimSpace(p.notional)
	if isOption {
		if n != "" {
			fmt.Fprintln(os.Stderr, "trade: --notional is not valid for options; use --qty (contracts)")
			return 2
		}
		if p.extHours {
			fmt.Fprintln(os.Stderr, "trade: --extended-hours is not valid for options")
			return 2
		}
		if q == "" {
			fmt.Fprintln(os.Stderr, "trade: --qty (whole contracts) is required for options")
			return 2
		}
		if fq, err := strconv.ParseFloat(q, 64); err != nil || fq <= 0 || fq != math.Trunc(fq) {
			fmt.Fprintf(os.Stderr, "trade: options --qty must be a positive whole number of contracts, got %q\n", q)
			return 2
		}
	} else if (q == "") == (n == "") {
		fmt.Fprintln(os.Stderr, "trade: exactly one of --qty or --notional is required")
		return 2
	}

	ctx := context.Background()
	c := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	c.Log = func(format string, a ...any) { slog.Debug(fmt.Sprintf(format, a...)) }

	// Same gate as the agent path: kill switch + caps + session rules.
	val := agents.NewValidator(agents.NewKillSwitch(), cfg)
	conf := 1.0 // manual override: confidence is the operator's
	acct, err := c.GetAccount(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trade: account unavailable: %v\n", err)
		return 1
	}
	equity, _ := strconv.ParseFloat(acct.Equity, 64)
	pfv, _ := strconv.ParseFloat(acct.PortfolioValue, 64)
	pf := risk.Portfolio{Equity: equity, PortfolioValue: pfv}
	posVal := map[string]float64{}
	if positions, err := c.GetPositions(ctx); err == nil {
		for _, p := range positions {
			qv, _ := strconv.ParseFloat(p.Qty, 64)
			pv, _ := strconv.ParseFloat(p.CurrentPrice, 64)
			posVal[p.Symbol] = math.Abs(qv * pv)
		}
	}
	val.Portfolio = func() (risk.Portfolio, error) { return pf, nil }
	val.Positions = func(sym string) float64 { return posVal[sym] }
	// Options need a live mark to compute premium outlay (qty x100 x price);
	// the validator's default PriceSource already does this via Alpaca
	// snapshots and fails closed.
	val = val.WithContext(ctx)
	verdict := val.Validate(risk.Proposal{
		Symbol:      symbol,
		Side:        side,
		Qty:         q,
		Notional:    n,
		Confidence:  &conf,
		OrderType:   orderType,
		TimeInForce: tif,
	})
	fmt.Printf("risk gate: %s\n", verdict.String())
	if !verdict.Approved {
		return 1
	}

	req := tools.OrderRequest{
		Symbol:        symbol,
		Side:          side,
		Type:          orderType,
		TimeInForce:   tif,
		LimitPrice:    limit,
		ExtendedHours: p.extHours,
	}
	if q != "" {
		req.Qty = q
	} else {
		req.Notional = n
	}
	o, err := c.PlaceOrder(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trade: place order: %v\n", err)
		return 1
	}

	// Journal the decision and the resulting fill like every other path.
	j, err := openJournal(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trade: journal unavailable: %v (order %s placed)\n", err, o.ID)
	} else {
		defer j.Close()
		detail := fmt.Sprintf("manual trade %s %s %s%s @%s tif=%s",
			symbol, side, reqDisplayQty(q), n, limit, tif)
		if p.extHours {
			detail += " extended_hours=true"
		}
		if isOption {
			detail += " option contracts (multiplier 100)"
		}
		_ = j.Append(pnl.Record{
			Kind: pnl.KindDecision, Source: "cli:trade", Risk: "APPROVED",
			Confidence: &conf, Detail: detail,
		})
	}
	fmt.Printf("order placed: id=%s status=%s\n", o.ID, o.Status)
	return 0
}

// tradeArgs is the parsed flag set of the `trade` subcommand.
type tradeArgs struct {
	envFile  string
	symbol   string
	occ      string
	side     string
	qty      string
	limit    string
	tif      string
	notional string
	extHours bool
}

// parseTradeArgs parses trade subcommand flags without touching config,
// network or journal so it can be table-tested. Returns a nonzero exit
// code on flag syntax or positional-argument errors.
func parseTradeArgs(args []string) (tradeArgs, int) {
	var p tradeArgs
	fs := flag.NewFlagSet("trade", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&p.envFile, "env", ".env", "path to .env file")
	fs.StringVar(&p.symbol, "symbol", "", "ticker symbol, or an OCC option symbol")
	fs.StringVar(&p.occ, "occ", "", "OCC option contract symbol (overrides --symbol)")
	fs.StringVar(&p.side, "side", "", "buy | sell (required)")
	fs.StringVar(&p.qty, "qty", "", "share quantity (or --notional); contracts for options")
	fs.StringVar(&p.limit, "limit", "", "limit price (implies type=limit)")
	fs.StringVar(&p.tif, "tif", "", "time in force: gtc | day | ioc | fop (default: gtc; options: day|gtc only)")
	fs.BoolVar(&p.extHours, "extended-hours", false, "request extended-hours execution (equities only; limit + day/gtc)")
	fs.StringVar(&p.notional, "notional", "", "notional USD value (alternative to --qty; equities only)")
	if err := fs.Parse(args); err != nil {
		return p, 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "trade: unexpected positional argument %q\n", fs.Arg(0))
		return p, 2
	}
	return p, 0
}

// reqDisplayQty renders the qty part of the trade audit line.
func reqDisplayQty(q string) string {
	if q == "" {
		return ""
	}
	return q + " "
}

// cmdPLWrapper loads config then runs the pl report.
func cmdPLWrapper(args []string) int {
	var envFile string
	fs := flag.NewFlagSet("pl", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&envFile, "env", ".env", "path to .env file")
	fs.Bool("json", false, "print stats as JSON instead of a table") // declared so --json passes through to cmdPL
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(envFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pl: config: %v\n", err)
		return 1
	}
	return cmdPL(context.Background(), cfg, args)
}

// cmdPL implements `alpacaruns pl`.
func cmdPL(ctx context.Context, cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("pl", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonFlag := fs.Bool("json", false, "print stats as JSON instead of a table")
	sinceFlag := fs.String("since", "", "P/L window start (YYYY-MM-DD or RFC3339); default from PL_SINCE env")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sinceStr := *sinceFlag
	if sinceStr == "" {
		sinceStr = os.Getenv("PL_SINCE")
	}
	since, err := pnl.ParseSince(sinceStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pl: %v\n", err)
		return 2
	}

	j, err := openJournal(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pl: %v\n", err)
		return 1
	}
	defer j.Close()

	c := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	c.Log = func(format string, args ...any) { slog.Debug(fmt.Sprintf(format, args...)) }

	// Backfill fills from the broker so numbers are current even if a
	// previous cycle died mid-flight.
	backfilled, err := backfillFills(ctx, c, j, time.Time{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pl: warning: backfill failed: %v\n", err)
	} else if backfilled > 0 {
		fmt.Printf("backfilled %d fill(s) from Alpaca into %s\n", backfilled, j.Path())
	}

	fills, err := j.Fills(since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pl: reading trade log: %v\n", err)
		return 1
	}
	st := pnl.Compute(fills)

	acct, err := c.GetAccount(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pl: account unreachable (%v)\n\n", err)
		printStats(st, "", j.Path(), since)
		return 1
	}

	pos, err := c.GetPositions(ctx)
	unrealized := 0.0
	if err == nil {
		for _, p := range pos {
			qty, _ := strconv.ParseFloat(p.Qty, 64)
			if p.Side == "short" {
				qty = -math.Abs(qty)
			}
			entry, _ := strconv.ParseFloat(p.AvgEntry, 64)
			cur, _ := strconv.ParseFloat(p.CurrentPrice, 64)
			u := pnl.Unrealized(qty, entry, cur)
			unrealized += u
			fmt.Printf("position %-6s qty %-8s entry %-10s mark %-10s unrealized %+9.2f\n",
				p.Symbol, p.Qty, p.AvgEntry, p.CurrentPrice, u)
		}
	} else {
		fmt.Fprintf(os.Stderr, "pl: positions unreachable (%v)\n", err)
	}

	if *jsonFlag {
		out := struct {
			pnl.Stats
			Equity     string  `json:"equity"`
			Unrealized float64 `json:"unrealized"`
			Since      string  `json:"since"`
		}{st, acct.Equity, unrealized, sinceLabel(since)}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "pl: encode json: %v\n", err)
			return 1
		}
		return 0
	}

	printStats(st, acct.Equity, j.Path(), since)
	fmt.Printf("\nunrealized (live marks): %+.2f\n", unrealized)
	fmt.Printf("total (realized+unrealized since %s): %+.2f\n",
		sinceLabel(since), st.Realized+unrealized)
	return 0
}

func printStats(st pnl.Stats, equity, logPath string, since time.Time) {
	fmt.Printf("trade log:            %s\n", logPath)
	fmt.Printf("window since:         %s\n", sinceLabel(since))
	fmt.Printf("equity:               %s\n", orDash(equity))
	fmt.Printf("fills replayed:       %d\n", st.FillCount)
	if st.ParseErrs > 0 {
		fmt.Printf("skipped bad records:  %d\n", st.ParseErrs)
	}
	fmt.Printf("realized P/L:         %+.2f\n", st.Realized)
	closed := st.Wins + st.Losses
	if closed > 0 {
		fmt.Printf("win rate:             %.1f%% (%dW / %dL of %d closed trades)\n",
			100*st.WinRate, st.Wins, st.Losses, closed)
	} else {
		fmt.Printf("win rate:             n/a (no closed trades in window)\n")
	}
	if len(st.Symbols) > 0 {
		fmt.Println("\nper-symbol breakdown:")
		fmt.Printf("  %-8s %12s %6s %6s %10s %8s\n", "symbol", "realized", "wins", "losses", "open_qty", "avg_cost")
		for _, s := range st.Symbols {
			open := "-"
			if s.OpenQty != 0 {
				open = fmt.Sprintf("%.4f", s.OpenQty)
			}
			avg := "-"
			if s.AvgCost != 0 {
				avg = fmt.Sprintf("%.4f", s.AvgCost)
			}
			fmt.Printf("  %-8s %+12.2f %6d %6d %10s %8s\n",
				s.Symbol, s.Realized, s.Wins, s.Losses, open, avg)
		}
	}
}

func sinceLabel(t time.Time) string {
	if t.IsZero() {
		return "all time"
	}
	return t.Format("2006-01-02")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runMonitorPanicSafe runs the monitor agent, restarting it after a panic
// so one bad tick never kills a 24/7 process. A clean shutdown signal
// (ctx cancelled / kill switch halted) exits the loop normally.
func runMonitorPanicSafe(ctx context.Context, g *agents.Graph, a agent.Agent, message string) {
	for {
		if ctx.Err() != nil || g.KillSwitch.Halted() {
			return
		}

		// B5: market-hours gate. Outside trading sessions skip the tick
		// (no LLM tokens burned on stale data) and sleep one poll interval.
		if cfgFromMain != nil && !marketOpen(cfgFromMain) {
			log.Printf("[monitor] market closed; sleeping %ds", cfgFromMain.PollSeconds)
			if !sleepCtx(ctx, time.Duration(cfgFromMain.PollSeconds)*time.Second) {
				return
			}
			continue
		}

		done := make(chan struct{})
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[monitor] recovered from panic: %v", r)
				}
				close(done)
			}()
			runOnce(ctx, a, message)
		}()
		select {
		case <-done:
			// Agent returned on its own: exit cleanly.
			return
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
			if g.KillSwitch.Halted() {
				return
			}
			// Loop continues; the goroutine is still running runOnce.
			// If it finished between done and here, done will fire next pass.
		}

		// Wait for the current attempt to finish before restarting.
		<-done

		// B2: refuse further ticks once halted; pace ticks with POLL_SECONDS.
		if g.KillSwitch.Halted() || ctx.Err() != nil {
			return
		}
		if cfgFromMain != nil {
			sleepCtx(ctx, time.Duration(cfgFromMain.PollSeconds)*time.Second)
		}
	}
}

// marketOpen checks Alpaca /v2/clock; on API failure it fails open so a
// transient data outage doesn't stall monitoring (order gates still apply).
func marketOpen(cfg *config.Config) bool {
	c := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	cl, err := c.GetClock(context.Background())
	if err != nil || cl == nil {
		log.Printf("[monitor] clock check failed (%v); assuming open", err)
		return true
	}
	return cl.IsOpen
}

// sleepCtx sleeps for d or until ctx is cancelled; reports whether the
// full duration elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---- decision audit log (shared with main.go) ----

var (
	muState     sync.Mutex
	lastState   = map[string]any{}
	cfgFromMain *config.Config // set in main(); used by runOnce/journalDecisions
)

// journalDecisions writes the latest expert outputs (trade ideas, risk
// assessment, executions) to the trade log for auditability.
func journalDecisions(cfg *config.Config) {
	if cfg == nil {
		return
	}
	j, err := openJournal(cfg)
	if err != nil {
		log.Printf("trade log unavailable: %v", err)
		return
	}
	defer j.Close()
	muState.Lock()
	snapshot := make(map[string]any, len(lastState))
	for k, v := range lastState {
		snapshot[k] = v
		delete(lastState, k)
	}
	muState.Unlock()
	for _, key := range pnl.StateKeys {
		v, ok := snapshot[key]
		if !ok {
			continue
		}
		for _, rec := range pnl.ExtractDecisions(key, v) {
			if err := j.Append(rec); err != nil {
				log.Printf("decision log append: %v", err)
			}
		}
	}
}

// ---- options chain ----

// chainArgs is the parsed flag set of the `chain` subcommand.
type chainArgs struct {
	envFile string
	symbol  string
	exp     string
	typ     string
}

// parseChainArgs parses chain subcommand flags without touching config or
// network so it can be table-tested.
func parseChainArgs(args []string) (chainArgs, int) {
	var p chainArgs
	fs := flag.NewFlagSet("chain", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&p.envFile, "env", ".env", "path to .env file")
	fs.StringVar(&p.symbol, "symbol", "", "underlying ticker (required)")
	fs.StringVar(&p.exp, "exp", "", "expiration date YYYY-MM-DD (default: through next weekend)")
	fs.StringVar(&p.typ, "type", "", "filter: call | put (default: both)")
	if err := fs.Parse(args); err != nil {
		return p, 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "chain: unexpected positional argument %q\n", fs.Arg(0))
		return p, 2
	}
	return p, 0
}

// cmdChainWrapper loads config then runs the chain report.
func cmdChainWrapper(args []string) int {
	p, code := parseChainArgs(args)
	if code != 0 {
		return code
	}
	cfg, err := config.Load(p.envFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chain: config: %v\n", err)
		return 1
	}
	return cmdChain(context.Background(), cfg, p)
}

// cmdChain implements `alpacaruns chain`: print the option chain for one
// underlying with quotes, greeks and implied vol. Read-only market data —
// no orders are placed. The query itself is journaled as a decision
// record (source cli:chain) so option research is auditable like trades.
func cmdChain(ctx context.Context, cfg *config.Config, p chainArgs) int {
	sym := strings.ToUpper(strings.TrimSpace(p.symbol))
	if sym == "" {
		fmt.Fprintln(os.Stderr, "chain: --symbol is required")
		return 2
	}
	typ := strings.ToLower(strings.TrimSpace(p.typ))
	if typ != "" && typ != "call" && typ != "put" {
		fmt.Fprintf(os.Stderr, "chain: invalid --type %q (want call|put)\n", p.typ)
		return 2
	}
	if p.exp != "" {
		if _, err := time.Parse("2006-01-02", p.exp); err != nil {
			fmt.Fprintf(os.Stderr, "chain: invalid --exp %q (want YYYY-MM-DD)\n", p.exp)
			return 2
		}
	}

	c := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	c.Log = func(format string, a ...any) { slog.Debug(fmt.Sprintf(format, a...)) }
	oc := options.NewClient(c)

	contracts, _, err := oc.GetContracts(ctx, options.ContractsQuery{
		UnderlyingSymbols: []string{sym},
		ExpirationDateGTE: p.exp,
		Type:              typ,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "chain: contracts lookup: %v\n", err)
		return 1
	}
	if len(contracts) == 0 {
		fmt.Println("no contracts found")
		return 0
	}

	syms := make([]string, len(contracts))
	for i, ct := range contracts {
		syms[i] = ct.Symbol
	}
	snaps, err := oc.GetSnapshots(ctx, syms)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chain: snapshots unavailable (%v); printing contracts only\n", err)
		snaps = nil // degrade gracefully: table without quotes
	}

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SYMBOL\tTYPE\tSTRIKE\tEXP\tBID\tASK\tMID\tLAST\tIV\tDELTA\tTHETA\tVEGA\tOI")
	for _, ct := range contracts {
		var bid, ask, mid, last, iv, delta, theta, vega float64
		var oi string
		if s, ok := snaps[ct.Symbol]; ok {
			bid, ask = s.LatestQuote.BidPrice, s.LatestQuote.AskPrice
			mid, last, iv = s.MidQuote(), s.LatestTrade.Price, s.ImpliedVolatility
			delta, theta, vega = s.Greeks.Delta, s.Greeks.Theta, s.Greeks.Vega
		}
		if ct.OpenInterest != "" {
			oi = ct.OpenInterest
		} else {
			oi = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%.2f\t%s\t%.2f\t%.2f\t%.2f\t%.2f\t%.3f\t%.3f\t%.3f\t%.3f\t%s\n",
			ct.Symbol, strings.ToUpper(ct.Type[:1]), ct.StrikePrice(), ct.ExpirationDate,
			bid, ask, mid, last, iv, delta, theta, vega, oi)
	}
	w.Flush()

	if j, err := openJournal(cfg); err == nil {
		_ = j.Append(pnl.Record{
			Kind:   pnl.KindDecision,
			Source: "cli:chain",
			Symbol: sym,
			Risk:   "INFO",
			Detail: fmt.Sprintf("option chain %s exp>=%s type=%s (%d contracts)", sym, p.exp, typ, len(contracts)),
		})
		j.Close()
	}
	return 0
}
