package main

// caps.go: `alpacaruns caps` — build the market-cap table the universe screen
// reads. Shares from SEC EDGAR, closes from the broker; both authoritative.

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
	"github.com/BROCKUGANDA/alpacaruns/tools"
	"github.com/BROCKUGANDA/alpacaruns/universe"
)

func cmdCaps(args []string) int {
	fs := flag.NewFlagSet("caps", flag.ContinueOnError)
	envFile := fs.String("env", ".env", "path to .env file")
	symbols := fs.String("symbols", "", "comma-separated single-name equities (default: STRATEGY_EQUITY_SYMBOLS)")
	out := fs.String("out", "", "output path (default: data/market-caps.json)")
	refresh := fs.Bool("refresh", true, "fetch and rewrite the table")
	pause := fs.Duration("pause", 150*time.Millisecond, "delay between EDGAR requests")
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
	syms := splitList(*symbols)
	if len(syms) == 0 {
		syms = set.EquitySymbols
	}
	if len(syms) == 0 {
		fmt.Fprintln(os.Stderr, "caps: no symbols given and STRATEGY_EQUITY_SYMBOLS is empty")
		return 2
	}
	path := strings.TrimSpace(*out)
	if path == "" {
		path = universe.Path(cfg.TradeLog)
	}

	if !*refresh {
		caps, err := universe.LoadCaps(path)
		if err != nil {
			log.Printf("caps: %v", err)
			return 1
		}
		printCaps(caps, path)
		return 0
	}

	ua := strings.TrimSpace(os.Getenv("EDGAR_USER_AGENT"))
	if ua == "" {
		ua = universe.DefaultEDGARUserAgent
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	client.Log = func(string, ...any) {}
	closeOf := func(ctx context.Context, sym string) (float64, error) {
		bars, err := client.GetBars(ctx, []string{sym}, "1Day",
			time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02"), "", 10)
		if err != nil {
			return 0, err
		}
		series := bars[sym]
		if len(series) == 0 {
			return 0, fmt.Errorf("no bars")
		}
		return series[len(series)-1].Close, nil
	}
	caps, skipped, err := universe.Fetch(ctx, &http.Client{Timeout: 20 * time.Second}, syms, closeOf,
		universe.FetchOptions{UserAgent: ua, Pause: *pause})
	if err != nil {
		log.Printf("caps: %v", err)
		return 1
	}
	if err := universe.SaveCaps(path, caps); err != nil {
		log.Printf("caps: save: %v", err)
		return 1
	}
	printCaps(caps, path)
	for _, s := range skipped {
		fmt.Println("  skipped:", s)
	}
	if len(caps.Caps) == 0 {
		fmt.Println("no caps recorded — check EDGAR_USER_AGENT (the SEC requires a contact address)")
		return 1
	}
	return 0
}

func printCaps(c universe.Caps, path string) {
	fmt.Printf("market caps from %s\n", c.Source)
	fmt.Printf("observed: %s   file: %s\n", c.AsOf.Format(time.RFC3339), path)
	for _, sym := range c.SortedSymbols() {
		fmt.Printf("  %-8s $%.2fB\n", sym, c.Caps[sym]/1e9)
	}
}
