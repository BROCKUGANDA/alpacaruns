// Package strategy implements the deterministic (no-LLM) trading engine:
// data ingestion -> factor scoring -> threshold decision -> timed
// execution windows -> position sizing -> TP/SL bracket management,
// across equities, crypto and options.
//
// Settings are parsed HERE, not in config/: the strategy workstream owns
// strategy/ only while config/config.go evolves concurrently in another
// workstream. Reading os.Getenv directly is safe AFTER config.Load ran,
// because config.Load materializes the selected .env file into process
// environment variables (loadEnvFile -> os.Setenv) before any consumer
// reads them. Every entry point therefore calls config.Load first and
// strategy.LoadSettings second.
package strategy

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Settings holds every knob of the deterministic engine. All values come
// from environment variables with sane paper-trading defaults.
type Settings struct {
	// Universes. Equities follow TRADING_WINDOWS; crypto follows
	// CRYPTO_WINDOWS (24/7 market, so the default is always open).
	EquitySymbols []string
	CryptoSymbols []string

	// HH:MM-HH:MM comma-separated execution windows in US/Eastern wall
	// time. Orders fire ONLY inside a window. Raw specs kept for logs.
	EquityWindowsSpec string
	CryptoWindowsSpec string

	// Fixed-fractional sizing: budget = portfolio_value * PositionPct,
	// capped at cfg.MaxPositionUSD; qty = floor(budget / price); skip
	// when qty < 1.
	PositionPct float64

	// Crypto-specific overrides (read from CRYPTO_POSITION_PCT and
	// CRYPTO_MAX_POSITION_USD). When set, crypto entries use these
	// instead of the equity PositionPct/MaxPositionUSD. Crypto assets
	// trade 24/7 and have larger unit prices (e.g. 1 BTC = ~$77k), so
	// they need bigger budgets to be buyable.
	CryptoPositionPct    float64
	CryptoMaxPositionUSD float64

	// Intraday book brackets: smaller TP/SL on 15-min bars (scalp-style).
	IntradayTPPct float64
	IntradaySLPct float64

	// Adaptive sizing: when true, the auto loop reads the live P/L each
	// tick and scales PositionPct/CryptoPositionPct by a P/L-driven
	// multiplier. In profit it scales up; in drawdown it scales down.
	// The scaling is capped to [0.25x, 2.0x] of the configured base so
	// it never goes to zero or explodes.
	AdaptiveSizing bool

	// Notional crypto orders: when true, the executor places crypto
	// entries as USD-notional (fractional) orders instead of share-qty
	// orders. Alpaca only supports notional on day orders.
	NotionalCrypto bool

	// CryptoNotionalUSD is the explicit USD amount for crypto notional
	// orders. When set (>0), the auto loop's handleBuy uses this
	// instead of the per-call SizingFor budget. Lets the user cap crypto
	CryptoNotionalUSD float64

	// MaxHoldHours, when > 0, force-closes any position held longer
	// than this many hours. The auto loop's timeBasedClose runs every
	// tick and reads Since from strategy-state.json. Default 0
	// (disabled). For a 1-month hackathon, 24-72 hours is typical.
	MaxHoldHours float64

	// ProfitTargetPct, when > 0, triggers takeProfits when account
	// equity exceeds (1 + target) * startingEquity. The most
	// profitable open position is closed to bank gains; the rest stay
	// exposed. Multiple ticks progressively lock in profit.
	// Default 0 (disabled). For a 1-month demo, 0.02-0.05 (2-5%) is
	// reasonable.
	ProfitTargetPct float64
	// TPPct > SLPct > 0 (validated in LoadSettings).
	TPPct float64
	SLPct float64

	//   trend      >= TrendBuy
	//   momentum   >= MomentumBuy
	TrendBuy    float64
	MomentumBuy float64

	// Deterministic exit gate for HELD positions (either triggers SELL):
	//   composite <= ExitComposite OR momentum <= ExitMomentum
	ExitComposite float64
	ExitMomentum  float64

	// Options overlay: when an equity BUY fires and options data is
	// available, prefer a call (put for exits) expiring between
	// OptMinDTE and OptMaxDTE days out with |delta| inside
	// [OptDeltaMin, OptDeltaMax]; contracts sized so the premium outlay
	// stays within the position budget (and thus within MaxPositionUSD).
	OptionsEnabled bool
	OptDeltaMin    float64
	OptDeltaMax    float64
	OptMinDTE      int
	OptMaxDTE      int

	// Drawdown halts, fractions of peak equity per period. A breach
	// engages the shared kill switch.
	DailyDD  float64
	WeeklyDD float64
	TotalDD  float64

	// Poll cadence reused from config (POLL_SECONDS); kept here so the
	// monitor loop and tests see one number.
	PollSeconds int

	// ---- Trading tracks ----

	// Swing track (default on): daily-bar engine over STRATEGY_* universes,
	// multi-day holds, TRADING_WINDOWS execution.
	SwingEnabled bool

	// Intraday track: independent 15-minute-bar engine over its own
	// universe, polled faster, flat by end of day.
	// INTRADAY_TRACK: off | shadow | live (shadow = dry-run logs only).
	IntradayTrack       string
	IntradaySymbols     []string // INTRADAY_EQUITY_SYMBOLS
	IntradayWindowsSpec string   // INTRADAY_TRADING_WINDOWS (US/Eastern)
	IntradayPollSeconds int      // INTRADAY_POLL_SECONDS
	PositionPctIntraday float64  // POSITION_PCT_INTRADAY sizing fraction
	FlattenEOD          bool     // FLATTEN_EOD: force-flat intraday book near close

	// Bracket placement mode (swing + intraday entries):
	// pct = fixed TP_PCT/SL_PCT percentages (legacy default);
	// atr = volatility-scaled: TP = ATR_TP_MULT x ATR14 above entry,
	// SL = ATR_SL_MULT x ATR14 below (swing-friendly breathing stops).
	BracketMode string
	ATRMultTP   float64
	ATRMultSL   float64
}

// defaults for every tunable; documented in README.md.
const (
	defEquitySymbols  = "AAPL,MSFT,NVDA"
	defCryptoSymbols  = "BTC/USD,ETH/USD"
	defEquityWindows  = "09:35-10:15,15:45-16:00"
	defCryptoWindows  = "00:00-23:59"
	defPositionPct    = 0.05
	defTPPct          = 0.08
	defSLPct          = 0.04
	defTrendBuy       = 0.7
	defMomentumBuy    = 0.6
	defExitComposite  = 0.4
	defExitMomentum   = 0.35
	defOptionsEnabled = true
	defOptDeltaMin    = 0.60
	defOptDeltaMax    = 0.70
	defOptMinDTE      = 30
	defOptMaxDTE      = 45
	defDailyDD        = 0.05
	defWeeklyDD       = 0.10
	defTotalDD        = 0.15
)

// LoadSettings parses every strategy env variable. Call it strictly AFTER
// config.Load(envFile) so .env values are already in the process env.
// getenv is injectable for tests; production passes os.Getenv.
func LoadSettings(getenv func(string) string, pollSeconds int) (Settings, error) {
	s := Settings{
		EquitySymbols:     splitSymbols(getenv("STRATEGY_EQUITY_SYMBOLS"), defEquitySymbols),
		CryptoSymbols:     splitSymbols(getenv("STRATEGY_CRYPTO_SYMBOLS"), defCryptoSymbols),
		EquityWindowsSpec: firstNonEmpty(getenv("TRADING_WINDOWS"), defEquityWindows),
		CryptoWindowsSpec: firstNonEmpty(getenv("CRYPTO_WINDOWS"), defCryptoWindows),
		PollSeconds:       pollSeconds,
	}

	var err error
	if s.PositionPct, err = envFloat(getenv, "POSITION_PCT", defPositionPct); err != nil {
		return s, err
	}
	if s.PositionPct <= 0 || s.PositionPct > 5 {
		return s, fmt.Errorf("POSITION_PCT must be in (0,5], got %g", s.PositionPct)
	}
	if s.TPPct, err = envFloat(getenv, "TP_PCT", defTPPct); err != nil {
		return s, err
	}
	if s.SLPct, err = envFloat(getenv, "SL_PCT", defSLPct); err != nil {
		return s, err
	}
	if s.SLPct <= 0 || s.TPPct <= 0 || s.TPPct <= s.SLPct {
		return s, fmt.Errorf("bracket pcts invalid: need TP_PCT > SL_PCT > 0, got TP=%g SL=%g", s.TPPct, s.SLPct)
	}

	if s.TrendBuy, err = envFloat(getenv, "TREND_BUY", defTrendBuy); err != nil {
		return s, err
	}
	if s.MomentumBuy, err = envFloat(getenv, "MOMENTUM_BUY", defMomentumBuy); err != nil {
		return s, err
	}
	if s.ExitComposite, err = envFloat(getenv, "EXIT_COMPOSITE", defExitComposite); err != nil {
		return s, err
	}
	if s.ExitMomentum, err = envFloat(getenv, "EXIT_MOMENTUM", defExitMomentum); err != nil {
		return s, err
	}
	for name, v := range map[string]float64{
		"TREND_BUY": s.TrendBuy, "MOMENTUM_BUY": s.MomentumBuy,
		"EXIT_COMPOSITE": s.ExitComposite, "EXIT_MOMENTUM": s.ExitMomentum,
	} {
		if v < 0 || v > 1 {
			return s, fmt.Errorf("%s must be in [0,1], got %g", name, v)
		}
	}

	if s.OptionsEnabled, err = envBool(getenv, "OPTIONS_ENABLED", defOptionsEnabled); err != nil {
		return s, err
	}
	if s.OptDeltaMin, err = envFloat(getenv, "OPTION_DELTA_MIN", defOptDeltaMin); err != nil {
		return s, err
	}
	if s.OptDeltaMax, err = envFloat(getenv, "OPTION_DELTA_MAX", defOptDeltaMax); err != nil {
		return s, err
	}
	if s.OptDeltaMin <= 0 || s.OptDeltaMin > s.OptDeltaMax || s.OptDeltaMax >= 1 {
		return s, fmt.Errorf("option deltas invalid: need 0 < OPTION_DELTA_MIN <= OPTION_DELTA_MAX < 1, got [%g,%g]",
			s.OptDeltaMin, s.OptDeltaMax)
	}
	if s.OptMinDTE, err = envInt(getenv, "OPTION_MIN_DTE", defOptMinDTE); err != nil {
		return s, err
	}
	if s.OptMaxDTE, err = envInt(getenv, "OPTION_MAX_DTE", defOptMaxDTE); err != nil {
		return s, err
	}
	if s.OptMinDTE <= 0 || s.OptMinDTE > s.OptMaxDTE {
		return s, fmt.Errorf("option DTE window invalid: need 0 < OPTION_MIN_DTE <= OPTION_MAX_DTE, got [%d,%d]",
			s.OptMinDTE, s.OptMaxDTE)
	}

	if s.DailyDD, err = envFloat(getenv, "DAILY_DD_HALT", defDailyDD); err != nil {
		return s, err
	}
	if s.WeeklyDD, err = envFloat(getenv, "WEEKLY_DD_HALT", defWeeklyDD); err != nil {
		return s, err
	}
	if s.TotalDD, err = envFloat(getenv, "TOTAL_DD_HALT", defTotalDD); err != nil {
		return s, err
	}
	for name, v := range map[string]float64{
		"DAILY_DD_HALT": s.DailyDD, "WEEKLY_DD_HALT": s.WeeklyDD, "TOTAL_DD_HALT": s.TotalDD,
	} {
		if v <= 0 || v >= 1 {
			return s, fmt.Errorf("%s must be in (0,1), got %g", name, v)
		}
	}

	// ---- Trading tracks ----
	if s.SwingEnabled, err = envBool(getenv, "SWING_ENABLED", true); err != nil {
		return s, err
	}
	switch s.IntradayTrack = strings.ToLower(strings.TrimSpace(getenv("INTRADAY_TRACK"))); s.IntradayTrack {
	case "", "off", "shadow", "live":
		// "" normalizes to off
		if s.IntradayTrack == "" {
			s.IntradayTrack = "off"
		}
	default:
		return s, fmt.Errorf("INTRADAY_TRACK must be off|shadow|live, got %q", getenv("INTRADAY_TRACK"))
	}
	s.IntradaySymbols = splitSymbols(getenv("INTRADAY_EQUITY_SYMBOLS"), "SPY,QQQ,AAPL")
	s.IntradayWindowsSpec = firstNonEmpty(getenv("INTRADAY_TRADING_WINDOWS"), "09:35-11:30,13:30-15:45")
	if s.IntradayPollSeconds, err = envInt(getenv, "INTRADAY_POLL_SECONDS", 60); err != nil {
		return s, err
	}
	if s.IntradayPollSeconds < 15 {
		return s, fmt.Errorf("INTRADAY_POLL_SECONDS must be >= 15, got %d", s.IntradayPollSeconds)
	}
	if s.PositionPctIntraday, err = envFloat(getenv, "POSITION_PCT_INTRADAY", 0.02); err != nil {
		return s, err
	}
	if s.PositionPctIntraday <= 0 || s.PositionPctIntraday > 1 {
		return s, fmt.Errorf("POSITION_PCT_INTRADAY must be in (0,1], got %g", s.PositionPctIntraday)
	}

	// Intraday book brackets: tight TP/SL on 15-min bars. Defaults to
	// 1.0% / 0.5% when unset, suitable for a 1-month scalping showcase.
	if s.IntradayTPPct, err = envFloat(getenv, "INTRADAY_TP_PCT", 0.01); err != nil {
		return s, err
	}
	if s.IntradaySLPct, err = envFloat(getenv, "INTRADAY_SL_PCT", 0.005); err != nil {
		return s, err
	}
	if s.IntradayTPPct <= 0 || s.IntradaySLPct <= 0 || s.IntradayTPPct <= s.IntradaySLPct {
		return s, fmt.Errorf("intraday brackets invalid: need INTRADAY_TP_PCT > INTRADAY_SL_PCT > 0, got TP=%g SL=%g", s.IntradayTPPct, s.IntradaySLPct)
	}

	// Crypto-specific sizing overrides. Falls back to equity PositionPct /
	// 0 (no extra cap) when unset, preserving pre-existing behavior.
	if s.CryptoPositionPct, err = envFloat(getenv, "CRYPTO_POSITION_PCT", 0); err != nil {
		return s, err
	}
	if s.CryptoPositionPct < 0 || s.CryptoPositionPct > 5 {
		return s, fmt.Errorf("CRYPTO_POSITION_PCT must be in [0,5], got %g", s.CryptoPositionPct)
	}
	if s.CryptoPositionPct == 0 {
		s.CryptoPositionPct = s.PositionPct
	}
	if s.CryptoMaxPositionUSD, err = envFloat(getenv, "CRYPTO_MAX_POSITION_USD", 0); err != nil {
		return s, err
	}
	if s.CryptoMaxPositionUSD < 0 {
		return s, fmt.Errorf("CRYPTO_MAX_POSITION_USD must be >= 0, got %g", s.CryptoMaxPositionUSD)
	}
	if s.CryptoNotionalUSD, err = envFloat(getenv, "CRYPTO_NOTIONAL_USD", 0); err != nil {
		return s, err
	}
	if s.CryptoNotionalUSD < 0 {
		return s, fmt.Errorf("CRYPTO_NOTIONAL_USD must be >= 0, got %g", s.CryptoNotionalUSD)
	}
	if s.MaxHoldHours, err = envFloat(getenv, "MAX_HOLD_HOURS", 0); err != nil {
		return s, err
	}
	if s.MaxHoldHours < 0 {
		return s, fmt.Errorf("MAX_HOLD_HOURS must be >= 0, got %g", s.MaxHoldHours)
	}
	if s.ProfitTargetPct, err = envFloat(getenv, "PROFIT_TARGET_PCT", 0); err != nil {
		return s, err
	}
	if s.ProfitTargetPct < 0 {
		return s, fmt.Errorf("PROFIT_TARGET_PCT must be >= 0, got %g", s.ProfitTargetPct)
	}
	if s.AdaptiveSizing, err = envBool(getenv, "ADAPTIVE_SIZING", false); err != nil {
		return s, err
	}
	// Notional crypto orders toggle.
	if s.NotionalCrypto, err = envBool(getenv, "NOTIONAL_CRYPTO", false); err != nil {
		return s, err
	}
	if s.CryptoNotionalUSD, err = envFloat(getenv, "CRYPTO_NOTIONAL_USD", 0); err != nil {
		return s, err
	}
	if s.CryptoNotionalUSD < 0 {
		return s, fmt.Errorf("CRYPTO_NOTIONAL_USD must be >= 0, got %g", s.CryptoNotionalUSD)
	}
	s.BracketMode = strings.ToLower(strings.TrimSpace(firstNonEmpty(getenv("BRACKET_MODE"), "pct")))
	if s.BracketMode != "pct" && s.BracketMode != "atr" {
		return s, fmt.Errorf("BRACKET_MODE must be pct|atr, got %q", getenv("BRACKET_MODE"))
	}
	if s.BracketMode == "atr" {
		if s.ATRMultTP, err = envFloat(getenv, "ATR_TP_MULT", 3.0); err != nil {
			return s, err
		}
		if s.ATRMultSL, err = envFloat(getenv, "ATR_SL_MULT", 1.5); err != nil {
			return s, err
		}
		if s.ATRMultTP <= s.ATRMultSL || s.ATRMultSL <= 0 {
			return s, fmt.Errorf("ATR multiples invalid: need ATR_TP_MULT > ATR_SL_MULT > 0, got %g/%g",
				s.ATRMultTP, s.ATRMultSL)
		}
	}
	return s, nil
}

// splitSymbols parses a comma-separated universe with whitespace tolerance.
func splitSymbols(raw, def string) []string {
	spec := firstNonEmpty(strings.TrimSpace(raw), def)
	var out []string
	for _, part := range strings.Split(spec, ",") {
		if s := strings.ToUpper(strings.TrimSpace(part)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func envFloat(getenv func(string) string, key string, def float64) (float64, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid number", key, raw)
	}
	return f, nil
}

func envInt(getenv func(string) string, key string, def int) (int, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid integer", key, raw)
	}
	return n, nil
}

func envBool(getenv func(string) string, key string, def bool) (bool, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a valid boolean", key, raw)
	}
	return b, nil
}

// OsGetenv is the production environment reader.
func OsGetenv(key string) string { return os.Getenv(key) }

// IsCrypto reports whether a symbol trades on Alpaca's crypto venue
// (BASE/QUOTE form such as BTC/USD) rather than the equity venue.
// OCC option symbols never contain "/", so there is no ambiguity.
func IsCrypto(symbol string) bool {
	return strings.Contains(strings.ToUpper(symbol), "/")
}

// SizingFor returns the appropriate Sizing for a symbol. Equities use
// PositionPct + cfg.MaxPositionUSD; crypto uses the Crypto* overrides
// when set, falling back to the equity values when unset. This lets the
// caller route sizing through one helper without duplicating the
// per-asset-class logic at every entry site.
func (s Settings) SizingFor(symbol string, cfgMaxPositionUSD float64) Sizing {
	if IsCrypto(symbol) {
		maxUSD := s.CryptoMaxPositionUSD
		if maxUSD == 0 {
			maxUSD = cfgMaxPositionUSD
		}
		return Sizing{PositionPct: s.CryptoPositionPct, MaxPositionUSD: maxUSD}
	}
	return Sizing{PositionPct: s.PositionPct, MaxPositionUSD: cfgMaxPositionUSD}
}


// SanitizeSymbol renders a venue symbol safe for embedding in
// client_order_id strings ("/" and spaces removed).
func SanitizeSymbol(s string) string { return sanitize(s) }
