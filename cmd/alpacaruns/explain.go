// `factors` subcommand: read-only multi-factor scoring reports.
//
//	alpacaruns factors                     list configured factor names,
//	                                       weights and FACTOR_MIN_SCORE
//	alpacaruns factors explain AAPL        per-factor breakdown for one
//	                                       symbol (no orders are placed)
//
// The query is journaled as a decision record (source cli:factors) so
// factor research is auditable like trades and option-chain lookups.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/factors"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// factorsArgs is the parsed flag set of the `factors` subcommand.
type factorsArgs struct {
	envFile string
	explain bool   // true => `factors explain <symbol>`
	symbol  string // required when explain
	exp     string // optional expiration date, validated but informational
}

// parseFactorsArgs parses `factors [explain <symbol>] [flags]` without
// touching config or network so it can be table-tested. The `explain
// SYMBOL` prefix is consumed first so flags may appear in any position
// (`factors explain AAPL --env x.env`). Returns a nonzero exit code on
// syntax or argument errors.
func parseFactorsArgs(args []string) (factorsArgs, int) {
	var p factorsArgs
	rest := args
	if len(args) > 0 && args[0] == "explain" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "factors: explain requires a symbol, e.g. alpacaruns factors explain AAPL")
			return p, 2
		}
		p.explain = true
		p.symbol = strings.ToUpper(strings.TrimSpace(args[1]))
		rest = args[2:]
	} else if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(os.Stderr, "factors: unknown subcommand %q (want \"explain <symbol>\" or nothing)\n", args[0])
		return p, 2
	}
	fs := flag.NewFlagSet("factors", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&p.envFile, "env", ".env", "path to .env file")
	fs.StringVar(&p.exp, "exp", "", "informational context date YYYY-MM-DD")
	if err := fs.Parse(rest); err != nil {
		return p, 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "factors: unexpected positional argument %q\n", fs.Arg(0))
		return p, 2
	}
	if p.exp != "" {
		if _, err := time.Parse("2006-01-02", p.exp); err != nil {
			fmt.Fprintf(os.Stderr, "factors: invalid --exp %q (want YYYY-MM-DD)\n", p.exp)
			return p, 2
		}
	}
	return p, 0
}

// cmdFactorsWrapper loads config then runs the factors report.
func cmdFactorsWrapper(args []string) int {
	p, code := parseFactorsArgs(args)
	if code != 0 {
		return code
	}
	cfg, err := config.Load(p.envFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factors: config: %v\n", err)
		return 1
	}
	return cmdFactors(context.Background(), cfg, p)
}

// cmdFactors implements `alpacaruns factors`. Read-only scoring — no
// orders are placed. Explain runs are journaled like chain lookups.
func cmdFactors(ctx context.Context, cfg *config.Config, p factorsArgs) int {
	if !p.explain {
		printFactorConfig(os.Stdout, cfg.FactorWeights, cfg.FactorMinScore)
		return 0
	}

	c := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	c.Log = func(format string, a ...any) { slog.Debug(fmt.Sprintf(format, a...)) }
	eng := factors.NewEngine(cfg, datedBars{c: c}, c, factors.Options{})
	res, err := eng.Score(ctx, p.symbol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factors: %v\n", err)
		return 1
	}
	printFactorExplain(os.Stdout, p.symbol, res, cfg.FactorWeights, cfg.FactorMinScore)

	detail := fmt.Sprintf("factor explain %s composite=%.3f min=%.3f pass=%t",
		p.symbol, res.Composite, cfg.FactorMinScore, res.Passed)
	if j, err := openJournal(cfg); err == nil {
		_ = j.Append(pnl.Record{
			Kind:   pnl.KindDecision,
			Source: "cli:factors",
			Symbol: p.symbol,
			Risk:   "INFO",
			Detail: detail,
		})
		j.Close()
	}
	return 0
}

// datedBars adapts *tools.Client to factors.BarSource. Alpaca's v2 bars
// endpoint returns an EMPTY page when `start` is omitted (verified live),
// so the adapter fills a ~8-month daily lookback; enough for the
// engine's 100-bar request without touching factors/. The response is
// decoded locally because tools.Bar expects a numeric timestamp while
// the v2 REST API returns RFC3339 strings (the websocket stream uses
// epoch seconds); Time is stored as unix nanos for the engine's sort.
type datedBars struct {
	c *tools.Client
}

type datedBar struct {
	Time   string  `json:"t"`
	Open   float64 `json:"o"`
	High   float64 `json:"h"`
	Low    float64 `json:"l"`
	Close  float64 `json:"c"`
	Volume int64   `json:"v"`
}

func (d datedBars) GetBars(ctx context.Context, symbols []string, timeframe, start, end string, limit int) (map[string][]tools.Bar, error) {
	if start == "" {
		start = time.Now().AddDate(0, -8, 0).Format("2006-01-02")
	}
	q := url.Values{}
	q.Set("symbols", strings.Join(symbols, ","))
	q.Set("timeframe", timeframe)
	if start != "" {
		q.Set("start", start)
	}
	if end != "" {
		q.Set("end", end)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var br struct {
		Bars map[string][]datedBar `json:"bars"`
	}
	if err := d.c.Do(ctx, "GET", d.c.DataURL+"/stocks/bars", q, nil, &br); err != nil {
		return nil, err
	}
	out := make(map[string][]tools.Bar, len(br.Bars))
	for sym, bars := range br.Bars {
		ts := make([]tools.Bar, len(bars))
		for i, b := range bars {
			t := int64(0)
			if p, err := time.Parse(time.RFC3339, b.Time); err == nil {
				t = p.UnixNano()
			}
			ts[i] = tools.Bar{Time: t, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume}
		}
		out[sym] = ts
	}
	return out, nil
}

// printFactorConfig lists configured factor names, weights and the
// minimum composite score. Pure function of its arguments (for tests).
func printFactorConfig(w io.Writer, weights map[string]float64, minScore float64) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FACTOR\tWEIGHT")
	for _, name := range sortedFactorNames(weights) {
		fmt.Fprintf(tw, "%s\t%.3f\n", name, weights[name])
	}
	tw.Flush()
	fmt.Fprintf(w, "\nFACTOR_MIN_SCORE = %.3f\n", minScore)
}

// sortedFactorNames returns the factor names in deterministic order.
func sortedFactorNames(weights map[string]float64) []string {
	names := make([]string, 0, len(weights))
	for n := range weights {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// printFactorExplain renders the per-factor breakdown: score, weight,
// contribution (score x weight) and rationale per factor, then the
// composite vs FACTOR_MIN_SCORE verdict line. Pure function of its
// arguments so it can be tested with an injected engine result.
func printFactorExplain(w io.Writer, symbol string, res factors.Result, weights map[string]float64, minScore float64) {
	names := make([]string, 0, len(res.Factors))
	for name := range res.Factors {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintf(w, "factor explain %s\n\n", symbol)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FACTOR\tSCORE\tWEIGHT\tCONTRIB\tRATIONALE")
	for _, n := range names {
		fr := res.Factors[n]
		wgt := weights[n]
		fmt.Fprintf(tw, "%s\t%.3f\t%.3f\t%+.3f\t%s\n", n, fr.Score, wgt, fr.Score*wgt, fr.Rationale)
	}
	tw.Flush()
	verdict := "FAIL"
	if res.Passed {
		verdict = "PASS"
	}
	fmt.Fprintf(w, "\ncomposite %.3f vs FACTOR_MIN_SCORE %.3f (%s)\n",
		res.Composite, minScore, verdict)
}
