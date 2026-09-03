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

// alpacaToolset connects to Alpaca's official MCP server over stdio
// (`uvx alpaca-mcp-server`). Keys are passed via env (read by the server).
// When requireConfirmation is true every tool call triggers ADK's
// Human-in-the-Loop confirmation (supervised mode).
func alpacaToolset(cfg *config.Config, requireConfirmation bool) (tool.Toolset, func(), error) {
	cmd := exec.Command("uvx", "alpaca-mcp-server")
	cmd.Env = append(cmd.Environ(),
		"ALPACA_API_KEY="+cfg.AlpacaKeyID,
		"ALPACA_SECRET_KEY="+cfg.AlpacaSecret,
		"CONDA_NO_PLUGINS=true",
	)
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

// orderToolNames are the Alpaca MCP tools that can create or modify
// orders. Everything else (data reads, status polls) passes unguarded so
// the agent keeps full visibility even while trading is blocked.
var orderToolNames = map[string]bool{
	"place_order":         true,
	"submit_order":        true,
	"create_order":        true,
	"cancel_order":        true,
	"cancel_all_orders":   true,
	"replace_order":       true,
	"close_position":      true,
	"close_all_positions": true,
	"execute_order":       true,
}

// riskGuardedToolset filters an execution toolset: order-placement tools
// get wrapped with the deterministic validator; data tools pass through.
type riskGuardedToolset struct {
	inner tool.Toolset
	val   *risk.Validator
}

func (r *riskGuardedToolset) Name() string { return "risk_guarded_" + r.inner.Name() }
func (r *riskGuardedToolset) Description() string {
	type describer interface{ Description() string }
	if d, ok := r.inner.(describer); ok {
		return d.Description()
	}
	return "execution tools behind deterministic risk checks"
}
func (r *riskGuardedToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	innerTools, err := r.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tool.Tool, 0, len(innerTools))
	for _, t := range innerTools {
		if !orderToolNames[strings.ToLower(t.Name())] {
			out = append(out, t)
			continue
		}
		tt := t
		out = append(out, &guardedTool{
			inner: tt,
			run: func(tctx agent.ToolContext, args any) (map[string]any, error) {
				p := proposalFromArgs(t.Name(), args)
				verdict := r.val.Validate(p)
				log.Printf("[risk-gate] %s %v -> %s", t.Name(), args, verdict.String())
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
				return nil, fmt.Errorf("order tool %q has no runnable implementation", t.Name())
			},
		})
	}
	return out, nil
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
		Cfg:      cfg,
		Kill:     kill,
		Clock:    marketClock{cfg: cfg},
		Portfolio: func() (risk.Portfolio, error) { return accountSnapshot(cfg) },
		Factors:  scorer,
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
