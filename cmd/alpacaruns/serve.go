// serve.go implements `alpacaruns serve`: the dashboard HTTP API.
// The bot and the API are deliberately two separate processes (Option A
// from the hackathon spec) so the bot keeps trading even when the
// dashboard is unreachable, and vice versa. The API reads the same
// data/ directory the bot writes; the pause flag is the only file
// both touch, and it's atomic (write-temp + rename).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/BROCKUGANDA/alpacaruns/api"
	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// serveFlags holds the parsed `serve` subcommand flags.
type serveFlags struct {
	envFile    string
	port       int
	corsOrigin string
	disablePause bool
	disableStep  bool
}

// newServeFlagSet builds and binds the `serve` flag set; extracted
// for tests. Registration happens exactly once here.
func newServeFlagSet(f *serveFlags) *flag.FlagSet {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&f.envFile, "env", ".env", "path to .env file")
	fs.IntVar(&f.port, "port", 8080, "TCP port to listen on (also ALPACA_API_PORT env)")
	fs.StringVar(&f.corsOrigin, "cors-origin", "", "allowed CORS origin (default: any, set explicitly in production)")
	fs.BoolVar(&f.disablePause, "no-pause", false, "disable /api/control/pause and /resume endpoints")
	fs.BoolVar(&f.disableStep, "no-step", false, "disable /api/control/step endpoint")
	return fs
}

// cmdServe parses flags, loads config + strategy settings, wires the
// Alpaca client and runs the HTTP server until SIGINT/SIGTERM.
func cmdServe(args []string) int {
	var fl serveFlags
	fs := newServeFlagSet(&fl)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "serve: unexpected positional argument %q\n", fs.Arg(0))
		return 2
	}

	cfg, err := config.Load(fl.envFile)
	if err != nil {
		log.Printf("config: %v", err)
		return 1
	}
	set, err := strategy.LoadSettings(strategy.OsGetenv, cfg.PollSeconds)
	if err != nil {
		log.Printf("strategy settings: %v", err)
		return 1
	}

	client := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	client.Log = func(format string, a ...any) { log.Printf("[alpaca] "+format, a...) }

	port := fl.port
	if env := os.Getenv("ALPACA_API_PORT"); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 && n < 65536 {
			port = n
		}
	}
	cors := fl.corsOrigin
	if cors == "" {
		cors = os.Getenv("CORS_ORIGIN")
	}
	if cors == "" {
		cors = "*"
	}

	settings := api.ServerSettings{
		Port:        port,
		CORSOrigin:  cors,
		TradeLog:    cfg.TradeLog,
		StateFile:   dataSibling(cfg.TradeLog, "strategy-state.json"),
		PauseFlag:   dataSibling(cfg.TradeLog, "paused"),
		AllowStep:   !fl.disableStep,
		AllowPause:  !fl.disablePause,
	}

	srv := api.New(cfg, client, settings, &set)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("[serve] alpacaruns dashboard api on :%d (trade_log=%s state=%s pause=%s cors=%s)",
		port, settings.TradeLog, settings.StateFile, settings.PauseFlag, cors)
	if err := srv.Run(ctx); err != nil {
		log.Printf("serve: %v", err)
		return 1
	}
	return 0
}

// dataSibling returns "dir/name" where dir is the directory holding
// tradeLog (defaulting to "data" when tradeLog is empty or relative
// without a directory component). Keeps the API's file layout
// aligned with the bot's defaults.
func dataSibling(tradeLog, name string) string {
	if tradeLog == "" {
		return filepath.Join("data", name)
	}
	dir := filepath.Dir(tradeLog)
	if dir == "" || dir == "." {
		return filepath.Join("data", name)
	}
	return filepath.Join(dir, name)
}