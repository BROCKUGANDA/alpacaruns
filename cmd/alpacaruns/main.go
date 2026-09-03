// Command alpacaruns is the CLI: run one trading cycle, start the monitor
// loop, or ask an ad-hoc market question. Paper trading only.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

const appName = "alpacaruns"

func main() {
	os.Exit(run(os.Args[1:]))
}

const usage = `usage: alpacaruns <command> [flags]
  cycle    [--symbols AAPL,MSFT,NVDA] [--provider llamacpp|oxlo|gemini] [--env .env]
  monitor  [--interval SECONDS] [--symbols ...] [--provider llamacpp|oxlo|gemini] [--env .env]
           start the continuous monitoring loop (Ctrl+C = graceful stop)
  auto     [--once] and/or [--dry-run] with [--env .env]
           deterministic no-LLM trading loop: factor scores through
           thresholds into sized entries with TP/SL brackets inside
           TRADING_WINDOWS (equities) / CRYPTO_WINDOWS (crypto, 24/7);
           once = single pass, dry run = log decisions, place none.
  query "<question>" [--provider llamacpp|oxlo|gemini] [--env .env]
           ad-hoc natural-language market question
  pl       [--since DATE] [--json] [--env .env]
           print equity, realized/unrealized P/L, win rate, per-symbol breakdown
  trade    --symbol TSLA|--occ AAPL240119C00100000 --side buy --qty 1
           [--limit PRICE] [--tif gtc|day] [--extended-hours (equities only)]
           [--notional N (equities only)] [--env .env]
           place one manual order through the same risk validator (no LLM);
           OCC-format symbols (or --occ) trade options: qty = contracts,
           tif day|gtc only, no notional, no extended hours
  chain    --symbol AAPL [--exp YYYY-MM-DD] [--type call|put] [--env .env]
           print the option chain with quotes, greeks and implied vol
  factors  [explain SYMBOL] [--env .env]
           list factor weights, or explain one symbol's multi-factor score
`

// run parses the global shape `alpacaruns <subcommand> [flags]` and
// dispatches. Every subcommand owns its own FlagSet so flags are
// validated per-command with clear errors and nonzero exit codes.
func run(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	sub, rest := argv[0], argv[1:]

	switch sub {
	case "pl":
		// `pl` talks straight to the Alpaca REST API: no LLM, no MCP
		// server, no uvx dependency. It loads its own config.
		return cmdPLWrapper(rest)
	case "trade":
		return cmdTrade(rest)
	case "chain":
		return cmdChainWrapper(rest)
	case "factors":
		return cmdFactorsWrapper(rest)
	case "auto":
		// `auto` is fully deterministic: no LLM, no MCP server. It loads
		// its own config and strategy settings.
		return cmdAuto(rest)
	}

	// LLM-backed subcommands share a common flag prefix.
	var envFile string
	var symbols string
	var interval int
	var question string
	var provider string
	fs := flag.NewFlagSet(sub, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&envFile, "env", ".env", "path to .env file")
	switch sub {
	case "cycle":
		fs.StringVar(&symbols, "symbols", "", "comma-separated watchlist (default: AAPL,MSFT,NVDA)")
	case "monitor":
		fs.IntVar(&interval, "interval", 0, "seconds between monitor ticks (default: POLL_SECONDS)")
		fs.StringVar(&symbols, "symbols", "", "comma-separated watchlist (default: AAPL,MSFT,NVDA)")
	case "query":
		fs.StringVar(&question, "question", "", "natural-language market question")
		fs.StringVar(&provider, "provider", "", "LLM backend override: llamacpp | oxlo | gemini (default: from env)")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", sub, usage)
		return 2
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		if sub == "query" && fs.NArg() == 1 {
			question = fs.Arg(0) // positional form kept for convenience
		} else {
			fmt.Fprintf(os.Stderr, "%s: unexpected positional argument %q\n", sub, fs.Arg(0))
			return 2
		}
	}
	if sub == "query" && strings.TrimSpace(question) == "" {
		fmt.Fprintln(os.Stderr, `query requires a question, e.g. alpacaruns query "what is AAPL's latest quote"`)
		return 2
	}
	if interval < 0 {
		fmt.Fprintf(os.Stderr, "monitor: --interval must be >= 0, got %d\n", interval)
		return 2
	}

	cfg, err := config.Load(envFile)
	if err != nil {
		log.Printf("config: %v", err)
		return 1
	}
	if err := applyProviderOverride(cfg, provider); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", sub, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, err := agents.Build(ctx, cfg, slog.Default())
	if err != nil {
		log.Printf("build agents: %v", err)
		return 1
	}
	defer g.Close()

	watch := parseSymbols(symbols)
	message := cycleMessage(watch)

	var target agent.Agent
	switch sub {
	case "cycle":
		target = g.TradeCycle
	case "monitor":
		if interval > 0 && interval != cfg.PollSeconds {
			cfg.PollSeconds = interval // --interval overrides POLL_SECONDS
		}
		// 24/7 readiness: SIGINT/SIGTERM stops NEW cycles cleanly via the
		// kill switch (in-flight order polling finishes), and the monitor
		// run itself recovers from panics so one bad tick never kills the
		// process. Reboot reconciliation happens in runOnce below.
		go func() {
			<-ctx.Done()
			g.KillSwitch.Halt()
			fmt.Println("\n[shutdown] signal received: halting new trades, finishing in-flight work")
		}()
		if err := bootReconcile(ctx, cfg); err != nil {
			log.Printf("reconcile warning: %v", err)
		}
		// Live streams: ingest market data and journal fills as they
		// happen. A stream failing to construct or dying permanently is
		// logged but never takes down the monitor loop.
		startStreams(ctx, cfg)
		target = g.Monitor
		message = "Start monitoring account risk each tick; exit_loop on breach."
	case "query":
		target, message = g.Root, question
	}

	runMonitorPanicSafe(ctx, g, target, message)
	return 0
}

// applyProviderOverride applies a --provider flag override to cfg,
// mirroring config.Load's LLM_PROVIDER switch: only the three lowercase
// provider names are accepted; empty string is a no-op.
func applyProviderOverride(cfg *config.Config, p string) error {
	switch v := config.LLMProvider(strings.ToLower(strings.TrimSpace(p))); v {
	case "":
		return nil
	case config.ProviderLLamaCPP, config.ProviderOxlo, config.ProviderGemini:
		cfg.LLMProvider = v
	default:
		return fmt.Errorf("--provider must be %q, %q or %q, got %q",
			config.ProviderLLamaCPP, config.ProviderOxlo, config.ProviderGemini, p)
	}
	return nil
}

// parseSymbols splits a comma-separated symbol list, defaulting to the
// standard AAPL/MSFT/NVDA watchlist when empty.
func parseSymbols(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToUpper(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"AAPL", "MSFT", "NVDA"}
	}
	return out
}

// cycleMessage renders the trading-cycle prompt for a watchlist.
func cycleMessage(watch []string) string {
	return "Run one full trading cycle for " + strings.Join(watch, ", ") + "."
}

// bootReconcile reloads broker state on startup and journals the snapshot
// plus any drift between the local FIFO books and live Alpaca positions.
func bootReconcile(ctx context.Context, cfg *config.Config) error {
	j, err := openJournal(cfg)
	if err != nil {
		return fmt.Errorf("open trade log: %w", err)
	}
	defer j.Close()
	c := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	drift, err := reconcile(ctx, c, j)
	if err != nil {
		return err
	}
	for _, d := range driftLines(drift) {
		log.Printf("[reconcile] drift: %s", d)
	}
	return nil
}

func runOnce(ctx context.Context, a agent.Agent, message string) {
	svc := session.InMemoryService()
	sess, err := svc.Create(ctx, &session.CreateRequest{AppName: appName, UserID: "local"})
	if err != nil {
		log.Fatalf("session: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName:        appName,
		Agent:          a,
		SessionService: svc,
	})
	if err != nil {
		log.Fatalf("runner: %v", err)
	}
	content := genai.NewContentFromText(message, genai.RoleUser)
	for ev, err := range r.Run(ctx, "local", sess.Session.ID(), content, agent.RunConfig{
		StreamingMode: agent.StreamingModeNone,
	}) {
		if err != nil {
			log.Printf("event error: %v", err)
			continue
		}
		if ev.Content != nil {
			for _, part := range ev.Content.Parts {
				if part.Text != "" {
					fmt.Println(part.Text)
				}
			}
		}
		// Capture expert outputs (OutputKey writes land as StateDelta) so
		// every trade decision is auditable in the trade log.
		for k, v := range ev.Actions.StateDelta {
			muState.Lock()
			lastState[k] = v
			muState.Unlock()
		}
	}

	journalDecisions(cfgFromMain)
}
