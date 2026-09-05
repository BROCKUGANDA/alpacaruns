// types.go — request/response shapes for the dashboard API. Every
// field is explicit (no map[string]any) so the Next.js frontend can
// rely on stable field names; omitempty on optional/empty fields
// keeps payloads small.
package api

import "time"

// ---- Health ----

// HealthResponse is the shape of GET /api/health. uptime_sec is since
// server start; last_poll is the most recent bot tick (the bot
// publishes this by writing a small stamp file the API can read —
// falls back to zero when no ticks have happened yet, e.g. on cold
// start with the bot still booting).
type HealthResponse struct {
	OK        bool      `json:"ok"`
	Version   string    `json:"version"`
	UptimeSec int64     `json:"uptime_sec"`
	LastPoll  time.Time `json:"last_poll,omitempty"`
	BotAlive  bool      `json:"bot_alive"` // true when a fresh poll stamp exists within 3x poll interval
}

// ---- Status ----

// BotState is the high-level state surfaced by /api/status. The bot
// is "halted" when one of the drawdown kill switches has engaged;
// "paused" when the operator toggled the pause flag; "running"
// otherwise; "error" when last tick failed (the API keeps the last
// known good status in that case).
type BotState string

const (
	StateRunning BotState = "running"
	StateHalted  BotState = "halted"
	StatePaused  BotState = "paused"
	StateError   BotState = "error"
)

// KillSwitch mirrors the per-scope drawdown halts. Halted is true when
// any of the three is engaged (daily | weekly | total).
type KillSwitch struct {
	Daily  bool `json:"daily"`
	Weekly bool `json:"weekly"`
	Total  bool `json:"total"`
	// Halted collapses the three above into the bot-level OR; the
	// frontend uses it to drive the red pill color.
	Halted bool `json:"halted"`
}

// StatusConfig is the bot's read-only runtime configuration. The bot
// does not need to expose keys; only sizing, halt thresholds, the LLM
// provider label, options toggle and the symbol universe.
type StatusConfig struct {
	MaxPositionUSD      float64  `json:"max_position_usd"`
	MaxPortfolioPct     float64  `json:"max_portfolio_pct"`
	CryptoMaxPositionUSD float64 `json:"crypto_max_position_usd"`
	DailyDDHalt         float64  `json:"daily_dd_halt"`
	WeeklyDDHalt        float64  `json:"weekly_dd_halt"`
	TotalDDHalt         float64  `json:"total_dd_halt"`
	LLMProvider         string   `json:"llm_provider"`
	OptionsEnabled      bool     `json:"options_enabled"`
	Symbols             []string `json:"symbols"`
	EnsembleEnabled     bool     `json:"ensemble_enabled"`
}

// StatusResponse is the shape of GET /api/status.
type StatusResponse struct {
	Bot        BotState     `json:"bot"`
	KillSwitch KillSwitch   `json:"kill_switch"`
	Config     StatusConfig `json:"config"`
	// TickNumber is monotonically increasing; the frontend uses it to
	// detect "new tick happened" without diffing payload bytes.
	TickNumber int64     `json:"tick_number"`
	LastTick   time.Time `json:"last_tick,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	Paused     bool      `json:"paused"` // mirror of data/paused for the UI pill
}

// ---- Account ----

// AccountResponse mirrors /v2/account from Alpaca plus a derived day
// P&L when last_close_equity is reachable (paper API sometimes omits
// it; in that case DayPnL is left zero).
type AccountResponse struct {
	Equity         string    `json:"equity"`
	Cash           string    `json:"cash"`
	DayPnL         string    `json:"day_pnl"`
	BuyingPower    string    `json:"buying_power"`
	Multiplier     string    `json:"multiplier"`
	Status         string    `json:"status"`
	AccountNumber  string    `json:"account_number"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	LastEquity     string    `json:"last_equity,omitempty"`
	PortfolioValue string    `json:"portfolio_value,omitempty"`
}

// ---- P&L ----

// EquitySnapshot is one point on the equity curve reconstructed from
// the trade log. DayPnL / drawdown_* are computed against the
// starting baseline ($100k) for the demo and against the persisted
// starting_equity (data/strategy-state.json) when ADAPTIVE_SIZING is
// in use.
type EquitySnapshot struct {
	T              time.Time `json:"t"`
	Equity         float64   `json:"equity"`
	DayPnL         float64   `json:"day_pnl"`
	DrawdownDaily  float64   `json:"drawdown_daily"`
	DrawdownWeekly float64   `json:"drawdown_weekly"`
	DrawdownTotal  float64   `json:"drawdown_total"`
}

// PnLSummary aggregates over the whole window requested.
type PnLSummary struct {
	StartingEquity float64 `json:"starting_equity"`
	CurrentEquity  float64 `json:"current_equity"`
	TotalPnL       float64 `json:"total_pnl"`
	MaxDrawdown    float64 `json:"max_drawdown"`
	Sharpe         float64 `json:"sharpe"`
	WinRate        float64 `json:"win_rate"`
	Trades         int     `json:"trades"`
}

// PnLResponse is the shape of GET /api/pnl.
type PnLResponse struct {
	Snapshots []EquitySnapshot `json:"snapshots"`
	Summary   PnLSummary       `json:"summary"`
}

// ---- Trades ----

// TradeRow is one row of GET /api/trades. factor_scores is a flat
// map (trend / momentum / volume / vol / sentiment) when the
// underlying journal entry carries them; otherwise nil.
type TradeRow struct {
	ID            string             `json:"id"`
	TS            time.Time          `json:"ts"`
	Symbol        string             `json:"symbol"`
	Side          string             `json:"side"`
	Qty           string             `json:"qty"`
	Price         string             `json:"price"`
	Status        string             `json:"status"`
	Path          string             `json:"path"` // agent | ensemble | manual | auto
	Confidence    *float64           `json:"confidence,omitempty"`
	FactorScores  map[string]float64 `json:"factor_scores,omitempty"`
	Notional      float64            `json:"notional,omitempty"`
	UnrealizedPL  string             `json:"unrealized_pl,omitempty"` // closed-loop realized P/L when known
}

// TradesResponse is the shape of GET /api/trades. next_cursor is
// the cursor for the NEXT page; null when this was the last page.
type TradesResponse struct {
	Trades     []TradeRow `json:"trades"`
	NextCursor *int64     `json:"next_cursor"`
}

// ---- Decisions ----

// DecisionRow is one row of GET /api/decisions. Risk is the
// APPROVED/REJECTED/HALT_TRADING/INFO label; source identifies the
// decision path (cli:trade, strategy:auto, strategy:ensemble,
// cli:chain, cli:factors, …); confidence is the verifier's score.
type DecisionRow struct {
	TS           time.Time          `json:"ts"`
	Symbol       string             `json:"symbol"`
	Side         string             `json:"side,omitempty"` // buy | sell | "" (reconcile, blocked, etc.)
	Risk         string             `json:"risk"`
	Source       string             `json:"source"`
	Confidence   *float64           `json:"confidence,omitempty"`
	FactorScores map[string]float64 `json:"factor_scores,omitempty"`
	Detail       string             `json:"detail,omitempty"`
}

// DecisionsResponse is the shape of GET /api/decisions.
type DecisionsResponse struct {
	Decisions   []DecisionRow `json:"decisions"`
	NextCursor  *int64        `json:"next_cursor"`
	GeneratedAt time.Time     `json:"generated_at"`
}

// ---- Positions ----

// PositionRow is one row of GET /api/positions.
type PositionRow struct {
	Symbol          string    `json:"symbol"`
	Qty             string    `json:"qty"`
	AvgEntryPrice   string    `json:"avg_entry_price"`
	CurrentPrice    string    `json:"current_price"`
	MarketValue     string    `json:"market_value"`
	UnrealizedPL    string    `json:"unrealized_pl"`
	UnrealizedPLPct string    `json:"unrealized_pl_pct"`
	ChangeToday     string    `json:"change_today,omitempty"`
	Side            string    `json:"side"`
	// Since is the entry time of the bot's last recorded bracket for
	// this symbol (zero when no bracket is stored, e.g. a position
	// the bot inherited from a prior run). The dashboard renders it
	// as a "held for 3d 4h" label next to the position row.
	Since time.Time `json:"since,omitempty"`
}

// ---- Control ----

// ControlResponse is the shape of every /api/control/* endpoint. The
// server returns the resulting pause flag and the tick number so the
// UI can refresh without a second round-trip.
type ControlResponse struct {
	Action   string    `json:"action"`              // pause | resume | step
	Paused   bool      `json:"paused"`              // resulting flag value
	Tick     int64     `json:"tick"`                // most recent bot tick at time of action
	Result   string    `json:"result,omitempty"`    // for /step, the human-readable outcome
	Decision *DecisionRow `json:"decision,omitempty"` // for /step, the decision the bot just made
}

// ---- Errors ----

// ErrorResponse is the wire shape of every non-2xx response.
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	TraceID string `json:"trace_id,omitempty"`
}

// ---- Config ----

// ConfigResponse is the wire shape of GET /api/config. Mirrors the
// knobs the bot currently has loaded so the controls form can show
// "what is" next to "what you can change to". Symbol universe is
// intentionally omitted — it is large and already exposed at
// /api/status.config.symbols.
type ConfigResponse struct {
	MaxPositionUSD       float64 `json:"max_position_usd"`
	MaxPortfolioPct      float64 `json:"max_portfolio_pct"`
	CryptoMaxPositionUSD float64 `json:"crypto_max_position_usd"`
	MinConfidence        float64 `json:"min_confidence"`
	DailyDDHalt          float64 `json:"daily_dd_halt"`
	WeeklyDDHalt         float64 `json:"weekly_dd_halt"`
	TotalDDHalt          float64 `json:"total_dd_halt"`
}

// ConfigUpdateRequest is the body shape for POST /api/config. Every
// field is optional; unset fields are left untouched. Unknown fields
// cause a 400 (extra-forbid equivalent) so a typo never silently
// drops a knob update.
type ConfigUpdateRequest struct {
	MaxPositionUSD       *float64 `json:"max_position_usd,omitempty"`
	MaxPortfolioPct      *float64 `json:"max_portfolio_pct,omitempty"`
	CryptoMaxPositionUSD *float64 `json:"crypto_max_position_usd,omitempty"`
	MinConfidence        *float64 `json:"min_confidence,omitempty"`
	DailyDDHalt          *float64 `json:"daily_dd_halt,omitempty"`
	WeeklyDDHalt         *float64 `json:"weekly_dd_halt,omitempty"`
	TotalDDHalt          *float64 `json:"total_dd_halt,omitempty"`
}

// ---- Manual Trade ----

// TradeProposalRequest is the body shape for both
// /api/trade/simulate and /api/trade/execute. Notional is the USD
// value (mutually exclusive with Qty, takes precedence when both
// are set — Alpaca semantics). ExtendedHours is honored only for
// type=limit + day|gtc; the validator rejects bad combos.
type TradeProposalRequest struct {
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"` // buy | sell
	Qty           string  `json:"qty,omitempty"`
	Notional      string  `json:"notional,omitempty"`
	OrderType     string  `json:"order_type,omitempty"`     // market | limit
	TimeInForce   string  `json:"time_in_force,omitempty"` // day | gtc
	LimitPrice    string  `json:"limit_price,omitempty"`
	ExtendedHours bool    `json:"extended_hours,omitempty"`
	Confidence    *float64 `json:"confidence,omitempty"`
}

// TradeSimulationResponse is the shape returned by
// /api/trade/simulate. approved=false is a normal outcome (the
// endpoint never 500s on a rejected proposal — only on bad input);
// the caller renders the reasons + the would-have-sent payload.
type TradeSimulationResponse struct {
	Approved      bool       `json:"approved"`
	Reasons       []string   `json:"reasons,omitempty"`
	Notional      float64    `json:"notional"`
	WouldHaveSent TradeOrder `json:"would_have_sent"`
}

// TradeExecutionResponse is the shape returned by
// /api/trade/execute. mode is "simulated" when the server has no
// Alpaca client wired (cold-start / no-key mode) or the bot is
// paused; "live" when an actual order was placed. approved=false
// always carries at least one reason.
type TradeExecutionResponse struct {
	Approved bool       `json:"approved"`
	Mode     string     `json:"mode"` // simulated | live
	Reasons  []string   `json:"reasons,omitempty"`
	Notional float64    `json:"notional"`
	Order    *TradeOrder `json:"order,omitempty"`
}

// TradeOrder is the projected/actual order envelope the dashboard
// renders next to the risk verdict. Mirrors tools.OrderRequest so
// the UI can paste it back into the Alpaca UI for confirmation.
type TradeOrder struct {
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Qty           string  `json:"qty,omitempty"`
	Notional      string  `json:"notional,omitempty"`
	OrderType     string  `json:"order_type,omitempty"`
	TimeInForce   string  `json:"time_in_force,omitempty"`
	LimitPrice    string  `json:"limit_price,omitempty"`
	ExtendedHours bool    `json:"extended_hours,omitempty"`
	ClientOrderID string  `json:"client_order_id,omitempty"`
}