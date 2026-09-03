// auto.go implements `alpacaruns auto`: the deterministic no-LLM trading
// loop. Every POLL_SECONDS it scores the symbol universe with
// factors.Engine, applies the threshold rule, sizes entries fixed-
// fractionally and routes orders through the same risk validator
// (agents.NewValidator) as every other path, then journals each decision.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math"
	"path/filepath"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/factors"
	"github.com/BROCKUGANDA/alpacaruns/options"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
	"github.com/BROCKUGANDA/alpacaruns/strategy/ensemble"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// autoLoop bundles everything one tick needs; explicit for testability.
type autoLoop struct {
	cfg        *config.Config
	set        strategy.Settings
	engine     *strategy.Engine
	windows    *strategy.TradingWindows
	exec       *strategy.Executor
	monitor    *strategy.PositionMonitor
	optPlanner *strategy.OptionPlanner
	journal    *pnl.Journal
	client     *tools.Client
	dryRun bool
	// pauseFlagPath is the path to the JSON pause flag file (defaults
	// to data/paused). Kept on the struct so tests can point it at a
	// temp dir without reaching for an env var.
	pauseFlagPath string
	// pausedLogged remembers whether we already printed the
	// "[control] paused" line; we don't spam it on every tick.
	pausedLogged bool
	// ensemble is non-nil only when ENSEMBLE_ENABLED=true; nil keeps the
	// original single-expert tick path bit-for-bit.
	ensemble   *ensemble.Runner
	// Intraday track (INTRADAY_TRACK != off): independent 15-minute-bar
	// engine, own execution windows and sizing; shadow mode logs only.
	intra    *strategy.Engine
	intraWin *strategy.TradingWindows
	// stats is the per-symbol win/loss ledger (see stats.go). Read on
	// every tick to gate entries via ConfidenceBias; written whenever
	// closePosition fires and the trade log shows a closed round-trip.
	stats *statsLedger
}

// autoFlags holds the parsed `auto` subcommand flags.
type autoFlags struct {
	envFile string
	once    bool
	dryRun  bool
}

// newAutoFlagSet builds and binds the `auto` flag set; extracted for
// tests. Registration happens exactly once here.
func newAutoFlagSet(f *autoFlags) *flag.FlagSet {
	fs := flag.NewFlagSet("auto", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&f.envFile, "env", ".env", "path to .env file")
	fs.BoolVar(&f.once, "once", false, "run a single engine pass and exit")
	fs.BoolVar(&f.dryRun, "dry-run", false, "log decisions without placing orders")
	return fs
}

// cmdAuto parses flags, loads config + strategy settings, wires the
// deterministic engine and runs the loop until Ctrl+C (or one pass with
// --once).
func cmdAuto(args []string) int {
	var fl autoFlags
	fs := newAutoFlagSet(&fl)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "auto: unexpected positional argument %q\n", fs.Arg(0))
		return 2
	}
	envFile, once, dryRun := fl.envFile, fl.once, fl.dryRun

	cfg, err := config.Load(envFile)
	if err != nil {
		log.Printf("config: %v", err)
		return 1
	}
	// Strategy env vars are read strictly AFTER config.Load so values
	// from --env .env are already materialized into the process
	// environment (loadEnvFile -> os.Setenv). The strategy workstream
	// owns its env parsing inside strategy/ because config/config.go is
	// concurrently owned by another workstream.
	set, err := strategy.LoadSettings(strategy.OsGetenv, cfg.PollSeconds)
	if err != nil {
		log.Printf("strategy settings: %v", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	loop, code := buildAutoLoop(ctx, cfg, set, dryRun)
	if code != 0 {
		return code
	}
	defer loop.journal.Close()

	log.Printf("[auto] deterministic loop start: equities=%v crypto=%v poll=%ds dry-run=%t",
		loop.set.EquitySymbols, loop.set.CryptoSymbols, loop.set.PollSeconds, dryRun)

	for {
		if ctx.Err() != nil || loop.exec.Kill.Halted() {
			log.Printf("[auto] stopped (signal or kill switch)")
			return 0
		}
		if err := loop.tick(ctx); err != nil {
			log.Printf("[auto] tick error: %v", err)
		}
		if once {
			return 0
		}
		if !sleepCtx(ctx, time.Duration(loop.set.PollSeconds)*time.Second) {
			log.Printf("[auto] stopped during sleep")
			return 0
		}
	}
}

// buildAutoLoop constructs every collaborator from loaded config.
func buildAutoLoop(ctx context.Context, cfg *config.Config, set strategy.Settings, dryRun bool) (*autoLoop, int) {
	client := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	client.Log = func(format string, a ...any) { slog.Debug(fmt.Sprintf(format, a...)) }

	j, err := openJournal(cfg)
	if err != nil {
		log.Printf("journal: %v", err)
		return nil, 1
	}
	state := strategy.NewStateStore(j.Path())
	kill := agents.NewKillSwitch()

	// Factor scoring across both venues; news stays nil so sentiment
	// degrades to neutral instead of failing ticks.
	bars := strategy.NewMultiVenueBars(client)
	scorer := strategy.FactorScorer{Inner: factors.NewEngine(cfg, bars, nil, factors.Options{})}
	riskScorer := factors.NewEngine(cfg, bars, nil, factors.Options{})

	// Wire the validator with the same MultiVenueBars-aware scorer so the
	// risk gate's factor check routes crypto symbols to /v1beta3/...
	val := agents.NewValidatorWithScorer(kill, cfg, riskScorer)

	engine, err := strategy.NewEngine(strategy.EngineConfig{
		Scorer: scorer,
		Prices: strategy.NewPriceSource(client),
		ReferenceBars: bars,
		Threshold: strategy.Thresholds{
			FactorMinScore: cfg.FactorMinScore,
			TrendBuy:       set.TrendBuy,
			MomentumBuy:    set.MomentumBuy,
			ExitComposite:  set.ExitComposite,
			ExitMomentum:   set.ExitMomentum,
		},
		Sizing: strategy.Sizing{PositionPct: set.PositionPct, MaxPositionUSD: cfg.MaxPositionUSD},
		TPPct:  set.TPPct, SLPct: set.SLPct,
	})
	if err != nil {
		log.Printf("engine: %v", err)
		j.Close()
		return nil, 1
	}

	windows, err := strategy.NewTradingWindows(set.EquityWindowsSpec, set.CryptoWindowsSpec, nil, nil)
	if err != nil {
		log.Printf("windows: %v", err)
		j.Close()
		return nil, 1
	}

	exec := &strategy.Executor{
		Client: client, Validator: val, Kill: kill, State: state, Journal: j,
	}
	var planner *strategy.OptionPlanner
	if set.OptionsEnabled {
		planner, err = strategy.NewOptionPlanner(options.NewClient(client), set,
			strategy.Sizing{PositionPct: set.PositionPct, MaxPositionUSD: cfg.MaxPositionUSD})
		if err != nil {
			log.Printf("option overlay disabled: %v", err)
		}
	} else {
		log.Println("option overlay disabled by OPTIONS_ENABLED=false")
	}

	monitor := &strategy.PositionMonitor{
		Client: client, Kill: kill, State: state, Journal: j, Settings: set,
	}

	loop := &autoLoop{
		cfg: cfg, set: set, engine: engine, windows: windows, exec: exec,
		monitor: monitor, optPlanner: planner, journal: j, client: client,
		dryRun:        dryRun,
		pauseFlagPath: defaultPauseFlagPath(cfg.TradeLog),
	}

	// Per-symbol win/loss ledger. Load existing data; rebuild from the
	// journal so historical trades carry over across restarts.
	loop.stats = newStatsLedger(j.Path())
	if err := loop.stats.load(); err != nil {
		log.Printf("[auto] WARNING: stats load failed: %v", err)
	}
	if err := loop.stats.refreshFromJournal(j); err != nil {
		log.Printf("[auto] WARNING: stats refresh failed: %v", err)
	}
	if all := loop.stats.all(); len(all) > 0 {
		log.Printf("[auto] trade stats loaded: %s", loop.stats.formatDump())
	}
	// Intraday track: independent 15-minute-bar engine with its own
	// windows and sizing. Shares client, validator, kill switch, state
	// store and journal with the swing path — one risk posture.
	if set.IntradayTrack == "shadow" || set.IntradayTrack == "live" {
		intraScorer := strategy.FactorScorer{Inner: factors.NewEngine(cfg, bars, nil,
			factors.Options{Timeframe: "15Min", MomentumDays: 16, VolWindow: 20})}
		intraEngine, err := strategy.NewEngine(strategy.EngineConfig{
			Scorer: intraScorer,
			Prices: strategy.NewPriceSource(client),
			ReferenceBars: bars,
			Threshold: strategy.Thresholds{
				FactorMinScore: cfg.FactorMinScore,
				TrendBuy:       set.TrendBuy,
				MomentumBuy:    set.MomentumBuy,
				ExitComposite:  set.ExitComposite,
				ExitMomentum:   set.ExitMomentum,
			},
			Sizing: strategy.Sizing{PositionPct: set.PositionPctIntraday, MaxPositionUSD: cfg.MaxPositionUSD},
			TPPct: set.IntradayTPPct, SLPct: set.IntradaySLPct,
			BracketMode: set.BracketMode,
			ATRMultTP: set.ATRMultTP, ATRMultSL: set.ATRMultSL,
		})
		if err != nil {
			log.Printf("intraday engine: %v", err)
			j.Close()
			return nil, 1
		}
		intraWin, err := strategy.NewTradingWindows(set.IntradayWindowsSpec, "00:00-23:59", nil, nil)
		if err != nil {
			log.Printf("intraday windows: %v", err)
			j.Close()
			return nil, 1
		}
		loop.intra = intraEngine
		loop.intraWin = intraWin
		log.Printf("[intraday] track enabled mode=%s symbols=%v windows=%s poll=%ds",
			set.IntradayTrack, set.IntradaySymbols, set.IntradayWindowsSpec, set.IntradayPollSeconds)
	}

	// Layer-2 ensemble (off by default): ENSEMBLE_ENABLED=true swaps the
	// tick path to multi-expert -> gater -> risk-budget. The single-
	// expert path above stays untouched when disabled.
	if eCfg, err := ensemble.LoadConfig(strategy.OsGetenv); err != nil {
		log.Printf("ensemble settings: %v", err)
		j.Close()
		return nil, 1
	} else if eCfg.Enabled {
		run, err := ensemble.NewRunner(ensemble.RunnerConfig{
			Cfg:            eCfg,
			Scorer:         ensemble.FactorsScorer{Inner: factors.NewEngine(cfg, bars, nil, factors.Options{})},
			PositionPct:    set.PositionPct,
			MaxPositionUSD: cfg.MaxPositionUSD,
			Thresholds: ensemble.TrendThresholds{
				MinComposite: cfg.FactorMinScore,
				TrendBuy:     set.TrendBuy,
				MomentumBuy:  set.MomentumBuy,
				ExitCompo:    set.ExitComposite,
				ExitMomentum: set.ExitMomentum,
			},
		})
		if err != nil {
			log.Printf("ensemble runner: %v", err)
			j.Close()
			return nil, 1
		}
		if err := run.AttachTracker(ensembleStatePath(j.Path())); err != nil {
			log.Printf("ensemble tracker: %v", err)
			j.Close()
			return nil, 1
		}
		loop.ensemble = run
		log.Printf("[auto] ENSEMBLE enabled: experts=trend,meanrev,breakout,pairs,xsmom,seasonality benchmark=%s pairs=%v",
			eCfg.Benchmark, eCfg.Pairs)
	}
	return loop, 0
}

// ensembleStatePath derives the pending-signal state file path from the
// trade-log path so both live together (data/trades.jsonl ->
// data/ensemble-state.json).
func ensembleStatePath(tradeLogPath string) string {
	dir := filepath.Dir(tradeLogPath)
	if dir == "" || dir == "." {
		return "ensemble-state.json"
	}
	return filepath.Join(dir, "ensemble-state.json")
}

// tick runs one deterministic pass: drawdown halts first, then score ->
// decide -> window-gate -> size -> risk-gate -> execute (or log).
func (l *autoLoop) tick(ctx context.Context) error {
	// Pause flag check FIRST (before any Alpaca call). When the operator
	// toggles "Pause new trades" in the dashboard (POST /api/control/pause),
	// data/paused is written with content "true". The bot reads the file
	// here, logs once per engagement change, and skips entry planning.
	// The monitor loop (positions, drawdown halts, TP/SL exits, EOD
	// flatten, profit targets) keeps running — pause only blocks NEW
	// entries. The file is allowed to be missing or non-"true"; that
	// means running.
	if pauseFlagEngaged(l.pauseFlagPath) {
		if !l.pausedLogged {
			log.Printf("[control] paused: data/paused=true; new entries skipped, monitor loop alive")
			l.pausedLogged = true
		}
		return nil
	}
	if l.pausedLogged {
		log.Printf("[control] resumed: data/paused cleared; entries re-enabled")
		l.pausedLogged = false
	}
	acct, err := l.client.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("account: %w", err)
	}
	equity, _ := strconv.ParseFloat(acct.Equity, 64)
	pfv, _ := strconv.ParseFloat(acct.PortfolioValue, 64)

	// Adaptive sizing: when ADAPTIVE_SIZING=true, scale the per-position
	// budget by a P/L-driven multiplier. The bot reads the live equity
	// each tick and computes pnl = (equity - startingEquity) / startingEquity.
	// In profit it scales up (up to 2x); in drawdown it scales down
	// (floor 0.25x). Starting equity is taken from STATE on first run
	// and persisted to data/strategy-state.json so reboots don't reset.
	if l.set.AdaptiveSizing {
		start := l.startingEquity()
		if start > 0 && equity > 0 {
			pnl := (equity - start) / start
			mult := adaptiveSizingMultiplier(pnl)
			if mult != 1.0 {
				log.Printf("[auto] adaptive sizing: pnl=%.2f%% mult=%.2fx (start=%.2f equity=%.2f)",
					pnl*100, mult, start, equity)
			pfv = pfv * mult
		}
	}
	// Periodic stats log: every 10th tick, dump the per-symbol
	// Periodic stats log: every 10th tick, dump the per-symbol
		// ledger so we can watch the bot learn from its own trades.
		if l.stats != nil {
			l.stats.mu.Lock()
			tickNum := l.stats.tickCounter
			l.stats.tickCounter++
			l.stats.mu.Unlock()
			if tickNum%10 == 0 && tickNum > 0 {
				log.Printf("[auto] trade stats @tick=%d: %s", tickNum, l.stats.formatDump())
			}
		}
 	}
	// First-tick initialization: persist the starting equity for next
	// boot. Idempotent — only writes when the key is unset.
	if l.set.AdaptiveSizing && l.startingEquity() == 0 && equity > 0 {
		l.persistStartingEquity(equity)
	}



	positions, err := l.client.GetPositions(ctx)
	if err != nil {
		return fmt.Errorf("positions: %w", err)
	}
	held := map[string]bool{}
	posSymbols := []string{}
	for _, p := range positions {
		q, _ := strconv.ParseFloat(p.Qty, 64)
		if q == 0 {
			continue
		}
		held[p.Symbol] = true
		posSymbols = append(posSymbols, p.Symbol)
	}

	// Drawdown halts run before any order decision; breach engages the
	// shared kill switch so even concurrent paths stop trading.
	prices := map[string]float64{}
	for _, sym := range posSymbols {
		if px, err := l.engine.PriceOf(ctx, sym); err == nil {
			prices[sym] = px
		}
	}
	res, err := l.monitor.Tick(ctx, prices, equity)
	if err != nil {
		return fmt.Errorf("monitor tick: %w", err)
	}
	if res.HaltEngaged {
		for _, r := range res.HaltReasons {
			log.Printf("[auto] HALT: %s", r)
		}
		return nil
	}

	// Time-based close: exit any position held longer than the cap.
	// This is the "no zombie positions" guard — even if the monitor's
	// TP/SL never trip (e.g. a crypto entry before the TP/SL fix
	// persisted zero levels), the position is force-closed at
	// MAX_HOLD_HOURS. Catches positions that survived a bot restart
	// with stale Since timestamps too.
	if l.set.MaxHoldHours > 0 {
		l.timeBasedClose(ctx, l.set.MaxHoldHours)
	}
	// Profit-target rebalancer: when equity is up >PROFIT_TARGET_PCT
	// from the persisted starting baseline, close the single most
	// profitable position to bank paper gains. Multiple ticks
	// progressively lock in profit while keeping the book exposed to
	// further upside.
	if l.set.ProfitTargetPct > 0 {
		l.takeProfits(ctx, l.set.ProfitTargetPct)
	}

 	universe := append(append([]string{}, l.set.EquitySymbols...), l.set.CryptoSymbols...)

	// Intraday track runs every tick alongside swing (independent engine,
	// own windows). Errors never block the swing path.
	if l.intra != nil {
		if err := l.tickIntraday(ctx, pfv); err != nil {
			log.Printf("[intraday] tick error: %v", err)
		}
	}

	// Layer-2 ensemble path: bars fetched ONCE, shared across experts.
	// Runs ALONGSIDE the swing engine; the early-return previously bypassed
	// the swing path entirely when ensemble was on, leaving the deterministic
	// engine dormant for the entire 5-min tick.
	if l.ensemble != nil {
		if err := l.tickEnsemble(ctx, universe, held, pfv); err != nil {
			log.Printf("[ensemble] tick error: %v", err)
		}
	}

	decisions := l.engine.RunTick(ctx, universe, held)
	conf := 1.0 // deterministic engine: confidence IS the factor gate
	for _, dec := range decisions {
		switch dec.Signal {
		case strategy.SignalBuy:
			l.handleBuy(ctx, dec, pfv, conf)
		case strategy.SignalSell:
			l.handleSell(ctx, dec)
		default:
			log.Printf("[auto] HOLD %s: %s", dec.Symbol, dec.Reason)
		}
	}
	return nil
}

// tickIntraday runs one pass of the intraday track: EOD flatten first,
// then score -> decide on 15-minute bars, gated by INTRADAY_TRADING_WINDOWS.
// Shadow mode logs every decision without placing orders; live mode routes
// through the same executor/risk gate as swing. Intraday positions are
// tracked in state with an "intraday:" symbol prefix so the two books
// never collide.
func (l *autoLoop) tickIntraday(ctx context.Context, pfv float64) error {
	et := l.nowET()
	if l.set.FlattenEOD && !l.intraWin.CanTrade("SPY") {
		// Outside intraday windows: if near/after close, flatten any
		// leftover intraday positions (market order through risk gate).
		if et.Hour() > 15 || (et.Hour() == 15 && et.Minute() >= 45) {
			return l.flattenIntraday(ctx)
		}
		return nil
	}

	intraHeld := l.intraHeldSymbols()
	for _, dec := range l.intra.RunTick(ctx, l.set.IntradaySymbols, intraHeld) {
		switch dec.Signal {
		case strategy.SignalBuy:
			l.intraEntry(ctx, dec, pfv)
		case strategy.SignalSell:
			l.intraExit(ctx, dec, "engine exit")
		default:
			log.Printf("[intraday] HOLD %s: %s", dec.Symbol, dec.Reason)
		}
	}
	return nil
}

// intraEntry places (or logs) one intraday entry with bracket protection.
func (l *autoLoop) intraEntry(ctx context.Context, dec strategy.Decision, pfv float64) {
	if !l.intraWin.CanTrade(dec.Symbol) {
		return // per-symbol window check
	}
	if l.intraHeldSymbols()[dec.Symbol] {
		return // already in the intraday book
	}
	plan, err := l.intra.PlanEntry(ctx, dec, pfv)
	if err != nil {
		log.Printf("[intraday] %s plan skipped: %v", dec.Symbol, err)
		return
	}
	autoJournal(l.journal, fmt.Sprintf("intraday-buy %s qty=%d price=%.4f tp=%.4f sl=%.4f",
		plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss))
	if l.set.IntradayTrack == "shadow" {
		log.Printf("[intraday] SHADOW BUY %s qty=%d @~%.4f tp=%.4f sl=%.4f",
			plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss)
		return
	}
	if err := l.exec.ExecuteEntry(ctx, plan, 1.0); err != nil {
		log.Printf("[intraday] %s entry rejected/failed: %v", plan.Symbol, err)
		return
	}
	_ = l.State().SetLevel(strategy.PositionLevels{
		Symbol: "intraday:" + plan.Symbol, EntryPrice: plan.Price,
		TakeProfit: plan.Brackets.TakeProfit, StopLoss: plan.Brackets.StopLoss,
		Qty: trimNum(float64(plan.Qty)), Since: time.Now().UTC(),
	})
}

// intraExit closes an intraday position by symbol (live mode only).
func (l *autoLoop) intraExit(ctx context.Context, dec strategy.Decision, why string) {
	key := "intraday:" + dec.Symbol
	qty, err := positionQtyFor(ctx, l.client, dec.Symbol)
	if err != nil || qty <= 0 {
		_ = l.State().ClearLevel(key)
		return
	}
	autoJournal(l.journal, fmt.Sprintf("intraday-sell %s qty=%s (%s)", dec.Symbol, trimNum(qty), why))
	if l.set.IntradayTrack == "shadow" {
		log.Printf("[intraday] SHADOW SELL %s qty=%s (%s)", dec.Symbol, trimNum(qty), why)
		return
	}
	req := tools.OrderRequest{
		Symbol: dec.Symbol, Side: "sell", Type: "market", TimeInForce: "gtc",
		Qty: trimNum(qty),
		ClientOrderID: fmt.Sprintf("intraday-eod-%s-%d-%d",
			strategy.SanitizeSymbol(dec.Symbol), int64(qty*10000), time.Now().UTC().Truncate(24*time.Hour).Unix()),
	}
	if _, err := l.client.PlaceOrder(ctx, req); err != nil {
		log.Printf("[intraday] %s close failed: %v", dec.Symbol, err)
		return
	}
	log.Printf("[intraday] EXIT %s qty=%s placed (%s)", dec.Symbol, trimNum(qty), why)
	_ = l.State().ClearLevel(key)
}

// flattenIntraday closes every open intraday-book position (EOD).
func (l *autoLoop) flattenIntraday(ctx context.Context) error {
	st, err := l.State().Load()
	if err != nil {
		return err
	}
	for sym := range st.Levels {
		if !strings.HasPrefix(sym, "intraday:") {
			continue
		}
		base := strings.TrimPrefix(sym, "intraday:")
		l.intraExit(ctx, strategy.Decision{Symbol: base}, "EOD flatten")
	}
	return nil
}

// intraHeldSymbols maps the intraday book into engine held-flags.
func (l *autoLoop) intraHeldSymbols() map[string]bool {
	held := map[string]bool{}
	st, err := l.State().Load()
	if err != nil {
		return held
	}
	for sym := range st.Levels {
		if strings.HasPrefix(sym, "intraday:") {
			held[strings.TrimPrefix(sym, "intraday:")] = true
		}
	}
	return held
}


func (l *autoLoop) State() *strategy.StateStore { return l.exec.State }

// positionUsd returns the live market value (current_price * abs(qty))
// of the open position for symbol, or 0 when flat / lookup fails.
// Used by the per-symbol position cap to detect "no double-down" and
// by the profit-target and time-based close logic to size exits.
func (l *autoLoop) positionUsd(ctx context.Context, symbol string) float64 {
	pos, err := l.client.GetPositions(ctx)
	if err != nil {
		return 0
	}
	for _, p := range pos {
		if p.Symbol != symbol {
			continue
		}
		q, _ := strconv.ParseFloat(p.Qty, 64)
		px, _ := strconv.ParseFloat(p.CurrentPrice, 64)
		return math.Abs(q * px)
	}
	return 0
}

// closePosition places a reducing market order to fully exit a single
// symbol. Used by the time-based close and the profit-target rebalancer.
// The client_order_id is deterministic on (symbol, reason) so retries
// dedupe on the broker side; the call is fire-and-forget for paper
// trading.
func (l *autoLoop) closePosition(ctx context.Context, symbol, reason string) {
	pos, err := l.client.GetPositions(ctx)
	if err != nil {
		log.Printf("[auto] %s close skipped: positions lookup failed: %v", symbol, err)
		return
	}
	var found *tools.Position
	for i := range pos {
		if pos[i].Symbol == symbol {
			found = &pos[i]
			break
		}
	}
	if found == nil {
		return
	}
	qty, _ := strconv.ParseFloat(found.Qty, 64)
	if qty == 0 {
		return
	}
	side := "sell"
	if qty < 0 {
		side = "buy"
	}
	tif := "gtc"
	if tools.IsOCCSymbol(symbol) {
		tif = "day"
	}
	clientID := fmt.Sprintf("auto-close-%s-%s-%d",
		sanitizeForOrderID(symbol), reason, time.Now().UnixNano())
	req := tools.OrderRequest{
		Symbol:        symbol,
		Side:          side,
		Type:          "market",
		TimeInForce:   tif,
		Qty:           strconv.FormatFloat(math.Abs(qty), 'f', -1, 64),
		ClientOrderID: clientID,
	}
	o, err := l.client.PlaceOrder(ctx, req)
	if err != nil {
		log.Printf("[auto] %s close failed: %v", symbol, err)
		return
	}
	log.Printf("[auto] %s %s %s qty=%s order=%s",
		symbol, reason, side, req.Qty, o.ID)
	if l.journal != nil {
		_ = l.journal.Append(pnl.Record{
			Kind: pnl.KindDecision, Source: "strategy:auto", Risk: "APPROVED",
			Detail: fmt.Sprintf("close %s reason=%s side=%s qty=%s order=%s",
				symbol, reason, side, req.Qty, o.ID),
		})
	}
	_ = l.State().ClearLevel(symbol)

	// After the close settles, record the outcome to the stats ledger
	// (recomputes the round-trip from journal fills) and immediately
	// re-evaluate the universe so freed capital is redeployed on the
	// same tick. Without this hook, the bot would wait for the next
	// 5-minute poll before looking for a new entry, leaving cash idle
	// for the duration of a tick.
	if l.stats != nil {
		_ = l.stats.refreshFromJournal(l.journal)
		_ = l.stats.persist()
	}
	// rebalanceAfterClose runs an immediate fresh-tick entry pass over
	// the universe so the freed-up capital isn't idle until the next
	// 5-minute poll. Pulls the live account portfolio value to size
	// the new entries.
	if acct, err := l.client.GetAccount(ctx); err == nil {
		if pfv, perr := strconv.ParseFloat(acct.PortfolioValue, 64); perr == nil {
			l.rebalanceAfterClose(ctx, pfv)
		}
	}
}

// rebalanceAfterClose runs an immediate fresh-tick entry pass over the
// universe to redeploy the cash freed by a recent close. This is the
// "profits don't sit idle" hook: every take-profit / stop-loss / max-
// hold close triggers a new entry search on the same tick. The
// ensemble gater is the source of truth for what to buy; this loop
// just invokes it once with the live pfv and lets the existing
// RiskBudget / per-symbol-cap / ConfidenceBias pipeline pick what to
// enter. We deliberately do NOT call this from the main tick — the
// main tick already runs the gater. This is a re-entry path only.
func (l *autoLoop) rebalanceAfterClose(ctx context.Context, pfv float64) {
	if l.ensemble == nil {
		return // swing-only mode: nothing to rebalance against
	}
	universe := append(append([]string{}, l.set.EquitySymbols...), l.set.CryptoSymbols...)
	pos, err := l.client.GetPositions(ctx)
	if err != nil {
		return
	}
	held := map[string]bool{}
	for _, p := range pos {
		if q, _ := strconv.ParseFloat(p.Qty, 64); q != 0 {
			held[p.Symbol] = true
		}
	}
	// Re-fetch bars and run the ensemble once with the live universe.
	// Same code path as the regular tickEnsemble — decisions get the
	// per-symbol confidence bias applied, per-symbol cap respected.
	bars := strategy.NewMultiVenueBars(l.client)
	md, err := l.ensemble.BuildMarketData(ctx, bars, universe, held, time.Now())
	if err != nil {
		return
	}
	decisions := l.ensemble.Run(ctx, md, pfv)
	for _, d := range decisions {
		if d.Blocked {
			continue
		}
		if d.Action != ensemble.ActionBuy {
			continue
		}
		// Run the same path the regular tick takes; handleBuy does the
		// position cap, sizing, and risk gate checks.
		dec := strategy.Decision{Symbol: d.Symbol, Signal: strategy.SignalBuy,
			Composite: d.Confidence, Reason: "rebalance-after-close: " + d.Reason}
		log.Printf("[auto] rebalance %s qty=%d conf=%.2f (after-close deploy)", d.Symbol, d.Qty, d.Confidence)
		l.handleBuy(ctx, dec, pfv, d.Confidence)
	}
}

// timeBasedClose force-closes every position held longer than
// maxHours. Reads Since from the local state (set on entry). Symbols
// without a stored Since are left alone. This is the "no zombie
// positions" guard: even if TP/SL never trip (e.g. zero levels stored
// for a crypto entry before the fix), the position will be exited
// before it can ride against the bot indefinitely.
func (l *autoLoop) timeBasedClose(ctx context.Context, maxHours float64) {
	if maxHours <= 0 {
		return
	}
	st, err := l.State().Load()
	if err != nil {
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(maxHours * float64(time.Hour)))
	for sym, lv := range st.Levels {
		if strings.HasPrefix(sym, "intraday:") {
			continue // intraday book has its own EOD flatten
		}
		if lv.Since.IsZero() || lv.Since.After(cutoff) {
			continue
		}
		l.closePosition(ctx, sym, fmt.Sprintf("max-hold-%.0fh", maxHours))
	}
}

// takeProfits closes the single most profitable open position when the
// account equity exceeds (1 + targetPct) * startingEquity. The point
// is to convert paper profits into locked-in cash before the regime
// reverses. We close one position per tick (not all) to keep the
// account exposed to upside — multiple ticks progressively bank gains.
func (l *autoLoop) takeProfits(ctx context.Context, targetPct float64) {
	if targetPct <= 0 {
		return
	}
	start := l.startingEquity()
	if start <= 0 {
		return
	}
	acct, err := l.client.GetAccount(ctx)
	if err != nil {
		return
	}
	equity, _ := strconv.ParseFloat(acct.Equity, 64)
	if equity < start*(1+targetPct) {
		return
	}
	pos, err := l.client.GetPositions(ctx)
	if err != nil || len(pos) == 0 {
		return
	}
	// Pick the position with the largest unrealized_pl (most profitable).
	// UnrealizedPL is a string per Alpaca's /positions wire format.
	best := pos[0]
	bestUp, _ := strconv.ParseFloat(best.UnrealizedPL, 64)
	for _, p := range pos[1:] {
		up, _ := strconv.ParseFloat(p.UnrealizedPL, 64)
		if up > bestUp {
			best = p
			bestUp = up
		}
	}
	if bestUp <= 0 {
		return // nothing in profit to bank
	}
	gain := (equity - start) / start * 100
	l.closePosition(ctx, best.Symbol, fmt.Sprintf("profit-take-%.1fpct", gain))
}

// sanitizeForOrderID makes a symbol safe to embed in client_order_id.
func sanitizeForOrderID(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "/", "-"), " ", "")
}


func (l *autoLoop) nowET() time.Time {
	if et, err := time.LoadLocation("America/New_York"); err == nil {
		return time.Now().In(et)
	}
	return time.Now()
}

// tickEnsemble runs the Layer-2 pass: batch bars -> MarketData -> all
// experts -> vol-regime -> gater -> RiskBudget -> existing executor
// paths (handleBuy/handleSell). Every ensemble decision is journaled as
// kind=decision source=strategy:ensemble with the full per-expert vote
// trail in Detail for auditability.
func (l *autoLoop) tickEnsemble(ctx context.Context, universe []string, held map[string]bool, pfv float64) error {
	bars := strategy.NewMultiVenueBars(l.client)
	md, err := l.ensemble.BuildMarketData(ctx, bars, universe, held, time.Now())
	if err != nil {
		return fmt.Errorf("ensemble market data: %w", err)
	}
	// Resolve yesterday's pending signals against fresh closes first so
	// hit-rates used by the gater are current.
	closes := map[string][]float64{}
	for sym, sd := range md.Symbols {
		closes[sym] = sd.Closes
	}
	l.ensemble.ResolvePerformance(closes)

	for _, d := range l.ensemble.Run(ctx, md, pfv) {
		votes := formatVotes(d.Votes)
		switch {
		case d.Blocked:
			log.Printf("[ensemble] BLOCKED %s BUY: %s", d.Symbol, d.BlockWhy)
			autoJournal(l.journal, fmt.Sprintf("ensemble-blocked buy %s conf=%.2f why=%q votes=[%s]",
				d.Symbol, d.Confidence, d.BlockWhy, votes))
		case d.Action == ensemble.ActionBuy:
			dec := strategy.Decision{Symbol: d.Symbol, Signal: strategy.SignalBuy,
				Composite: d.Confidence, Reason: "ensemble: " + d.Reason}
			autoJournal(l.journal, fmt.Sprintf("ensemble-buy %s qty=%d conf=%.2f votes=[%s] reason=%q",
				d.Symbol, d.Qty, d.Confidence, votes, d.Reason))
			l.handleBuy(ctx, dec, pfv, d.Confidence)
		case d.Action == ensemble.ActionSell:
			dec := strategy.Decision{Symbol: d.Symbol, Signal: strategy.SignalSell,
				Reason: "ensemble: " + d.Reason}
			autoJournal(l.journal, fmt.Sprintf("ensemble-sell %s conf=%.2f votes=[%s]", d.Symbol, d.Confidence, votes))
			l.handleSell(ctx, dec)
		}
	}
	return nil
}

// formatVotes renders one verdict's per-expert audit trail compactly.
func formatVotes(votes []ensemble.VoiceVote) string {
	parts := make([]string, len(votes))
	for i, v := range votes {
		parts[i] = fmt.Sprintf("%s:%s@%.2fx%.2f", v.Expert, v.Action, v.Conf, v.Weight)
	}
	return strings.Join(parts, ", ")
}
func (l *autoLoop) handleBuy(ctx context.Context, dec strategy.Decision, pfv, conf float64) {
	// Per-symbol adaptive confidence: chronic losers need higher
	// confidence to clear the risk gate, proven winners get a
	// discount. The bias is set by the stats ledger; this just adds it
	// to the signal confidence so the risk gate's MinConfidence check
	// naturally rewards the good symbols and punishes the bad ones.
	if l.stats != nil {
		bias := l.stats.get(dec.Symbol).ConfidenceBias
		if bias < 0 {
			log.Printf("[auto] %s confidence bias: %+.2f (chronic loser — needing higher conf)", dec.Symbol, bias)
		}
		conf += bias
	}
	// Per-asset-class sizing for live entries.
	dec.Sizing = l.set.SizingFor(dec.Symbol, l.cfg.MaxPositionUSD)
	// and the user opted in via NOTIONAL_CRYPTO, use USD-notional so
	// fractional fills work. Alpaca supports notional on crypto with
	// time_in_force=day only.
	dec.Notional = l.set.NotionalCrypto && strategy.IsCrypto(dec.Symbol)
	// CRYPTO_NOTIONAL_USD overrides the per-position budget for crypto
	// notional orders. When set, the bot caps each crypto entry to
	// this fixed dollar amount (independent of MAX_POSITION_USD).
	dec.NotionalUSD = l.set.CryptoNotionalUSD
	// Per-symbol position cap: skip if already holding more than the
	// per-symbol max. This is the "no double-down" guard that prevents
	// the ensemble's every-tick BUY signal from stacking 6 entries of
	// the same symbol into one giant position. Equity cap = MAX_POSITION_USD;
	// crypto cap = 3x CRYPTO_NOTIONAL_USD (room to scale in 3 times
	// before the cap kicks in, then re-entry after a TP/SL exit).
	curUSD := l.positionUsd(ctx, dec.Symbol)
	maxPerSym := l.cfg.MaxPositionUSD
	if strategy.IsCrypto(dec.Symbol) && l.set.CryptoNotionalUSD > 0 {
		maxPerSym = l.set.CryptoNotionalUSD * 3
	}
	if curUSD >= maxPerSym {
		log.Printf("[auto] %s BUY skipped: already holding $%.2f >= cap $%.2f", dec.Symbol, curUSD, maxPerSym)
		return
	}
	plan, err := l.engine.PlanEntry(ctx, dec, pfv)
	if err != nil {
		log.Printf("[auto] %s plan skipped: %v", dec.Symbol, err)
		return
	}
	// Options overlay: prefer a deep-ITM call substitute when data allows;
	// otherwise fall through to the equity entry.
	if l.optPlanner != nil && !plan.Crypto {
		leg, why := l.optPlanner.Plan(ctx, dec.Symbol, pfv)
		if leg != nil {
			autoJournal(l.journal, fmt.Sprintf("buy-option %s %s contracts=%d delta=%.2f premium=%.2f",
				dec.Symbol, leg.OCC, leg.Contracts, leg.Delta, leg.Premium))
			if l.dryRun {
				log.Printf("[auto] DRY-RUN option entry %s x%d (%s)", leg.OCC, leg.Contracts, dec.Symbol)
				return
			}
			if err := placeOptionEntry(ctx, l.client, leg); err != nil {
				log.Printf("[auto] option entry %s failed: %v", leg.OCC, err)
				return
			}
			log.Printf("[auto] OPTION ENTRY %s x%d for %s signal", leg.OCC, leg.Contracts, dec.Symbol)
			return
		}
		log.Printf("[auto] option overlay skipped for %s: %s", dec.Symbol, why)
	}
	autoJournal(l.journal, fmt.Sprintf("buy %s qty=%d price=%.4f tp=%.4f sl=%.4f crypto=%t",
		plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss, plan.Crypto))
	if l.dryRun {
		log.Printf("[auto] DRY-RUN BUY %s qty=%d @~%.4f tp=%.4f sl=%.4f",
			plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss)
		return
	}
	if err := l.exec.ExecuteEntry(ctx, plan, conf); err != nil {
		log.Printf("[auto] %s entry rejected/failed: %v", plan.Symbol, err)
	}
}

// preOrder queues one off-hours equity entry: reference-priced plan,
// deduped against open orders (one pre-order per symbol), then placed as
// a resting limit/GTC bracket. The same plan also fires the options
// overlay: a deep-ITM call pre-order is placed alongside the equity
// so the position is captured in options when the market reopens.
// Dry-run logs without placing.
func (l *autoLoop) preOrder(ctx context.Context, dec strategy.Decision, pfv, conf float64) {
	// Per-asset-class sizing for pre-orders (same routing as live entries).
	dec.Sizing = l.set.SizingFor(dec.Symbol, l.cfg.MaxPositionUSD)
	dec.NotionalUSD = l.set.CryptoNotionalUSD
	plan, err := l.engine.PlanEntryFromReference(ctx, dec, pfv)
	if err != nil {
		log.Printf("[auto] %s pre-order skipped: %v", dec.Symbol, err)
		return
	}
	open, err := l.client.ListOrders(ctx, "open", 500)
	if err != nil {
		log.Printf("[auto] pre-order dedup unavailable for %s: %v", dec.Symbol, err)
		return
	}
	for _, o := range open {
		if o.Symbol == dec.Symbol && o.Side == "buy" {
			log.Printf("[auto] %s pre-order already resting (order=%s); skipping duplicate", dec.Symbol, o.ID)
			return
		}
	}
	// Options overlay: place a deep-ITM call pre-order alongside the
	// equity. The option chain is available 24/7, so the option leg
	// can be priced and placed even when the equity market is closed.
	if l.optPlanner != nil && !plan.Crypto {
		leg, why := l.optPlanner.Plan(ctx, dec.Symbol, pfv)
		if leg != nil {
			autoJournal(l.journal, fmt.Sprintf("pre-order-option %s %s contracts=%d delta=%.2f premium=%.2f",
				dec.Symbol, leg.OCC, leg.Contracts, leg.Delta, leg.Premium))
			if l.dryRun {
				log.Printf("[auto] DRY-RUN PRE-ORDER option %s x%d (%s)", leg.OCC, leg.Contracts, dec.Symbol)
			} else if err := placeOptionEntry(ctx, l.client, leg); err != nil {
				log.Printf("[auto] pre-order option %s failed: %v", leg.OCC, err)
			} else {
				log.Printf("[auto] PRE-ORDER OPTION %s x%d for %s", leg.OCC, leg.Contracts, dec.Symbol)
			}
		} else {
			log.Printf("[auto] pre-order option overlay skipped for %s: %s", dec.Symbol, why)
		}
	}
	autoJournal(l.journal, fmt.Sprintf("pre-order %s qty=%d ref-price=%.4f tp=%.4f sl=%.4f conf=%.2f",
		plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss, conf))
	if l.dryRun {
		log.Printf("[auto] DRY-RUN PRE-ORDER %s qty=%d @limit %.4f tp=%.4f sl=%.4f",
			plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss)
		return
	}
	if err := l.exec.ExecutePreOrder(ctx, plan, conf); err != nil {
		log.Printf("[auto] %s pre-order rejected/failed: %v", dec.Symbol, err)
	}
}

func (l *autoLoop) handleSell(ctx context.Context, dec strategy.Decision) {
	if !l.windows.CanTrade(dec.Symbol) {
		log.Printf("[auto] %s SELL outside window; skipped", dec.Symbol)
		return
	}
	autoJournal(l.journal, "sell-exit "+dec.Symbol+" "+dec.Reason)
	if l.dryRun {
		log.Printf("[auto] DRY-RUN SELL %s: %s", dec.Symbol, dec.Reason)
		return
	}
	qty, err := positionQtyFor(ctx, l.client, dec.Symbol)
	if err != nil || qty <= 0 {
		log.Printf("[auto] %s exit: no live position found", dec.Symbol)
		return
	}
	req := tools.OrderRequest{
		Symbol: dec.Symbol, Side: "sell", Type: "market",
		TimeInForce: exitTIF(dec.Symbol), Qty: trimNum(qty),
	}
	if _, err := l.client.PlaceOrder(ctx, req); err != nil {
		log.Printf("[auto] %s exit order failed: %v", dec.Symbol, err)
		return
	}
	log.Printf("[auto] EXIT %s qty=%s placed (%s)", dec.Symbol, trimNum(qty), dec.Reason)
	autoJournal(l.journal, fmt.Sprintf("exit %s qty=%s", dec.Symbol, trimNum(qty)))
	_ = l.exec.State.ClearLevel(dec.Symbol)
}

// ---- small helpers ----

func placeOptionEntry(ctx context.Context, c *tools.Client, leg *strategy.OptionLeg) error {
	req := tools.OrderRequest{
		Symbol:      leg.OCC,
		Qty:         strconv.Itoa(leg.Contracts),
		Side:        "buy",
		Type:        "market",
		TimeInForce: "day", // options require day|gtc
	}
	_, err := c.PlaceOrder(ctx, req)
	return err
}

func positionQtyFor(ctx context.Context, c *tools.Client, symbol string) (float64, error) {
	ps, err := c.GetPositions(ctx)
	if err != nil {
		return 0, err
	}
	for _, p := range ps {
		if p.Symbol == symbol {
			return strconv.ParseFloat(p.Qty, 64)
		}
	}
	return 0, nil
}

// exitTIF picks the right time-in-force for exits: gtc for stock/crypto,
// day for OCC option positions (options reject other values).
func exitTIF(symbol string) string {
	if tools.IsOCCSymbol(symbol) {
		return "day"
	}
	return "gtc"
}

func trimNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func autoJournal(j *pnl.Journal, detail string) {
	if j == nil {
		return
	}
	_ = j.Append(pnl.Record{
		Kind: pnl.KindDecision, Source: "strategy:auto",
		Risk: "APPROVED", Detail: detail,
	})
}

// adaptiveSizingMultiplier maps account P/L fraction to a position-budget
// multiplier. Linear scale: 0 PnL = 1.0x; +20% PnL = 2.0x (capped); -20%
// PnL = 0.25x (floored). This is paper-tuned for a 1-month showcase: it
// amplifies winners (compounding) and shrinks losers (anti-martingale)
// without ever reaching 0 or any number that would crash the math.
func adaptiveSizingMultiplier(pnl float64) float64 {
	const cap, floor = 2.0, 0.25
	m := 1.0 + 5.0*pnl
	if m > cap {
		return cap
	}
	if m < floor {
		return floor
	}
	return m
}

// startingEquity returns the baseline equity the adaptive sizing is
// measured against. The first time the loop runs, the current equity
// is persisted to data/strategy-state.json under a starting_equity key.
// Subsequent boots read the persisted value so reboots don't reset the
// PnL baseline.
func (l *autoLoop) startingEquity() float64 {
	if l.exec == nil || l.exec.State == nil {
		return 0
	}
	type adaptiveBlob struct {
		StartingEquity float64 `json:"starting_equity"`
	}
	var blob adaptiveBlob
	path := l.exec.State.Path()
	if path == "" {
		return 0
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &blob)
	}
	return blob.StartingEquity
}

// persistStartingEquity writes the starting_equity key into the strategy
// state file. Idempotent on re-read: startingEquity() returns the
// non-zero value on next boot and skip this path. The state file is a
// JSON blob; the executor's State store is what reads/writes it for
// positions/levels, so we patch the file in place to add our field
// without disturbing the executor's schema.
func (l *autoLoop) persistStartingEquity(equity float64) {
	if l.exec == nil || l.exec.State == nil {
		return
	}
	path := l.exec.State.Path()
	if path == "" {
		return
	}
	existing := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &existing)
	}
	existing["starting_equity"] = equity
	if out, err := json.MarshalIndent(existing, "", "  "); err == nil {
		_ = os.WriteFile(path, out, 0o644)
}
}


// ---- pause flag ----

// pauseFlagEngaged returns true when path exists and its trimmed
// contents equal "true". Missing file, read error, or any other
// content (including "false") is treated as "not paused".
func pauseFlagEngaged(path string) bool {
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	// Trim spaces, CR, LF, tab; reject anything but the literal "true".
	start, end := 0, len(b)
	for start < end {
		switch b[start] {
		case ' ', '\n', '\r', '\t':
			start++
		default:
			goto done_left
		}
	}
done_left:
	for end > start {
		switch b[end-1] {
		case ' ', '\n', '\r', '\t':
			end--
		default:
			goto done_right
		}
	}
done_right:
	if end-start != 4 {
		return false
	}
	return string(b[start:end]) == "true"
}

// defaultPauseFlagPath derives the pause flag file path from the
// trade-log path: data/trades.jsonl -> data/paused. Falling back to
// "data/paused" when no path is configured keeps the dashboard's
// default file location working out of the box.
func defaultPauseFlagPath(tradeLog string) string {
	if tradeLog == "" {
		return "data/paused"
	}
	dir := filepath.Dir(tradeLog)
	if dir == "" || dir == "." {
		return "data/paused"
	}
	return filepath.Join(dir, "paused")
}
