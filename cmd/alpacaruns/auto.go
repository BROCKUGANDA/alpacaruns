// auto.go implements `alpacaruns auto`: the deterministic no-LLM trading
// loop. Every POLL_SECONDS it scores the symbol universe with
// factors.Engine, applies the threshold rule, sizes entries fixed-
// fractionally and routes orders through the same risk validator
// (agents.NewValidator) as every other path, then journals each decision.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/a2a"
	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/factors"
	"github.com/BROCKUGANDA/alpacaruns/options"
	"github.com/BROCKUGANDA/alpacaruns/picker"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/refdata"
	"github.com/BROCKUGANDA/alpacaruns/selfheal"
	"github.com/BROCKUGANDA/alpacaruns/store"
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
	dryRun     bool
	// pauseFlagPath is the path to the JSON pause flag file (defaults
	// to data/paused). Kept on the struct so tests can point it at a
	// temp dir without reaching for an env var.
	pauseFlagPath string
	// pausedLogged remembers whether we already printed the
	// "[control] paused" line; we don't spam it on every tick.
	pausedLogged bool
	// haltLogged does the same for the kill-switch notice: one line when
	// the halt engages, not one per tick for the rest of the process's
	// life. The halt no longer exits the loop, so monitoring continues.
	haltLogged bool
	// venueChecked records that the account was proven to belong to the
	// venue the base URL claims. Checked once, before the first order.
	venueChecked bool
	// overridesAt is the mtime of the dashboard's config-overrides file
	// when it was last applied, so the file is re-read only when it changes.
	overridesAt time.Time
	// fillBackfilled is set once a reconciliation pass has actually read the
	// broker, which is what downgrades the first-pass wide window to the
	// per-tick one.
	fillBackfilled bool
	// mirror is the SQLite view of the journal and state, opened lazily and
	// verified on a cadence. nil when SQLITE_MIRROR=false.
	mirror *store.DB
	// ensemble is non-nil only when ENSEMBLE_ENABLED=true; nil keeps the
	// original single-expert tick path bit-for-bit.
	ensemble *ensemble.Runner
	// Intraday track (INTRADAY_TRACK != off): independent 15-minute-bar
	// engine, own execution windows and sizing; shadow mode logs only.
	intra    *strategy.Engine
	intraWin *strategy.TradingWindows
	// stats is the per-symbol win/loss ledger (see stats.go). Read on
	// every tick to gate entries via ConfidenceBias; written whenever
	// closePosition fires and the trade log shows a closed round-trip.
	stats *statsLedger
	// tickNum is a monotonic loop counter persisted via State.Heartbeat
	// so the separate `serve` API process can show liveness / tick count
	// without any in-process sharing.
	tickNum int64
	// posCache memoizes the broker /positions snapshot for the duration
	// of ONE tick (invalidated at tick start). Without it, every buy
	// decision in a tick re-fetches the full position list (the
	// per-symbol cap check), which multiplies Alpaca calls by universe
	// size — exactly how a small account trips the API rate limit and
	// starves the price/bar fetches that matter.
	posCache []tools.Position
	// bridge is the agent-to-agent bridge: inbound signals from peer
	// agents, evaluated by the deterministic risk gate. nil when the
	// bridge is disabled.
	bridge *a2a.Bridge
	// book is the live view of the broker's positions that the risk gate
	// reads through. It is invalidated after every mutation this loop
	// makes, which is what lets one tick place two buys of the same symbol
	// without the second one sizing against a book it already changed.
	book *liveBook
	// registry is the classified universe (equities, ETFs, index
	// trackers, crypto) built by the universal signal picker. It is
	// refreshed on a timer because the broker asset catalogue is a
	// large, rarely-changing payload.
	registry *picker.Registry
	// registryAt is when the registry was last refreshed.
	registryAt time.Time
	// learn is the learner's ledger: the bounded adjustments it has
	// earned from closed trades, and the cadence for recomputing them.
	// Always constructed (nil only on a config error) so the evidence
	// accumulates even while LEARN_ENABLED is off.
	learn *learningLedger
	// statsClassed marks that the ledger has been rebuilt once with the
	// picker's real asset classes available, so a restarted bot files
	// historical trades under the same classes as new ones.
	statsClassed bool
	// healOpts / healDeps wire the self-heal pass. It runs on a tick
	// cadence, not every tick: the checks are broker-wide reads and the
	// states they look for do not change between two 5-minute polls.
	healOpts   selfheal.Options
	healDeps   selfheal.Deps
	healEvery  int64
	healAtTick int64
	healLast   selfheal.Report
	// healStore persists the last report for the dashboard API, which is a
	// separate process and cannot see the loop's memory.
	healStore *selfheal.ReportStore
}

// registryTTL is how long the classified universe is reused before it
// is rebuilt from the broker's asset catalogue.
const registryTTL = time.Hour

// universe returns the symbols the auto loop scores this tick. It
// covers every configured class: equities, ETFs, index trackers and
// crypto. Options are handled by the overlay (an option has no series
// of its own — its signal is the underlying's).
func (l *autoLoop) universe(ctx context.Context) []string {
	reg, err := l.refreshRegistry(ctx)
	if err != nil || reg == nil {
		// Fall back to the flat configured lists so a catalogue outage
		// never stops the bot from trading what it always traded.
		out := append([]string{}, l.set.EquitySymbols...)
		out = append(out, l.set.ETFSymbols...)
		out = append(out, l.set.IndexSymbols...)
		out = append(out, l.set.CryptoSymbols...)
		return out
	}
	return reg.TradableSymbols()
}

// refreshRegistry returns the classified universe, rebuilding it when
// it is missing or stale.
func (l *autoLoop) refreshRegistry(ctx context.Context) (*picker.Registry, error) {
	if l.registry != nil && time.Since(l.registryAt) < registryTTL {
		return l.registry, nil
	}
	spec := picker.Spec{
		Equities: l.set.EquitySymbols,
		ETFs:     l.set.ETFSymbols,
		Indices:  l.set.IndexSymbols,
		Crypto:   l.set.CryptoSymbols,
	}
	reg, err := picker.Build(ctx, l.client, spec)
	if err != nil {
		return nil, err
	}
	l.registry = reg
	l.registryAt = time.Now()
	// A symbol the broker will not trade is dropped from scoring, and said
	// out loud: a configured ticker that has been delisted, halted or
	// renamed otherwise just stops appearing in decisions, which looks
	// identical to "no signal today".
	// Reference data is versioned so a disappearance or a tradability flip is
	// a reported event rather than an absence of signals. This is the only
	// record of what the broker said yesterday.
	l.recordReferenceData(reg)
	if bad := reg.Untradable(); len(bad) > 0 {
		names := make([]string, 0, len(bad))
		for _, i := range bad {
			names = append(names, i.Symbol)
		}
		log.Printf("[auto] excluded %d configured symbol(s) the broker will not trade: %v", len(bad), names)
	}
	return reg, nil
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
	defer loop.closeMirror()

	log.Printf("[auto] deterministic loop start: equities=%v etfs=%v indices=%v crypto=%v options=%v poll=%ds dry-run=%t",
		loop.set.EquitySymbols, loop.set.ETFSymbols, loop.set.IndexSymbols,
		loop.set.CryptoSymbols, loop.set.OptionUnderlyings, loop.set.PollSeconds, dryRun)

	for {
		if ctx.Err() != nil {
			log.Printf("[auto] stopped (signal)")
			return 0
		}
		// A kill switch halt used to stop this process. That was the most
		// dangerous line in the loop: with Restart=always the unit comes
		// back 10 seconds later with a FRESH switch, so the bot spends its
		// life cycling — and between the exit and the restart nothing
		// enforces crypto TP/SL, rebuilds missing protection or watches
		// drawdown. A halt means "stop taking risk", which the tick
		// enforces; it never meant "stop protecting what is open".
		if loop.exec.Kill.Halted() && !loop.haltLogged {
			log.Printf("[auto] HALTED: new entries suppressed, protective monitoring continues")
			loop.haltLogged = true
		}
		if err := loop.safeTick(ctx); err != nil {
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

// safeTick runs one tick behind a panic barrier. The auto loop is the
// long-running 24/7 process: a panic in one tick (nil map, index math,
// a broker payload shape we didn't expect) must degrade to a logged
// error and the NEXT tick — never tear the process down and leave
// positions unmonitored. The panic value is logged with the symbol of
// the failing tick's stack so the operator can reproduce.
func (l *autoLoop) safeTick(ctx context.Context) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[auto] PANIC recovered in tick: %v\n%s", rec, stackTrace())
			err = fmt.Errorf("tick panicked: %v", rec)
		}
	}()
	return l.tick(ctx)
}

// stackTrace renders the current goroutine stack at panic depth so the
// log names the failing function, not just the panic value.
func stackTrace() string {
	buf := make([]byte, 8*1024)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
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
	// The gate reads the book through a short-TTL live view. Without this
	// wiring the validator's additive-exposure check was skipped on exactly
	// the unattended path, so every percentage cap was evaluated against
	// the order alone: a book of twenty positions each under the cap passed
	// with no check ever summing them.
	book := newLiveBook(client)
	val.Exposure = func(sym string) (float64, error) {
		return book.exposureUSD(context.Background(), sym)
	}
	val.Holdings = func(sym string) (float64, error) {
		return book.signedQty(context.Background(), sym)
	}

	engine, err := strategy.NewEngine(strategy.EngineConfig{
		Scorer:        scorer,
		Prices:        strategy.NewPriceSource(client),
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
		// Bracket mode is a per-engine strategy knob: the swing engine
		// must honor BRACKET_MODE=atr exactly like the intraday engine —
		// previously only the intraday engine received these fields, so
		// ATR-based breathing stops silently never applied to the main
		// swing book (it always ran fixed pct brackets).
		BracketMode: set.BracketMode,
		ATRMultTP:   set.ATRMultTP, ATRMultSL: set.ATRMultSL,
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
		// Daily bars for ATR-scaled levels. The reconciliation pass needs
		// them to size a stop off realized volatility instead of a fixed
		// percentage, including for positions this bot did not open.
		Bars: bars,
	}

	loop := &autoLoop{
		cfg: cfg, set: set, engine: engine, windows: windows, exec: exec,
		monitor: monitor, optPlanner: planner, journal: j, client: client,
		// book backs the gate's live view of the account; every mutation
		// this loop makes invalidates it so the next cap check is honest.
		book: book,

		dryRun:        dryRun,
		pauseFlagPath: defaultPauseFlagPath(cfg.TradeLog),
	}

	// Agent-to-agent bridge: enabled only when A2A_ENABLED=true and a
	// token is configured. The bridge state lives next to the trade log
	// so the dashboard API and the bot agree on the ramp position.
	if cfg.A2AEnabled {
		bridge := a2a.NewBridge(a2a.Config{
			Enabled:           cfg.A2AEnabled,
			Token:             cfg.A2AToken,
			Agents:            cfg.A2AAgents,
			DefaultMode:       a2a.Mode(cfg.A2AMode),
			ShadowPasses:      cfg.A2AShadowPasses,
			MaxNotional:       cfg.A2AMaxNotional,
			MaxSignalsPerHour: 60,
			PeerTokens:        cfg.A2APeerTokens,
		}, a2a.NewStore(j.Path()))
		loop.bridge = bridge
		log.Printf("[a2a] bridge enabled: agents=%v mode=%s shadow_passes=%d max_notional=%.2f",
			cfg.A2AAgents, cfg.A2AMode, cfg.A2AShadowPasses, cfg.A2AMaxNotional)
	}

	// Per-symbol win/loss ledger. Load existing data; rebuild from the
	// journal so historical trades carry over across restarts.
	loop.stats = newStatsLedger(j.Path())
	if err := loop.stats.load(); err != nil {
		log.Printf("[auto] WARNING: stats load failed: %v", err)
	}
	if err := loop.stats.refreshFromJournalWith(j, loop.classResolver()); err != nil {
		log.Printf("[auto] WARNING: stats refresh failed: %v", err)
	}
	if all := loop.stats.all(); len(all) > 0 {
		log.Printf("[auto] trade stats loaded: %s", loop.stats.formatDump())
	}

	// Learner. Built unconditionally: the samples are already in the
	// ledger, and running the engine while LEARN_ENABLED=false produces
	// the evidence without acting on it — which is what makes turning it
	// on a considered decision instead of a leap.
	learn, err := newLearningLedger(cfg, j.Path())
	if err != nil {
		log.Printf("[auto] learning disabled: %v", err)
	} else {
		loop.learn = learn
		adj := learn.snapshot()
		if adj.Active {
			log.Printf("[learn] restored %d learned adjustments over %d closed trades (floor +%.3f, quarantined=%d)",
				len(adj.Classes), adj.SamplesSeen, adj.ScoreFloorAdd, len(adj.Quarantined))
		} else {
			log.Printf("[learn] learner armed but inactive: %d closed trades, needs %d (enabled=%t)",
				adj.SamplesSeen, learn.eng.Config().MinTrades, learn.eng.Config().Enabled)
		}
	}

	// Self-heal: the pass that finds and fixes the states this bot falls
	// into on its own. Enabled by default because the most common one —
	// a protective order resting over no position — is a short sale in
	// waiting, not a cosmetic problem.
	loop.healOpts = selfheal.Options{
		ReportOnly:      cfg.SelfHealReportOnly,
		HaltLiftEnabled: cfg.SelfHealLiftHalt,
		OrphanMaxAge:    time.Duration(cfg.SelfHealOrphanHours) * time.Hour,
	}
	loop.healEvery = int64(cfg.SelfHealEveryTicks)
	if loop.healEvery < 1 {
		loop.healEvery = 1
	}
	loop.healStore = selfheal.NewReportStore(j.Path())
	if cfg.SelfHealEnabled {
		loop.healDeps = selfheal.Deps{
			Broker:      selfHealBroker{client: client},
			StatePath:   state.Path(),
			JournalPath: j.Path(),
			Halted:      kill.Halted,
			ResetPeak:   loop.resetDrawdownPeak,
			Equity:      loop.accountEquity,
		}
		log.Printf("[heal] self-heal enabled: every %d tick(s), report-only=%t, halt-lift=%t, orphan age=%s",
			loop.healEvery, loop.healOpts.ReportOnly, loop.healOpts.HaltLiftEnabled, loop.healOpts.OrphanMaxAge)
	}
	// Intraday track: independent 15-minute-bar engine with its own
	// windows and sizing. Shares client, validator, kill switch, state
	// store and journal with the swing path — one risk posture.
	if set.IntradayTrack == "shadow" || set.IntradayTrack == "live" {
		intraScorer := strategy.FactorScorer{Inner: factors.NewEngine(cfg, bars, nil,
			factors.Options{Timeframe: "15Min", MomentumDays: 16, VolWindow: 20})}
		intraEngine, err := strategy.NewEngine(strategy.EngineConfig{
			Scorer:        intraScorer,
			Prices:        strategy.NewPriceSource(client),
			ReferenceBars: bars,
			Threshold: strategy.Thresholds{
				FactorMinScore: cfg.FactorMinScore,
				TrendBuy:       set.TrendBuy,
				MomentumBuy:    set.MomentumBuy,
				ExitComposite:  set.ExitComposite,
				ExitMomentum:   set.ExitMomentum,
			},
			Sizing: strategy.Sizing{PositionPct: set.PositionPctIntraday, MaxPositionUSD: cfg.MaxPositionUSD},
			TPPct:  set.IntradayTPPct, SLPct: set.IntradaySLPct,
			BracketMode: set.BracketMode,
			ATRMultTP:   set.ATRMultTP, ATRMultSL: set.ATRMultSL,
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
	// Heartbeat FIRST (before the pause flag and any Alpaca call). This
	// persists tick counter + timestamp to strategy-state.json every tick
	// so the separate `serve` API process can show liveness and a fresh
	// "last tick" without in-process sharing. Bumping it even when paused
	// means a paused-but-running bot still reports as alive on the
	// dashboard. Best-effort: a failed write must never abort a tick.
	l.tickNum++
	if l.exec != nil && l.exec.State != nil {
		if err := l.exec.State.Heartbeat(l.tickNum); err != nil {
			log.Printf("[auto] heartbeat: %v", err)
		}
	}
	// New tick, new world: drop the previous tick's /positions snapshot
	// so the per-symbol cap sees fills placed since.
	l.posCache = nil
	// Pause flag: when the operator toggles "Pause new trades" in the
	// dashboard (POST /api/control/pause), data/paused is written with
	// content "true". The bot reads the file here and logs once per
	// engagement change. Pause blocks NEW ENTRIES only — the monitor
	// pass (positions, drawdown halts, TP/SL exits, EOD flatten, profit
	// targets) still runs, so a paused bot keeps protecting open
	// positions. The file may be missing or non-"true"; that means
	// running.
	paused := pauseFlagEngaged(l.pauseFlagPath)
	if paused {
		if !l.pausedLogged {
			log.Printf("[control] paused: data/paused=true; new entries skipped, monitor loop alive")
			l.pausedLogged = true
		}
	} else if l.pausedLogged {
		log.Printf("[control] resumed: data/paused cleared; entries re-enabled")
		l.pausedLogged = false
	}
	if l.client == nil {
		return fmt.Errorf("auto: no broker client configured")
	}
	// Operator knobs saved in the dashboard take effect from this tick on.
	l.applyConfigOverrides()
	acct, err := l.client.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("account: %w", err)
	}
	// Prove the venue before the first order, not just that the URL string
	// looks right. A paper URL plus live keys passes every other check in
	// this process; this is the one that would catch it. A mismatch halts
	// the switch — the bot keeps monitoring, but takes on nothing new.
	if !l.venueChecked {
		inside, verr := l.client.VerifyVenue(ctx, tools.ExpectPaperVenue(l.cfg.AlpacaBaseURL))
		if verr != nil {
			log.Printf("[auto] VENUE CHECK FAILED: %v", verr)
			if l.exec != nil && l.exec.Kill != nil {
				l.exec.Kill.Halt()
			}
			return fmt.Errorf("venue verification: %w", verr)
		}
		l.venueChecked = true
		log.Printf("[auto] venue verified: account %s (%s) is the paper account the URL claims",
			inside.ID, inside.AccountNumber)
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

	positions, err := l.positionsCached(ctx)
	if err != nil {
		return fmt.Errorf("positions: %w", err)
	}
	// Self-heal before anything else reasons about the book: an orphan
	// protective order resting over no position is a short sale waiting
	// to fill, and the monitor should not have to work around it.
	l.maybeHeal(ctx)
	// Recover any fill the broker has but the journal does not. This runs
	// BEFORE the drawdown check and the ledger rebuild, because both derive
	// their numbers from those rows: a missing fill is not a missing
	// statistic, it is a halt that will not fire and a learner that will
	// never see the trade.
	l.mirrorTick(ctx)
	if n, err := l.reconcileFills(ctx); err != nil {
		log.Printf("[reconcile] fill reconciliation failed: %v", err)
	} else if n > 0 {
		log.Printf("[reconcile] recovered %d fill(s) the broker reported and the journal did not", n)
	}
	// Then fold newly closed round-trips into the ledger and let the
	// learner recalibrate on its own cadence (every N closed trades, not
	// every tick).
	l.refreshAndLearn(time.Now().UTC())
	held := map[string]bool{}
	posSymbols := []string{}
	for _, p := range positions {
		q, _ := strconv.ParseFloat(p.Qty, 64)
		if q == 0 {
			continue
		}
		// The broker reports crypto in trading form ("BTCUSD") while the
		// strategy universe uses "BTC/USD". Keying the held-map on the
		// broker form meant a crypto position never looked held, so the
		// engine re-bought it every tick and the per-symbol cap never
		// fired. Canonicalise both sides before comparing.
		held[strategy.CanonicalSymbol(p.Symbol)] = true
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

	universe := l.universe(ctx)

	// Freeze the tick's positions view for all per-symbol cap checks in
	// this pass (handleBuy -> positionUsd reads it without re-fetching).
	if _, err := l.positionsCached(ctx); err != nil {
		log.Printf("[auto] positions prefetch failed (cap checks will refetch): %v", err)
	}

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
		if paused {
			log.Printf("[ensemble] tick skipped: paused")
		} else if err := l.tickEnsemble(ctx, universe, held, pfv); err != nil {
			log.Printf("[ensemble] tick error: %v", err)
		}
	}

	// Inbound signals from peer agents are evaluated through the same
	// risk gate as the bot's own decisions. The ramp decides whether
	// they are executed, only shadowed, or refused.
	l.processA2A(ctx, pfv)

	decisions := l.engine.RunTick(ctx, universe, held)
	conf := 1.0 // deterministic engine: confidence IS the factor gate
	for _, dec := range decisions {
		if paused && dec.Signal == strategy.SignalBuy {
			log.Printf("[auto] %s BUY skipped: paused", dec.Symbol)
			continue
		}
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

	// Intraday-book enforcement pass: local crypto TP/SL + bracket
	// verification restricted to Track="intraday" levels. Previously
	// intraday levels were never enforced at all (the prefixed key
	// never matched the broker symbol, so the swing monitor wiped them
	// on the first tick). Halt evaluation stays on the swing pass.
	intraPrices := map[string]float64{}
	if intraHeld := l.intraHeldSymbols(); len(intraHeld) > 0 {
		for sym := range intraHeld {
			if px, err := l.intra.PriceOf(ctx, sym); err == nil {
				intraPrices[sym] = px
			}
		}
		if _, err := l.monitor.TickTrack(ctx, intraPrices, 0, "intraday"); err != nil {
			log.Printf("[intraday] monitor tick: %v", err)
		}
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
	if l.exec != nil && l.exec.Kill != nil && l.exec.Kill.Halted() {
		return // halted: no new risk, in either book
	}
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
	// The learner gates and sizes intraday entries too: a faster book is
	// not a reason to ignore what the same symbol just did in the swing
	// book, and the samples are shared.
	if ok, why := l.learningGate(dec.Symbol, dec.Composite); !ok {
		log.Printf("[intraday] %s entry skipped: %s", dec.Symbol, why)
		return
	}
	if whyPlan, okPlan := l.learningPlan(&plan); !okPlan {
		log.Printf("[intraday] %s entry skipped: %s", dec.Symbol, whyPlan)
		return
	} else if whyPlan != "" {
		log.Printf("[intraday] %s %s", dec.Symbol, whyPlan)
	}
	autoJournal(l.journal, fmt.Sprintf("intraday-buy %s qty=%d price=%.4f tp=%.4f sl=%.4f",
		plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss),
		plan.Symbol, "buy", 0)
	if l.set.IntradayTrack == "shadow" {
		log.Printf("[intraday] SHADOW BUY %s qty=%d @~%.4f tp=%.4f sl=%.4f",
			plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss)
		return
	}
	if err := l.submitEntry(ctx, plan, 1.0); err != nil {
		log.Printf("[intraday] %s entry rejected/failed: %v", plan.Symbol, err)
		return
	}
	// Levels are stored under the PLAIN symbol with Track="intraday".
	// The old "intraday:"-prefixed key never matched the broker symbol
	// in the monitor's position lookup, so the level was wiped on the
	// first monitor tick after entry — disabling intraday TP/SL and
	// emptying the held-map (which allowed immediate re-buys).
	_ = l.State().SetLevel(strategy.PositionLevels{
		Symbol: plan.Symbol, EntryPrice: plan.Price,
		TakeProfit: plan.Brackets.TakeProfit, StopLoss: plan.Brackets.StopLoss,
		Qty: trimNum(float64(plan.Qty)), Since: time.Now().UTC(),
		Track: "intraday",
	})
}

// intraExit closes an intraday position by symbol (live mode only).
// intraExit closes one intraday-book position through the same exit
// authority as the swing book: fresh position read, protection cleared
// first, gate proven reducing, deterministic id.
func (l *autoLoop) intraExit(ctx context.Context, dec strategy.Decision, why string) {
	autoJournal(l.journal, fmt.Sprintf("intraday-sell %s (%s)", dec.Symbol, why), dec.Symbol, "sell", 0)
	if l.set.IntradayTrack == "shadow" {
		log.Printf("[intraday] SHADOW SELL %s (%s)", dec.Symbol, why)
		return
	}
	done, err := l.exec.ExecuteReduce(ctx, dec.Symbol, "intraday: "+why)
	l.book.Invalidate()
	if err != nil {
		log.Printf("[intraday] %s close refused: %v", dec.Symbol, err)
		return
	}
	if !done {
		// Flat: drop the level so the held-map stops reporting a position
		// that is not there (a stale level re-buys on the next tick).
		_ = l.State().ClearLevelTrack(dec.Symbol, "intraday")
		return
	}
	log.Printf("[intraday] EXIT %s placed (%s)", dec.Symbol, why)
	_ = l.State().ClearLevelTrack(dec.Symbol, "intraday")
}

// flattenIntraday closes every open intraday-book position (EOD).
func (l *autoLoop) flattenIntraday(ctx context.Context) error {
	st, err := l.State().Load()
	if err != nil {
		return err
	}
	for _, lv := range st.Levels {
		if lv.Track != "intraday" {
			continue
		}
		// lv.Symbol, not the map key: the key carries the track suffix
		// ("AAPL|intraday"), which is not a symbol the broker knows.
		l.intraExit(ctx, strategy.Decision{Symbol: lv.Symbol}, "EOD flatten")
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
	for _, lv := range st.Levels {
		if lv.Track == "intraday" {
			held[strategy.CanonicalSymbol(lv.Symbol)] = true
		}
	}
	return held
}

func (l *autoLoop) State() *strategy.StateStore { return l.exec.State }

// positionsCached returns the tick-scoped /positions snapshot, fetching
// it at most once per tick. Callers that need broker-fresh data (fills
// just placed) must call l.client.GetPositions directly.
func (l *autoLoop) positionsCached(ctx context.Context) ([]tools.Position, error) {
	if l.posCache != nil {
		return l.posCache, nil
	}
	pos, err := l.client.GetPositions(ctx)
	if err != nil {
		return nil, err
	}
	l.posCache = pos
	return pos, nil
}

// positionUsd returns the live market value (current_price * abs(qty))
// of the open position for symbol, or 0 when flat / lookup fails.
// Used by the per-symbol position cap to detect "no double-down" and
// by the profit-target and time-based close logic to size exits.
func (l *autoLoop) positionUsd(ctx context.Context, symbol string) float64 {
	pos, err := l.positionsCached(ctx)
	if err != nil {
		return 0
	}
	for _, p := range pos {
		if !tools.SameSymbol(p.Symbol, symbol) {
			continue
		}
		q, _ := strconv.ParseFloat(p.Qty, 64)
		px, _ := strconv.ParseFloat(p.CurrentPrice, 64)
		return math.Abs(q * px)
	}
	return 0
}

// closePosition exits a symbol from a scheduled policy: the max-hold
// close and the profit-target rebalancer. It delegates the order to the
// exit authority (Executor.ExecuteReduce) rather than assembling one,
// because this path used to read its quantity from the tick-frozen
// position cache — the same tick's earlier monitor pass or profit-take
// could already have reduced or closed that position, and the stale number
// was sent as a market sell against a smaller holding, which is an
// unintended short.
func (l *autoLoop) closePosition(ctx context.Context, symbol, reason string) {
	if l.dryRun {
		log.Printf("[auto] DRY-RUN close %s (%s)", symbol, reason)
		return
	}
	done, err := l.exec.ExecuteReduce(ctx, symbol, reason)
	l.book.Invalidate()
	if err != nil {
		log.Printf("[auto] %s close refused: %v", symbol, err)
		return
	}
	if !done {
		return // flat already: nothing to redeploy
	}
	log.Printf("[auto] %s closed (%s)", symbol, reason)

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
	// the universe to redeploy the cash freed by a recent close. Pulls
	// the live account portfolio value to size the new entries.
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
	universe := l.universe(ctx)
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
	for _, lv := range st.Levels {
		if lv.Track == "intraday" {
			continue // intraday book has its own EOD flatten
		}
		if lv.Since.IsZero() || lv.Since.After(cutoff) {
			continue
		}
		l.closePosition(ctx, lv.Symbol, fmt.Sprintf("max-hold-%.0fh", maxHours))
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
				d.Symbol, d.Confidence, d.BlockWhy, votes),
				d.Symbol, "buy", d.Confidence)
		case d.Action == ensemble.ActionBuy:
			dec := strategy.Decision{Symbol: d.Symbol, Signal: strategy.SignalBuy,
				Composite: d.Confidence, Reason: "ensemble: " + d.Reason}
			autoJournal(l.journal, fmt.Sprintf("ensemble-buy %s qty=%d conf=%.2f votes=[%s] reason=%q",
				d.Symbol, d.Qty, d.Confidence, votes, d.Reason),
				d.Symbol, "buy", d.Confidence)
			l.handleBuy(ctx, dec, pfv, d.Confidence)
		case d.Action == ensemble.ActionSell:
			dec := strategy.Decision{Symbol: d.Symbol, Signal: strategy.SignalSell,
				Reason: "ensemble: " + d.Reason}
			autoJournal(l.journal, fmt.Sprintf("ensemble-sell %s conf=%.2f votes=[%s]", d.Symbol, d.Confidence, votes),
				d.Symbol, "sell", d.Confidence)
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
	// A halt suppresses NEW risk. It must never suppress an exit: that is
	// the whole reason the drawdown halt exists.
	if l.exec != nil && l.exec.Kill != nil && l.exec.Kill.Halted() {
		log.Printf("[auto] %s BUY skipped: kill switch halted (entries closed, monitoring live)", dec.Symbol)
		return
	}
	// Market state decides the ENTRY MODE: with PRE_ORDERS enabled an
	// equity entry while the market is closed is queued as a resting
	// limit/GTC bracket at the last close; otherwise a closed market
	// means no entry at all. Crypto trades 24/7, so the equity clock
	// does not gate it.
	if !strategy.IsCrypto(dec.Symbol) {
		open := l.equityMarketOpen(ctx)
		if !open {
			if l.cfg.PreOrders {
				l.preOrder(ctx, dec, pfv, conf)
				return
			}
			log.Printf("[auto] %s BUY skipped: market closed", dec.Symbol)
			return
		}
	}
	// Execution window: entries fire ONLY inside the configured window
	// for the asset class (equities/options -> TRADING_WINDOWS, crypto ->
	// CRYPTO_WINDOWS). The risk gate only knows "market open", so
	// without this check the bot traded at any hour the market was open.
	if l.windows != nil && !l.windows.CanTrade(dec.Symbol) {
		log.Printf("[auto] %s BUY skipped: outside trading window", dec.Symbol)
		return
	}
	// Learning gate: after enough closed trades the learner may quarantine
	// a symbol or raise the score bar above the configured floor. Both are
	// brakes — it can refuse a trade, never authorise one the factors
	// declined.
	if ok, why := l.learningGate(dec.Symbol, dec.Composite); !ok {
		log.Printf("[auto] %s BUY skipped: %s", dec.Symbol, why)
		autoJournal(l.journal, "learning-blocked buy "+dec.Symbol+": "+why, dec.Symbol, "buy", dec.Composite)
		return
	}
	// Per-symbol track record. A chronic loser has its signal confidence
	// REDUCED, so the risk gate's unchanged floor now rejects it. Only the
	// penalty side is applied: the ledger also computes a positive bias for
	// proven winners, and adding that to the signal would let the bot
	// inflate its own score and clear a bar it did not actually meet.
	// Every self-adjusting knob in this system is therefore a brake — the
	// learner's floor rises, the stop tightens, and a bad record costs
	// confidence. Nothing earned by trading can buy more risk.
	if l.stats != nil {
		bias := l.stats.get(dec.Symbol).ConfidenceBias
		if bias < 0 {
			log.Printf("[auto] %s confidence bias: %+.2f (chronic loser — needing higher conf)", dec.Symbol, bias)
			conf += bias
		}
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
	// Learned sizing and stop tightening for this asset class. Shrink and
	// tighten only: a class that has been losing trades smaller and with a
	// stop closer to entry, never larger or wider than the operator set.
	whyPlan, okPlan := l.learningPlan(&plan)
	if !okPlan {
		log.Printf("[auto] %s BUY skipped: %s", dec.Symbol, whyPlan)
		autoJournal(l.journal, "learning-shrunk buy "+dec.Symbol+": "+whyPlan, dec.Symbol, "buy", dec.Composite)
		return
	}
	if whyPlan != "" {
		log.Printf("[auto] %s %s", dec.Symbol, whyPlan)
		autoJournal(l.journal, whyPlan, dec.Symbol, "buy", dec.Composite)
	}
	// Options overlay: prefer a deep-ITM call substitute when data allows;
	// otherwise fall through to the equity entry.
	if l.optPlanner != nil && !plan.Crypto {
		leg, why := l.optPlanner.Plan(ctx, dec.Symbol, pfv)
		if leg != nil {
			leg.Score = dec.Composite
			if m := l.learningSizeMult("option"); m < 1 {
				n := int(math.Floor(float64(leg.Contracts) * m))
				if n < 1 {
					log.Printf("[auto] %s option entry skipped: learning shrank %d contracts to nothing (x%.2f)",
						dec.Symbol, leg.Contracts, m)
					autoJournal(l.journal, fmt.Sprintf(
						"learning-shrunk option %s: %d contracts x%.2f is below one contract", leg.OCC, leg.Contracts, m),
						dec.Symbol, "buy", dec.Composite)
					return
				}
				log.Printf("[auto] %s option contracts x%.2f: %d -> %d", dec.Symbol, m, leg.Contracts, n)
				leg.Contracts = n
			}
			autoJournal(l.journal, fmt.Sprintf("buy-option %s %s contracts=%d delta=%.2f premium=%.2f",
				dec.Symbol, leg.OCC, leg.Contracts, leg.Delta, leg.Premium),
				dec.Symbol, "buy", 0)
			if l.dryRun {
				log.Printf("[auto] DRY-RUN option entry %s x%d (%s)", leg.OCC, leg.Contracts, dec.Symbol)
				return
			}
			if err := l.submitOptionEntry(ctx, leg, conf); err != nil {
				log.Printf("[auto] option entry %s failed: %v", leg.OCC, err)
				return
			}
			log.Printf("[auto] OPTION ENTRY %s x%d for %s signal", leg.OCC, leg.Contracts, dec.Symbol)
			return
		}
		log.Printf("[auto] option overlay skipped for %s: %s", dec.Symbol, why)
	}
	autoJournal(l.journal, fmt.Sprintf("buy %s qty=%d price=%.4f tp=%.4f sl=%.4f crypto=%t",
		plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss, plan.Crypto),
		plan.Symbol, "buy", conf)
	if l.dryRun {
		log.Printf("[auto] DRY-RUN BUY %s qty=%d @~%.4f tp=%.4f sl=%.4f",
			plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss)
		return
	}
	if err := l.submitEntry(ctx, plan, conf); err != nil {
		log.Printf("[auto] %s entry rejected/failed: %v", plan.Symbol, err)
	}
}

// equityMarketOpen reports whether the US equity market is currently
// open, using Alpaca's clock. A clock failure is treated as CLOSED for
// entry purposes: the risk gate still enforces its own market-hours
// check, and failing closed here cannot open a position by accident.
func (l *autoLoop) equityMarketOpen(ctx context.Context) bool {
	cl, err := l.client.GetClock(ctx)
	if err != nil || cl == nil {
		log.Printf("[auto] clock check failed (%v); treating market as closed", err)
		return false
	}
	return cl.IsOpen
}

// preOrder queues one off-hours equity entry: reference-priced plan,
// deduped against open orders (one pre-order per symbol), then placed as
// a resting limit/GTC bracket. The same plan also fires the options
// overlay: a deep-ITM call pre-order is placed alongside the equity
// so the position is captured in options when the market reopens.
// Dry-run logs without placing.
func (l *autoLoop) preOrder(ctx context.Context, dec strategy.Decision, pfv, conf float64) {
	// Same learning gate as live entries: a queued entry is still an
	// entry, and it fills later on a signal the learner may already have
	// judged worthless.
	if ok, why := l.learningGate(dec.Symbol, dec.Composite); !ok {
		log.Printf("[auto] %s pre-order skipped: %s", dec.Symbol, why)
		return
	}
	// Per-asset-class sizing for pre-orders (same routing as live entries).
	dec.Sizing = l.set.SizingFor(dec.Symbol, l.cfg.MaxPositionUSD)
	dec.NotionalUSD = l.set.CryptoNotionalUSD
	plan, err := l.engine.PlanEntryFromReference(ctx, dec, pfv)
	if err != nil {
		log.Printf("[auto] %s pre-order skipped: %v", dec.Symbol, err)
		return
	}
	if whyPlan, okPlan := l.learningPlan(&plan); !okPlan {
		log.Printf("[auto] %s pre-order skipped: %s", dec.Symbol, whyPlan)
		return
	} else if whyPlan != "" {
		log.Printf("[auto] %s %s (pre-order)", dec.Symbol, whyPlan)
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
				dec.Symbol, leg.OCC, leg.Contracts, leg.Delta, leg.Premium),
				dec.Symbol, "buy", conf)
			if l.dryRun {
				log.Printf("[auto] DRY-RUN PRE-ORDER option %s x%d (%s)", leg.OCC, leg.Contracts, dec.Symbol)
			} else if err := l.submitOptionEntry(ctx, leg, conf); err != nil {
				log.Printf("[auto] pre-order option %s failed: %v", leg.OCC, err)
			} else {
				log.Printf("[auto] PRE-ORDER OPTION %s x%d for %s", leg.OCC, leg.Contracts, dec.Symbol)
			}
		} else {
			log.Printf("[auto] pre-order option overlay skipped for %s: %s", dec.Symbol, why)
		}
	}
	autoJournal(l.journal, fmt.Sprintf("pre-order %s qty=%d ref-price=%.4f tp=%.4f sl=%.4f conf=%.2f",
		plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss, conf),
		plan.Symbol, "buy", conf)
	if l.dryRun {
		log.Printf("[auto] DRY-RUN PRE-ORDER %s qty=%d @limit %.4f tp=%.4f sl=%.4f",
			plan.Symbol, plan.Qty, plan.Price, plan.Brackets.TakeProfit, plan.Brackets.StopLoss)
		return
	}
	if err := l.submitPreOrder(ctx, plan, conf); err != nil {
		log.Printf("[auto] %s pre-order rejected/failed: %v", dec.Symbol, err)
	}
}

// handleSell exits a symbol on a signal. It used to refuse when the
// current time fell outside the ENTRY trading window, and to skip any
// position whose quantity was not positive — so a short could never be
// closed by a signal, and an exit could be blocked by the schedule that
// governs opening risk. Neither policy belongs here: the window answers
// "may this bot take on exposure now?", not "may it shed exposure?".
func (l *autoLoop) handleSell(ctx context.Context, dec strategy.Decision) {
	autoJournal(l.journal, "sell-exit "+dec.Symbol+" "+dec.Reason, dec.Symbol, "sell", 0)
	if l.dryRun {
		log.Printf("[auto] DRY-RUN SELL %s: %s", dec.Symbol, dec.Reason)
		return
	}
	done, err := l.exec.ExecuteReduce(ctx, dec.Symbol, dec.Reason)
	l.book.Invalidate()
	if err != nil {
		log.Printf("[auto] %s exit refused: %v", dec.Symbol, err)
		return
	}
	if !done {
		log.Printf("[auto] %s exit: nothing held to reduce", dec.Symbol)
		return
	}
	log.Printf("[auto] EXIT %s placed (%s)", dec.Symbol, dec.Reason)
}

// ---- small helpers ----

func positionQtyFor(ctx context.Context, c *tools.Client, symbol string) (float64, error) {
	ps, err := c.GetPositions(ctx)
	if err != nil {
		return 0, err
	}
	for _, p := range ps {
		if tools.SameSymbol(p.Symbol, symbol) {
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

// autoJournal writes a single audit-trail line for a buy/sell decision
// taken by the auto loop. Symbol + Side are populated explicitly so
// the dashboard's Recent Decisions feed can show them in the table
// (rows with empty Symbol render as blank "—" cells and confuse
// operators into thinking the bot never sells). The Confidence is
// the strategy.Engine's composite score at decision time when
// available; zero on early-loop ticks before the scorer warms up.
func autoJournal(j *pnl.Journal, detail, symbol, side string, confidence float64) {
	if j == nil {
		return
	}
	conf := confidence
	rec := pnl.Record{
		Kind:   pnl.KindDecision,
		Source: "strategy:auto",
		Risk:   "APPROVED",
		Symbol: symbol,
		Side:   side,
		Detail: detail,
	}
	if conf > 0 {
		rec.Confidence = &conf
	}
	_ = j.Append(rec)
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
	// Read through the typed store. The previous version parsed the file
	// itself, which masked a defect: the field was not part of the persisted
	// document, so every Heartbeat marshalled the struct and DROPPED it. The
	// adaptive baseline was destroyed on every tick, the P/L multiplier sat
	// at 1.0 forever, and the dashboard reported a baseline the bot did not
	// have.
	v, err := l.exec.State.ReadStartingEquity()
	if err != nil {
		log.Printf("[auto] starting_equity read failed: %v", err)
		return 0
	}
	return v
}

// persistStartingEquity records the adaptive-sizing baseline once, through
// the typed store, so the value survives the per-tick heartbeat writes.
func (l *autoLoop) persistStartingEquity(equity float64) {
	if l.exec == nil || l.exec.State == nil || equity <= 0 {
		return
	}
	wrote, err := l.exec.State.SetStartingEquityIfUnset(equity)
	if err != nil {
		log.Printf("[auto] starting_equity write failed: %v", err)
		return
	}
	if wrote {
		log.Printf("[auto] adaptive sizing baseline set to %.2f", equity)
	}
}

// applyConfigOverrides reads the dashboard-owned knobs and applies them to
// this process's live config.
//
// Before the single-writer split the API wrote these keys into the bot's
// strategy-state.json, which the bot never read: the dashboard's risk knobs
// were decorative for the trading process. Reading the API's own file makes
// them real. The read is mtime-gated (one stat per tick) and applies only
// positive values, so a key the operator never set keeps the env default
// rather than zeroing a cap.
func (l *autoLoop) applyConfigOverrides() {
	if l.exec == nil || l.exec.State == nil || l.cfg == nil {
		return
	}
	path := strategy.ConfigOverridesPath(l.exec.State.Path())
	st, err := os.Stat(path)
	if err != nil {
		return // no overrides yet: env stands
	}
	if !l.overridesAt.IsZero() && st.ModTime().Equal(l.overridesAt) {
		return
	}
	ov, err := strategy.LoadConfigOverrides(path)
	if err != nil {
		// A corrupt overrides file must not silently zero the operator's
		// caps, and must not be applied partially either.
		log.Printf("[config] overrides unreadable (%v); keeping the current settings", err)
		return
	}
	l.overridesAt = st.ModTime()

	var applied []string
	if ov.MaxPositionUSD > 0 && ov.MaxPositionUSD != l.cfg.MaxPositionUSD {
		l.cfg.MaxPositionUSD = ov.MaxPositionUSD
		applied = append(applied, fmt.Sprintf("max_position_usd=%.2f", ov.MaxPositionUSD))
	}
	if ov.MaxPortfolioPct > 0 && ov.MaxPortfolioPct != l.cfg.MaxPortfolioPct {
		l.cfg.MaxPortfolioPct = ov.MaxPortfolioPct
		applied = append(applied, fmt.Sprintf("max_portfolio_pct=%.4f", ov.MaxPortfolioPct))
	}
	if ov.CryptoMaxPositionUSD > 0 && ov.CryptoMaxPositionUSD != l.cfg.CryptoMaxPositionUSD {
		l.cfg.CryptoMaxPositionUSD = ov.CryptoMaxPositionUSD
		applied = append(applied, fmt.Sprintf("crypto_max_position_usd=%.2f", ov.CryptoMaxPositionUSD))
	}
	if ov.MinConfidence > 0 && ov.MinConfidence != l.cfg.MinConfidence {
		l.cfg.MinConfidence = ov.MinConfidence
		applied = append(applied, fmt.Sprintf("min_confidence=%.4f", ov.MinConfidence))
	}
	// Drawdown halts live on a COPY inside the monitor, so the monitor's
	// settings must be updated too or the dashboard's halt change would
	// apply to the gate and not to the thing that watches drawdown.
	if l.monitor != nil {
		if ov.DailyDDHalt > 0 && ov.DailyDDHalt != l.monitor.Settings.DailyDD {
			l.monitor.Settings.DailyDD = ov.DailyDDHalt
			l.set.DailyDD = ov.DailyDDHalt
			applied = append(applied, fmt.Sprintf("daily_dd_halt=%.4f", ov.DailyDDHalt))
		}
		if ov.WeeklyDDHalt > 0 && ov.WeeklyDDHalt != l.monitor.Settings.WeeklyDD {
			l.monitor.Settings.WeeklyDD = ov.WeeklyDDHalt
			l.set.WeeklyDD = ov.WeeklyDDHalt
			applied = append(applied, fmt.Sprintf("weekly_dd_halt=%.4f", ov.WeeklyDDHalt))
		}
		if ov.TotalDDHalt > 0 && ov.TotalDDHalt != l.monitor.Settings.TotalDD {
			l.monitor.Settings.TotalDD = ov.TotalDDHalt
			l.set.TotalDD = ov.TotalDDHalt
			applied = append(applied, fmt.Sprintf("total_dd_halt=%.4f", ov.TotalDDHalt))
		}
	}
	if len(applied) == 0 {
		return
	}
	log.Printf("[config] applied dashboard overrides: %s", strings.Join(applied, ", "))
	if l.journal != nil {
		_ = l.journal.Append(pnl.Record{
			Kind: pnl.KindDecision, Source: "config:overrides", Risk: "APPROVED",
			Detail: "applied dashboard risk overrides: " + strings.Join(applied, ", "),
		})
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

// recordReferenceData saves a versioned snapshot of the universe and journals
// what changed since the last one. Failures are logged, never fatal: research
// bookkeeping must not stop the monitor.
func (l *autoLoop) recordReferenceData(reg *picker.Registry) {
	if reg == nil || l.journal == nil {
		return
	}
	path := refdata.Path(l.journal.Path())
	prev, err := refdata.Load(path)
	if err != nil {
		// A corrupt snapshot must not be read as "everything appeared".
		log.Printf("[refdata] previous snapshot unreadable (%v); starting a new one", err)
	}
	snap := refdata.Build(reg.Instruments(), time.Now())
	snap.Version = prev.Version + 1
	changes := refdata.Diff(prev, snap)
	for _, c := range changes {
		log.Printf("[refdata] %s %s: %s", c.Kind, c.Symbol, c.Detail)
		_ = l.journal.Append(pnl.Record{
			Kind: pnl.KindDecision, Source: "refdata:" + string(c.Kind),
			Risk: "HEAL_NOTED", Symbol: c.Symbol, Detail: c.Detail,
		})
	}
	if _, err := refdata.Save(path, snap); err != nil {
		log.Printf("[refdata] snapshot not saved: %v", err)
	}
}
