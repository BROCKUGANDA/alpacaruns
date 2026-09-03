// Package agents defines the MoE expert agents, the gating root agent,
// and the Sequential/Parallel/Loop orchestration graph.
//
// Model selection: if config.LLMBaseURL is set, all agents run against a
// local llama.cpp llama-server (OpenAI-compatible); otherwise Gemini API.
// Alpaca capabilities come from Alpaca's official MCP server via mcptoolset.
package agents

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/agent/workflowagents/loopagent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
	"google.golang.org/adk/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	llamacpp "github.com/BROCKUGANDA/alpacaruns/model/llamacpp"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/factors"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)


// Graph bundles every agent plus the kill-switch handle.
type Graph struct {
	Root       agent.Agent // dynamic gating router
	TradeCycle agent.Agent // deterministic sequential trading cycle
	Monitor    agent.Agent // loop agent for continuous monitoring
	KillSwitch *KillSwitch
	closers    []func()
}

func (g *Graph) Close() {
	for _, c := range g.closers {
		c()
	}
}

// KillSwitch is a shared flag; when set, every gate refuses to proceed.
type KillSwitch struct{ halted chan struct{} }

func NewKillSwitch() *KillSwitch { return &KillSwitch{halted: make(chan struct{})} }
func (k *KillSwitch) Halt() {
	select {
	case <-k.halted:
	default:
		close(k.halted)
	}
}
func (k *KillSwitch) Halted() bool {
	select {
	case <-k.halted:
		return true
	default:
		return false
	}
}

// Build creates the model, connects Alpaca's MCP server, and wires the graph.
func Build(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Graph, error) {
	g := &Graph{KillSwitch: NewKillSwitch()}
	buildErr := func(err error) (*Graph, error) {
		g.Close()
		return nil, err
	}

	var mdl model.LLM
	switch cfg.LLMProvider {
	case config.ProviderOxlo:
		mdl = llamacpp.NewWithKey(cfg.OxloBaseURL, cfg.LLMModel, cfg.OxloAPIKey, log)
		log.Info("using Oxlo.ai cloud model", "base_url", cfg.OxloBaseURL, "model", cfg.LLMModel)
	case config.ProviderGemini:
		gm, err := gemini.NewModel(ctx, "gemini-3.1-flash-lite", nil)
		if err != nil {
			return buildErr(fmt.Errorf("create gemini model: %w", err))
		}
		mdl = gm
		log.Info("using Gemini API model", "model", "gemini-3.1-flash-lite")
	default: // config.ProviderLLamaCPP (and any unset value)
		mdl = llamacpp.New(cfg.LLMBaseURL, cfg.LLMModel, log)
		log.Info("using local llama.cpp model", "base_url", cfg.LLMBaseURL, "model", cfg.LLMModel)
	}

	supervised := cfg.Mode == config.ModeSupervised

	// Two MCP connections to Alpaca's official server:
	//  - dataToolset: read-only market/account tools, no confirmation needed
	//  - execToolset: order placement/status/cancel; in supervised mode every
	//    call triggers ADK Human-in-the-Loop confirmation.
	dataTS, closeData, err := alpacaToolset(cfg, false)
	if err != nil {
		return buildErr(fmt.Errorf("connect alpaca mcp (data): %w", err))
	}
	g.closers = append(g.closers, closeData)
	execTS, closeExec, err := alpacaToolset(cfg, supervised)
	if err != nil {
		return buildErr(fmt.Errorf("connect alpaca mcp (execution): %w", err))
	}
	g.closers = append(g.closers, closeExec)

	// Deterministic pre-trade gate (B1): every order-placement MCP call
	// passes through the risk.Validator before reaching Alpaca. Data
	// tools stay unguarded so monitoring keeps working while blocked.
	val := &risk.Validator{
		Cfg:   cfg,
		Kill:  g.KillSwitch,
		Clock: marketClock{cfg: cfg},
		Portfolio: func() (risk.Portfolio, error) {
			return accountSnapshot(cfg)
		},
		Factors: factors.NewEngine(cfg,
			tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL),
			nil, factors.Options{}),
	}
	execTS = &riskGuardedToolset{inner: execTS, val: val}

	ts := []tool.Toolset{dataTS}
	execTs := []tool.Toolset{execTS}

	killTool, err := functiontool.New(functiontool.Config{
		Name:        "kill_switch",
		Description: "EMERGENCY STOP. Halts trading immediately. No further trades will be processed.",
	}, func(ctx agent.ToolContext, _ struct{}) (string, error) {
		g.KillSwitch.Halt()
		log.Warn("KILL SWITCH ACTIVATED", "agent", ctx.AgentName())
		return "Trading halted. Kill switch engaged.", nil
	})
	if err != nil {
		return buildErr(err)
	}

	mustAgent := func(c llmagent.Config) agent.Agent {
		a, err := llmagent.New(c)
		if err != nil {
			return buildErrPanic(g, err)
		}
		return a
	}

	// ---- Experts (the mixture). Each writes its result to session.state
	// under a defined output_key so downstream experts read structured data. ----
	marketData := mustAgent(llmagent.Config{
		Name:        "MarketDataExpert",
		Model:       mdl,
		Description: "Fetches bars, quotes, snapshots and news for watched symbols.",
		Instruction: "You only fetch market data using your tools and report it as structured JSON. Never give opinions or place orders.",
		Toolsets:    ts,
		OutputKey:   "market_data",
	})
	technical := mustAgent(llmagent.Config{
		Name:        "TechnicalAnalysisExpert",
		Model:       mdl,
		Description: "Computes RSI, MACD, moving averages, volatility and volume anomalies from fetched bars.",
		Instruction: "Given market data in session state (key: market_data), compute technical indicators (RSI-14, MACD, SMA20/SMA50 crossovers, volatility, volume anomalies). Output trend/momentum signals with numeric evidence. You have no order tools.",
		OutputKey:   "technical_signals",
	})
	sentiment := mustAgent(llmagent.Config{
		Name:        "SentimentNewsExpert",
		Model:       mdl,
		Description: "Scores news sentiment for watched tickers.",
		Instruction: "Fetch recent news then score sentiment per ticker (-1..+1) with a one-line rationale each. You never place orders.",
		Toolsets:    ts,
		OutputKey:   "news_sentiment",
	})
	tradeIdea := mustAgent(llmagent.Config{
		Name:        "TradeIdeaExpert",
		Model:       mdl,
		Description: "Synthesizes analysis into ranked trade ideas with confidence scores.",
		Instruction: `Combine technical_signals and news_sentiment from session state into 0-3 ranked trade ideas. For each output JSON: {ticker, direction buy|sell, entry, stop, target, rationale, confidence 0..1}. Confidence below 0.5 means discard the idea. You never place orders.`,
		OutputKey:   "trade_ideas",
	})
	risk := mustAgent(llmagent.Config{
		Name:        "RiskManagementExpert",
		Model:       mdl,
		Description: "Validates trade ideas against position sizing, drawdown, exposure and buying power.",
		Instruction: fmt.Sprintf(`For each idea in trade_ideas, validate against the account: (1) notional <= %.2f USD per position, (2) single position <= %.0f%% of portfolio, (3) exposure leaves margin buffer, (4) in autonomous mode reject any idea whose confidence < %.2f. Additionally, every order is gated by a deterministic multi-factor engine (trend vs SMA20/SMA50, momentum, volatility, volume vs 20-day average, news attention) computed from live market data; an order is placed only if its LLM confidence AND its composite factor score >= %.2f both clear their thresholds — the code-enforced gate applies this automatically and reports which factors were weak. Mark each APPROVED or REJECTED with reasons. If ALL are rejected say HALT_TRADING.`,
			cfg.MaxPositionUSD, cfg.MaxPortfolioPct*100, cfg.MinConfidence, cfg.FactorMinScore),
		Toolsets:  ts,
		OutputKey: "risk_assessment",
	})
	execution := mustAgent(llmagent.Config{
		Name:        "ExecutionExpert",
		Model:       mdl,
		Description: "Places approved paper orders and confirms fills.",
		Instruction: `Place paper orders ONLY for ideas risk_assessment marks APPROVED, respecting entry/stop/target. After each placement poll get_order_status until filled/rejected. Summarize fills. If no approved ideas, do nothing.`,
		Toolsets:    execTs,
		OutputKey:   "executions",
	})
	nlQuery := mustAgent(llmagent.Config{
		Name:        "NLQueryExpert",
		Model:       mdl,
		Description: "Answers ad-hoc natural-language market questions (quotes, RSI, news sentiment).",
		Instruction: "Answer natural-language market-data questions using your tools ('what is AAPL's latest quote', 'show TSLA 5-day bars'). Never place orders.",
		Toolsets:    ts,
	})

	// ---- Workflow orchestration (deterministic core cycle) ----
	analysisPar, err := parallelagent.New(parallelagent.Config{
		AgentConfig: agent.Config{
			Name:        "AnalysisParallel",
			Description: "Runs technical and sentiment analysis concurrently.",
			SubAgents:   []agent.Agent{technical, sentiment},
		},
	})
	if err != nil {
		return buildErr(err)
	}
	cycle, err := sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name:        "TradingCycle",
			Description: "Full trading cycle: data -> parallel analysis -> ideas -> risk -> execution.",
			SubAgents:   []agent.Agent{marketData, analysisPar, tradeIdea, risk, execution},
		},
	})
	if err != nil {
		return buildErr(err)
	}

	haltableRisk := mustAgent(llmagent.Config{
		Name:        "HaltableRiskGate",
		Model:       mdl,
		Description: "Per-tick risk check inside the monitor loop; exits or halts on breach.",
		Instruction: `Check open positions vs risk limits (get_account/get_positions). If limits are breached call kill_switch immediately; for soft warnings call exit_loop. Otherwise reply OK.`,
		Toolsets:    append(ts, &plainTools{tools: []tool.Tool{killTool}}),
	})
	monitor, err := loopagent.New(loopagent.Config{
		AgentConfig: agent.Config{
			Name:        "MonitorLoop",
			Description: "Continuous monitoring loop during market hours.",
			SubAgents:   []agent.Agent{haltableRisk},
		},
		MaxIterations: 0,
	})
	if err != nil {
		return buildErr(err)
	}

	root := mustAgent(llmagent.Config{
		Name:        "GatingRoot",
		Model:       mdl,
		Description: "Routes requests: ad-hoc market questions to NLQueryExpert, scheduled cycles to TradingCycle, monitoring to MonitorLoop.",
		Instruction: `You are the gating network of a MoE trading system. Route:
- natural-language market questions -> transfer to NLQueryExpert
- 'run one trading cycle' / scheduled tick -> transfer to TradingCycle
- 'start monitoring' -> transfer to MonitorLoop
Never answer market questions yourself and never route execution outside the cycle. Paper trading only.`,
		SubAgents: []agent.Agent{nlQuery, cycle, monitor},
	})

	g.Root, g.TradeCycle, g.Monitor = root, cycle, monitor
	return g, nil
}

func buildErrPanic(g *Graph, err error) agent.Agent {
	g.Close()
	panic(err)
}

// plainTools adapts bare tools into a one-tool toolset.
type plainTools struct{ tools []tool.Tool }

func (p *plainTools) Name() string        { return "plain_tools" }
func (p *plainTools) Description() string { return "locally defined function tools" }
func (p *plainTools) Tools(agent.ReadonlyContext) ([]tool.Tool, error) {
	return p.tools, nil
}
