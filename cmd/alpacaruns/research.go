package main

// research.go: `alpacaruns research` — replay the decision rule over history
// and report what it would have made, per walk-forward fold.
//
// It reads market data and writes a report. It never places an order, never
// touches strategy state, and never feeds anything back into the loop. A
// backtest that can move the live book is not a research tool.

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/factors"
	"github.com/BROCKUGANDA/alpacaruns/research"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

func cmdResearch(args []string) int {
	fs := flag.NewFlagSet("research", flag.ContinueOnError)
	envFile := fs.String("env", ".env", "path to .env file")
	symbols := fs.String("symbols", "", "comma-separated symbols (required)")
	from := fs.String("from", "", "start date YYYY-MM-DD (required)")
	to := fs.String("to", "", "end date YYYY-MM-DD (required)")
	folds := fs.Int("folds", 4, "chronological walk-forward folds")
	notional := fs.Float64("notional", 10000, "fixed stake per trade")
	spread := fs.Float64("spread-bps", 2, "quoted spread, charged half per side")
	slippage := fs.Float64("slippage-bps", 3, "adverse fill per side")
	commission := fs.Float64("commission", 0, "flat currency per side")
	tp := fs.Float64("tp", 0, "take-profit pct (default: strategy settings)")
	sl := fs.Float64("sl", 0, "stop-loss pct (default: strategy settings)")
	register := fs.String("register", "data/research-runs.jsonl", "experiment registry (empty disables)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fromT, err := parseDate(*from)
	if err != nil {
		fmt.Fprintln(os.Stderr, "research: --from:", err)
		return 2
	}
	toT, err := parseDate(*to)
	if err != nil {
		fmt.Fprintln(os.Stderr, "research: --to:", err)
		return 2
	}
	syms := splitList(*symbols)
	if len(syms) == 0 {
		fmt.Fprintln(os.Stderr, "research: --symbols is required")
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
	rc := research.Config{
		Symbols: syms,
		From:    fromT, To: toT, Folds: *folds,
		NotionalPerTrade: *notional,
		Costs: research.CostModel{
			SpreadBps: *spread, SlippageBps: *slippage, CommissionPerSide: *commission,
		},
		Strategy: research.Strategy{
			MinScore:     cfg.FactorMinScore,
			TrendBuy:     set.TrendBuy,
			MomentumBuy:  set.MomentumBuy,
			ExitCompos:   set.ExitComposite,
			ExitMomentum: set.ExitMomentum,
			TPPct:        set.TPPct,
			SLPct:        set.SLPct,
			MaxHoldDays:  int(set.MaxHoldHours / 24),
		},
		Weights:       cfg.FactorWeights,
		FactorOptions: factors.Options{MomentumDays: 10, VolWindow: 20, VolThreshold: 0.03},
	}
	if *tp > 0 {
		rc.Strategy.TPPct = *tp
	}
	if *sl > 0 {
		rc.Strategy.SLPct = *sl
	}
	if len(rc.Weights) == 0 {
		// The engine has defaults; the replay needs the same ones.
		rc.Weights = map[string]float64{"trend": 0.25, "momentum": 0.25, "volatility": 0.15, "volume": 0.15, "sentiment": 0.20}
	}

	client := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	client.Log = func(string, ...any) {}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// Split the universe by venue: crypto pairs come from the crypto bars
	// endpoint, everything else from stocks. Researching only equities is how
	// a class gets judged without ever being measured.
	start := fromT.AddDate(0, 0, -120).Format("2006-01-02") // warm-up for SMA50
	end := toT.Format("2006-01-02")
	var stocks, pairs []string
	for _, s := range syms {
		if strings.Contains(s, "/") {
			pairs = append(pairs, s)
		} else {
			stocks = append(stocks, s)
		}
	}
	bars := map[string][]tools.Bar{}
	if len(stocks) > 0 {
		got, err := client.GetBars(ctx, stocks, "1Day", start, end, 10000)
		if err != nil {
			log.Printf("research: fetch stock bars: %v", err)
			return 1
		}
		for k, v := range got {
			bars[k] = v
		}
	}
	if len(pairs) > 0 {
		got, err := client.GetCryptoBars(ctx, pairs, "1Day", start, end, 10000)
		if err != nil {
			log.Printf("research: fetch crypto bars: %v", err)
			return 1
		}
		for k, v := range got {
			bars[k] = v
		}
	}
	if err != nil {
		log.Printf("research: fetch bars: %v", err)
		return 1
	}
	if len(bars) == 0 {
		log.Printf("research: no bars returned for %v", syms)
		return 1
	}

	rep, err := research.Run(bars, rc)
	if err != nil {
		log.Printf("research: %v", err)
		return 1
	}
	printReport(rep, rc, syms)

	if strings.TrimSpace(*register) != "" {
		path := *register
		if !filepath.IsAbs(path) && cfg.TradeLog != "" {
			// Keep the registry beside the journal so a deployment has one
			// data directory to back up.
			path = filepath.Join(filepath.Dir(cfg.TradeLog), filepath.Base(path))
		}
		rec, err := research.AppendRun(path, rc, rep)
		if err != nil {
			log.Printf("research: registry: %v", err)
			return 1
		}
		fmt.Printf("\nregistered: %s (%s)\n", path, rec.ConfigHash)
	}
	return 0
}

func printReport(rep research.Report, cfg research.Config, syms []string) {
	fmt.Printf("walk-forward replay  %s → %s  symbols=%s  folds=%d\n",
		cfg.From.Format("2006-01-02"), cfg.To.Format("2006-01-02"), strings.Join(syms, ","), len(rep.Folds))
	fmt.Printf("costs: spread %.1fbps (half per side), slippage %.1fbps, commission %.2f/side\n",
		cfg.Costs.SpreadBps, cfg.Costs.SlippageBps, cfg.Costs.CommissionPerSide)
	fmt.Printf("%-6s %8s %8s %10s %12s %10s %10s\n", "fold", "trades", "win%", "net", "expectancy", "costs", "maxDD")
	for _, f := range rep.Folds {
		fmt.Printf("%-6d %8d %7.1f%% %10.2f %11.2f%% %10.2f %9.1f%%\n",
			f.Index, f.Metrics.Trades, 100*f.Metrics.WinRate, f.Metrics.NetPL,
			100*f.Metrics.ExpectancyPct, f.Metrics.Costs, 100*f.Metrics.MaxDrawdownPct)
	}
	o := rep.Overall
	fmt.Printf("%-6s %8d %7.1f%% %10.2f %11.2f%% %10.2f %9.1f%%\n",
		"ALL", o.Trades, 100*o.WinRate, o.NetPL, 100*o.ExpectancyPct, o.Costs, 100*o.MaxDrawdownPct)
	fmt.Printf("return on staked capital: %.2f%%   gross %.2f, costs %.2f, net %.2f\n",
		100*o.NetReturnPct, o.GrossPL, o.Costs, o.NetPL)
	// Explain a zero-trade result before the caveats: "the rule found nothing"
	// and "the replay never scored a bar" look identical in the table.
	d := rep.Diag
	fmt.Printf("diagnostics: scored=%d entries=%d | below floor=%d, trend gate=%d, momentum gate=%d, scorer errors=%d\n",
		d.BarsScored, d.Entries, d.BelowMinScore, d.TrendGate, d.MomentumGate, d.ScoreErrors)
	if d.BarsScored == 0 {
		fmt.Println("  WARNING: no bar was ever scored — a data or warm-up problem, not a verdict on the rule")
	}
	if d.Entries == 0 && d.BarsScored > 0 {
		fmt.Println("  reading: the rule is too selective to fire on this universe/period; that is a result, not a bug")
	}
	for _, n := range rep.Notes {
		fmt.Println("  note:", n)
	}
}

func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("required")
	}
	return time.Parse("2006-01-02", s)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.ToUpper(p))
		}
	}
	return out
}
