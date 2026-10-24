package agents

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"
	"google.golang.org/genai"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/factors"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// Toolset name groups understood by Alpaca's MCP server
// (ALPACA_TOOLSETS). The data session gets read-only market-data
// toolsets so it structurally CANNOT place an order; the execution
// session gets the trading toolsets, whose order tools are wrapped by
// the deterministic risk gate.
const (
	DataToolsetEnv = "stock-data,crypto-data,options-data,news"
	ExecToolsetEnv = "trading,account,assets"
)

// alpacaToolset connects to Alpaca's official MCP server over stdio
// (`uvx alpaca-mcp-server`). Keys are passed via env (read by the server).
// When requireConfirmation is true every tool call triggers ADK's
// Human-in-the-Loop confirmation (supervised mode).
//
// The venue is pinned from the configured base URL: ALPACA_PAPER_TRADE is
// derived from ALPACA_BASE_URL rather than left to the server's default,
// so a live base URL can never be silently downgraded to paper (or the
// other way round) behind the config's I_ALPACA_LIVE acknowledgement.
func alpacaToolset(cfg *config.Config, requireConfirmation bool, toolsets string) (tool.Toolset, func(), error) {
	spec := mcpServerSpec(cfg)
	cmd := exec.Command("uvx", spec)
	cmd.Env = append(cmd.Environ(),
		"ALPACA_API_KEY="+cfg.AlpacaKeyID,
		"ALPACA_SECRET_KEY="+cfg.AlpacaSecret,
		"ALPACA_PAPER_TRADE="+strconv.FormatBool(isPaperVenue(cfg.AlpacaBaseURL)),
		"DATA_API_URL="+cfg.AlpacaDataURL,
		"CONDA_NO_PLUGINS=true",
	)
	if toolsets != "" {
		cmd.Env = append(cmd.Env, "ALPACA_TOOLSETS="+toolsets)
	}
	// The spec is logged because the guard's whole safety story is a
	// classification of tool NAMES: an unpinned `uvx alpaca-mcp-server`
	// silently upgrades to a build whose tool surface nobody reviewed, and
	// a renamed mutation tool is denied only if its name is recognised as
	// unknown rather than mistaken for a read.
	log.Printf("[mcp] launching %s (toolsets=%s, paper=%t)", spec, orNoneToolset(toolsets), isPaperVenue(cfg.AlpacaBaseURL))
	ts, err := mcptoolset.New(mcptoolset.Config{
		Transport:           &mcp.CommandTransport{Command: cmd},
		RequireConfirmation: requireConfirmation,
	})
	if err != nil {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil, nil, err
	}
	closeFn := func() {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	return ts, closeFn, nil
}

// isPaperVenue reports whether a trading base URL points at Alpaca's
// paper environment.
func isPaperVenue(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "paper-api")
}

// guardedTool wraps a single MCP tool and runs the deterministic risk
// validator before any order placement call reaches the server. Order
// tools are identified by name; every other tool passes through.
type guardedTool struct {
	inner tool.Tool
	decl  *genai.FunctionDeclaration
	run   func(ctx agent.ToolContext, args any) (map[string]any, error)
}

func (g *guardedTool) Name() string        { return g.inner.Name() }
func (g *guardedTool) Description() string { return g.inner.Description() }
func (g *guardedTool) IsLongRunning() bool { return g.inner.IsLongRunning() }
func (g *guardedTool) Declaration() *genai.FunctionDeclaration {
	if g.decl != nil {
		return g.decl
	}
	type declarer interface {
		Declaration() *genai.FunctionDeclaration
	}
	if d, ok := g.inner.(declarer); ok {
		return d.Declaration()
	}
	return nil
}
func (g *guardedTool) ProcessRequest(ctx agent.ToolContext, req *model.LLMRequest) error {
	type processor interface {
		ProcessRequest(agent.ToolContext, *model.LLMRequest) error
	}
	if p, ok := g.inner.(processor); ok {
		return p.ProcessRequest(ctx, req)
	}
	return nil
}
func (g *guardedTool) Run(ctx agent.ToolContext, args any) (map[string]any, error) {
	return g.run(ctx, args)
}

var _ tool.Tool = (*guardedTool)(nil)

// toolClass says what an MCP tool is allowed to do.
type toolClass int

const (
	// toolRead is a read-only lookup: it passes straight through.
	toolRead toolClass = iota
	// toolOrder creates or increases exposure: it must clear the
	// deterministic risk gate first.
	toolOrder
	// toolControl reduces or reshapes existing exposure (cancel, close).
	// It is allowed through — blocking a cancel or a flatten would make
	// the system less safe — but it is journaled.
	toolControl
	// toolBlocked is never allowed from an agent.
	toolBlocked
)

// classifyTool maps an Alpaca MCP tool name to its class.
//
// The classification is DEFAULT-DENY: a tool is read-only only when its
// name carries an explicit read prefix, and any action tool that is not
// explicitly recognised is blocked. The previous implementation matched
// a short list of guessed names ("place_order"), while the real server
// exposes "place_stock_order", "place_crypto_order" and
// "place_option_order" — so every actual order tool bypassed the risk
// gate entirely.
func classifyTool(name string) toolClass {
	n := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.HasPrefix(n, "get_"), strings.HasPrefix(n, "search_"),
		strings.HasPrefix(n, "fetch_"), strings.HasPrefix(n, "list_"):
		return toolRead
	}
	switch n {
	case "place_stock_order", "place_crypto_order", "place_option_order":
		return toolOrder
	case "replace_order_by_id", "exercise_options_position":
		// Both change the size or existence of exposure.
		return toolOrder
	case "cancel_order_by_id", "cancel_all_orders", "close_position",
		"close_all_positions", "do_not_exercise_options_position":
		return toolControl
	case "update_account_config", "create_locate":
		// Account configuration and borrow locates are operator actions,
		// never agent actions.
		return toolBlocked
	}
	return toolBlocked
}

// riskGuardedToolset filters an execution toolset: order-placement tools
// get wrapped with the deterministic validator; control tools are
// journaled; blocked tools are refused; data tools pass through.
type riskGuardedToolset struct {
	inner tool.Toolset
	val   *risk.Validator
	log   func(format string, args ...any)
}

func (r *riskGuardedToolset) Name() string { return "risk_guarded_" + r.inner.Name() }
func (r *riskGuardedToolset) Description() string {
	type describer interface{ Description() string }
	if d, ok := r.inner.(describer); ok {
		return d.Description()
	}
	return "execution tools behind deterministic risk checks"
}
func (r *riskGuardedToolset) logf(format string, args ...any) {
	if r.log != nil {
		r.log(format, args...)
	}
}
func (r *riskGuardedToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	innerTools, err := r.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tool.Tool, 0, len(innerTools))
	for _, t := range innerTools {
		tt := t
		switch classifyTool(tt.Name()) {
		case toolRead:
			out = append(out, tt)
		case toolBlocked:
			out = append(out, &guardedTool{
				inner: tt,
				run: func(tctx agent.ToolContext, args any) (map[string]any, error) {
					r.logf("[risk-gate] blocked tool %s: not permitted for agents", tt.Name())
					return map[string]any{
						"blocked": true,
						"reason":  "tool " + tt.Name() + " is not permitted for agent use",
					}, nil
				},
			})
		case toolControl:
			out = append(out, &guardedTool{
				inner: tt,
				run: func(tctx agent.ToolContext, args any) (map[string]any, error) {
					r.logf("[risk-gate] control tool %s %v", tt.Name(), args)
					type runner interface {
						Run(agent.ToolContext, any) (map[string]any, error)
					}
					if rr, ok := tt.(runner); ok {
						return rr.Run(tctx, args)
					}
					return nil, fmt.Errorf("control tool %q has no runnable implementation", tt.Name())
				},
			})
		default: // toolOrder
			out = append(out, &guardedTool{
				inner: tt,
				run: func(tctx agent.ToolContext, args any) (map[string]any, error) {
					p := proposalFromArgs(tt.Name(), args)
					if p.Confidence == nil {
						p.Confidence = r.deriveConfidence(p.Symbol)
					}
					verdict := r.val.Validate(p)
					r.logf("[risk-gate] %s %v -> %s", tt.Name(), args, verdict.String())
					if !verdict.Approved {
						return map[string]any{
							"blocked": true,
							"reason":  verdict.String(),
						}, nil
					}
					args = applyExtendedHoursDefault(r.val.Cfg, p, args)
					type runner interface {
						Run(agent.ToolContext, any) (map[string]any, error)
					}
					if rr, ok := tt.(runner); ok {
						return rr.Run(tctx, args)
					}
					return nil, fmt.Errorf("order tool %q has no runnable implementation", tt.Name())
				},
			})
		}
	}
	return out, nil
}

// deriveConfidence produces the confidence value for a proposal the LLM
// did not score. Alpaca's MCP order schema has no confidence parameter,
// so the LLM cannot supply one; without a substitute every agent order
// was rejected with "missing confidence score" and the agent path could
// never trade. The deterministic factor composite stands in — it cannot
// be self-reported away, and the gate still applies MIN_CONFIDENCE to
// it. Returns nil when no factor score is available, which leaves the
// proposal rejected (fail closed).
func (r *riskGuardedToolset) deriveConfidence(symbol string) *float64 {
	if r.val == nil || r.val.Factors == nil || strings.TrimSpace(symbol) == "" {
		return nil
	}
	res, err := r.val.Factors.ScoreFactors(r.val.Context(), symbol)
	if err != nil {
		r.logf("[risk-gate] confidence derivation failed for %s: %v", symbol, err)
		return nil
	}
	c := res.Composite
	return &c
}

// NewValidator builds the deterministic pre-trade validator exactly as
// wired inside agents.Build: live kill switch, Alpaca market clock and
// account snapshot. Exported so non-LLM paths (the `trade` subcommand)
// go through the identical risk gate.
func NewValidator(kill risk.HaltSource, cfg *config.Config) *risk.Validator {
	return &risk.Validator{
		Cfg:   cfg,
		Kill:  kill,
		Clock: marketClock{cfg: cfg},
		Portfolio: func() (risk.Portfolio, error) {
			return accountSnapshot(cfg)
		},
		Factors: factors.NewEngine(cfg,
			tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL),
			nil, factors.Options{}),
	}
}

// NewValidatorWithScorer builds the validator with a caller-supplied
// FactorScorer. This is how the auto loop wires the MultiVenueBars-aware
// scorer so crypto symbols can pass the risk gate's factor check; the
// bare NewValidator above still uses tools.Client directly and fails
// closed for crypto (acceptable for the manual `trade` path).
func NewValidatorWithScorer(kill risk.HaltSource, cfg *config.Config, scorer risk.FactorScorer) *risk.Validator {
	return &risk.Validator{
		Cfg:       cfg,
		Kill:      kill,
		Clock:     marketClock{cfg: cfg},
		Portfolio: func() (risk.Portfolio, error) { return accountSnapshot(cfg) },
		Factors:   scorer,
	}
}

// marketClock adapts the Alpaca /v2/clock endpoint to the risk.MarketClock
// interface. It fails open for reads (returns open=true on error) because
// a transient data outage should not permanently block trading; the risk
// validator still enforces all position caps independently.
type marketClock struct {
	cfg *config.Config
}

func (m marketClock) MarketOpen() bool {
	cl := m.fetchClock()
	if cl == nil {
		log.Printf("[risk-gate] clock check failed; assuming market open")
		return true
	}
	return cl.IsOpen
}

// SessionOpen approximates the US extended session window from the clock
// timestamp: weekdays 04:00-20:00 ET. The /v2/clock timestamp is already
// ET-formatted, so we compare wall-clock fields directly.
func (m marketClock) SessionOpen(p risk.Proposal) bool {
	cl := m.fetchClock()
	if cl == nil {
		return false
	}
	ts, err := time.Parse(time.RFC3339, cl.Timestamp)
	if err != nil {
		return false
	}
	return risk.InExtendedSession(ts.Weekday(), ts.Hour(), ts.Minute())
}

// SessionKnown reports whether a clock fetch produced usable state;
// unknown state fails closed in the extended-hours gate.
func (m marketClock) SessionKnown() bool {
	return m.fetchClock() != nil
}

// proposalFromArgs extracts a risk.Proposal from the raw tool-call args
// emitted by the LLM. Field names cover the common spellings the Alpaca
// MCP server uses; unknown shapes fail validation via missing fields.
// Order type and time_in_force feed the extended-hours session check.
func proposalFromArgs(name string, args any) risk.Proposal {
	m, _ := args.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	getStr := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				switch s := v.(type) {
				case string:
					return s
				case float64:
					return strconv.FormatFloat(s, 'f', -1, 64)
				}
			}
		}
		return ""
	}
	p := risk.Proposal{
		Symbol:      getStr("symbol", "ticker"),
		Side:        getStr("side", "direction", "action"),
		Qty:         getStr("qty", "quantity", "shares", "contracts"),
		Notional:    getStr("notional", "amount", "value"),
		OrderType:   getStr("type", "order_type"),
		TimeInForce: getStr("time_in_force", "tif"),
	}
	if c := getStr("confidence", "score"); c != "" {
		if f, err := strconv.ParseFloat(c, 64); err == nil {
			p.Confidence = &f
		}
	} else if f, ok := m["confidence"].(float64); ok {
		p.Confidence = &f
	}
	if eh, ok := m["extended_hours"].(bool); ok {
		p.ExtendedHours = eh
	}
	return p
}

// fetchClock returns the current clock or nil on any error.
func (m marketClock) fetchClock() *tools.Clock {
	c := tools.NewClient(m.cfg.AlpacaKeyID, m.cfg.AlpacaSecret, m.cfg.AlpacaBaseURL, m.cfg.AlpacaDataURL)
	cl, err := c.GetClock(context.Background())
	if err != nil {
		log.Printf("[risk-gate] clock read failed: %v", err)
		return nil
	}
	return cl
}

// applyExtendedHoursDefault stamps extended_hours=true onto order args
// when EXTENDED_HOURS is enabled in config and the proposal satisfies the
// Alpaca constraint (type=limit, time_in_force day|gtc) and the caller
// did not set the field explicitly. Returns args unchanged otherwise.
func applyExtendedHoursDefault(cfg *config.Config, p risk.Proposal, args any) any {
	if cfg == nil || !cfg.ExtendedHours || p.ExtendedHours {
		return args
	}
	m, ok := args.(map[string]any)
	if !ok {
		return args
	}
	t := strings.ToLower(strings.TrimSpace(p.OrderType))
	tif := strings.ToLower(strings.TrimSpace(p.TimeInForce))
	if t != "limit" || (tif != "day" && tif != "gtc") {
		return args
	}
	cp := make(map[string]any, len(m)+1)
	for k, v := range m {
		cp[k] = v
	}
	cp["extended_hours"] = true
	return cp
}

// accountSnapshot fetches live equity/portfolio value for the validator.
func accountSnapshot(cfg *config.Config) (risk.Portfolio, error) {
	c := tools.NewClient(cfg.AlpacaKeyID, cfg.AlpacaSecret, cfg.AlpacaBaseURL, cfg.AlpacaDataURL)
	acct, err := c.GetAccount(context.Background())
	if err != nil {
		return risk.Portfolio{}, err
	}
	equity, _ := strconv.ParseFloat(acct.Equity, 64)
	pv, _ := strconv.ParseFloat(acct.PortfolioValue, 64)
	return risk.Portfolio{Equity: equity, PortfolioValue: pv}, nil
}

// DefaultMCPServerSpec pins the Alpaca MCP server the agents talk to. The
// version is part of the supply chain, not a detail: the risk guard decides
// what an agent may do by classifying tool names, so an unannounced upgrade
// changes the surface the guard was written against. Bump this deliberately
// after reading the new tool manifest.
const DefaultMCPServerSpec = "alpaca-mcp-server==2.4.0"

// mcpServerSpec returns the pinned uvx spec, overridable for a deliberate
// upgrade without a rebuild.
func mcpServerSpec(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.AlpacaMCPSpec) != "" {
		return strings.TrimSpace(cfg.AlpacaMCPSpec)
	}
	return DefaultMCPServerSpec
}

func orNoneToolset(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}
