// Package config loads Alpacaruns settings from environment / .env.
package config

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Mode controls whether trades require human approval.
type Mode string

const (
	ModeSupervised Mode = "supervised"
	ModeAutonomous Mode = "autonomous"
)

// LLMProvider selects which backend serves the agents' model.LLM.
type LLMProvider string

const (
	ProviderLLamaCPP LLMProvider = "llamacpp"
	ProviderOxlo     LLMProvider = "oxlo"
	ProviderGemini   LLMProvider = "gemini"
)

// DefaultOxloModel is used when LLM_PROVIDER=oxlo and LLM_MODEL is unset.
// Chosen from a live probe of api.oxlo.ai/v1 (2026-08): gpt-oss-20b emitted
// well-formed tool_calls on every attempt at the lowest stable latency
// (~1.6-2.4s round-trip) among catalog models advertising tool_calling.
const DefaultOxloModel = "gpt-oss-20b"

// Config holds all runtime settings.
type Config struct {
	AlpacaKeyID   string
	AlpacaSecret  string
	AlpacaBaseURL string // trading API; paper by default
	AlpacaDataURL string // market data API
	// AlpacaMCPSpec overrides the pinned uvx spec for the Alpaca MCP server
	// (default: agents.DefaultMCPServerSpec). Changing it changes which tools
	// the risk guard is classifying, so it is an explicit act.
	AlpacaMCPSpec string
	// DashboardTokens names the operators who may drive the control
	// endpoints, "alice:token,bob:token". One shared token cannot say WHO
	// acted, and revoking one operator means rotating everyone's credential;
	// named tokens give attribution in the journal and per-name revocation.
	// DASHBOARD_TOKEN remains supported as the unnamed fallback.
	DashboardTokens map[string]string
	// OIDC lets operators present a token from an identity provider instead
	// of a static secret. All three are required together: an issuer with no
	// audience cannot be verified, and a key URL with no issuer would accept
	// tokens minted for someone else.
	OIDCIssuer   string
	OIDCAudience string
	OIDCJWKSURL  string
	// OptionPuts allows a BEARISH signal on a name the bot does not hold to
	// be expressed as a long put (defined risk). The engine never shorts, so
	// without this a bearish view is simply inaction.
	OptionPuts bool
	// Universe screening and live cost/margin limits.
	//
	// MinMarketCapUSD turns on the market-cap screen for single-name
	// equities (0 = off). ETFs, index trackers and crypto are exempt by
	// class: a fund has AUM rather than a market cap and a crypto pair has
	// no share count.
	MinMarketCapUSD float64
	MaxMarketCapUSD float64
	// MarketCapMaxAgeDays refuses to screen on a stale cap table.
	MarketCapMaxAgeDays int
	// MaxSpreadBps skips an entry whose quoted spread is wider than this
	// (0 = off). It is a liquidity filter, not a cost model.
	MaxSpreadBps float64
	// MaxGrossExposurePct caps the book's gross market value as a share of
	// equity. 1.0 means no leverage; 2.0 would allow a 2x book in margin.
	MaxGrossExposurePct float64
	// FactorMinScoreByClass overrides the composite floor per asset class
	// (equity, etf, index, crypto, option). A class absent here uses
	// FactorMinScore. Crypto and equities do not share a score
	// distribution, so one floor calibated on equities silently excludes
	// the other class.
	FactorMinScoreByClass map[string]float64
	// MirrorEnabled keeps a verified SQLite mirror of the journal and state
	// next to them. The files remain the write path; see package store.
	MirrorEnabled        bool
	GeminiAPIKey         string
	Mode                 Mode
	PollSeconds          int     // monitoring loop interval
	MinConfidence        float64 // autonomous mode gate
	MaxPositionUSD       float64 // per-trade cap for equities
	CryptoMaxPositionUSD float64 // per-trade cap for crypto notional (BTC/ETH need bigger)
	MaxPortfolioPct      float64 // max % of portfolio in one position

	ExtendedHours bool   // default extended_hours=true on agent orders
	PreOrders     bool   // queue resting limit/GTC bracket pre-orders while market closed
	StreamFeed    string // market-data stream feed: iex | sip
	TradeLog      string // path to the JSONL trade log

	// Rest-of-hours trading. EXTENDED_HOURS is the switch; the keys below
	// shape it, because the extended session is a different market from the
	// one the strategy was calibrated in: thinner book, wider spread, and no
	// broker-side stop (Alpaca refuses extended_hours on bracket and OCO
	// orders), so the bot's own monitoring is the only protection.
	//
	// ExtendedHoursWindows is the HH:MM-HH:MM spec, US/Eastern, inside which
	// entries may open in the extended session. It is deliberately narrower
	// than the venue's 04:00-09:30 / 16:00-20:00: the first and last minutes
	// of a session are where the quote is thinnest and where an order placed
	// against a stale print is most likely to be filled at the stale price.
	ExtendedHoursWindows string
	// ExtendedHoursMaxSpreadBps is the liquidity bar for extended-session
	// entries. 0 means "use MAX_SPREAD_BPS". Set it higher and the extended
	// session trades the same book as the regular one, which is the claim
	// the measurement contradicts.
	ExtendedHoursMaxSpreadBps float64
	// ExtendedHoursMaxQuoteAgeSeconds refuses an extended-session entry or
	// exit whose quote print is older than this. Outside the regular session
	// a snapshot keeps serving its last print — measured on a Sunday, AAPL's
	// quote was Friday 20:00:03Z with a 10% spread — so price and freshness
	// must be checked as one condition, not separately.
	ExtendedHoursMaxQuoteAgeSeconds int

	// Local LLM (llama.cpp llama-server). When LLMBaseURL is set it takes
	// precedence over Gemini.
	LLMBaseURL string // e.g. http://127.0.0.1:8080
	LLMModel   string // e.g. Qwen3-4B-Instruct-Q4_K_M
	// Cloud LLM providers. LLMProvider selects the backend explicitly;
	// when empty it is derived: LLMBaseURL set -> llamacpp, else
	// GEMINI_API_KEY set -> gemini. OxloAPIKey authenticates against the
	// Oxlo.ai OpenAI-compatible endpoint.
	LLMProvider LLMProvider // "llamacpp" | "oxlo" | "gemini"
	OxloAPIKey  string      // Oxlo.ai API key (secret)
	OxloBaseURL string      // defaults to https://api.oxlo.ai/v1

	// Multi-factor decision engine (factors/). FactorWeights maps factor
	// names to weights for the composite score; FactorMinScore is the
	// minimum composite a proposal needs in addition to MinConfidence.
	FactorWeights  map[string]float64
	FactorMinScore float64

	// Agent-to-agent bridge. The ingress is opt-in: A2AEnabled must be
	// true AND a token must be configured before any peer agent can
	// post a signal.
	A2AEnabled      bool
	A2AToken        string   // bearer token required on inbound signals
	A2AAgents       []string // allow-list of peer agent ids
	A2AMode         string   // default ramp position: off | shadow | on
	A2AMaxNotional  float64  // per-signal USD cap
	A2AShadowPasses int      // shadow evaluations needed before ramp-on
	// A2APeerTokens binds a credential to one agent id (agent -> token),
	// parsed from A2A_PEER_TOKENS=agent:token,agent2:token2. With one
	// shared token any holder can claim ANY allow-listed agent id —
	// including one ramped ON, whose signals execute. A peer with its own
	// token can only be impersonated by someone holding that token; agents
	// without an entry keep using the shared one.
	A2APeerTokens map[string]string

	// Learning: the bot recalibrates its own entry bar, sizing and stops
	// from realized results once enough trades have closed. Every bound
	// moves only in the risk-reducing direction (see the learning
	// package), and the whole feature is off unless LEARN_ENABLED=true.
	LearnEnabled          bool
	LearnMinTrades        int     // closed trades needed before first calibration
	LearnEveryTrades      int     // recalibrate every N further closed trades
	LearnMinSizeMult      float64 // floor on how far learning may shrink sizing
	LearnMaxTightenMult   float64 // floor on how far learning may tighten stops
	LearnQuarantineWinPct float64 // win rate below which a symbol is quarantined
	LearnQuarantineTrades int     // trades a symbol needs before quarantine applies
	LearnMinPerClass      int     // samples a class needs before it gets an adjustment
	LearnScoreBuckets     int     // entry-score buckets used for the expectancy curve

	// Self-heal: detect and repair degraded state (orphaned protective
	// orders, duplicate brackets, stale resting entries, corrupt state).
	SelfHealEnabled     bool
	SelfHealReportOnly  bool // find and journal, but never act
	SelfHealLiftHalt    bool // opt-in: allow an engaged halt to be lifted
	SelfHealOrphanHours int  // age after which an orphan order is cancelled
	SelfHealEveryTicks  int  // run the pass every N ticks
}

// Load reads config from the environment, applying defaults.
// If envFile is non-empty it is parsed first (KEY=VALUE lines).
func Load(envFile string) (*Config, error) {
	if envFile != "" {
		if err := loadEnvFile(envFile); err != nil {
			return nil, fmt.Errorf("load env file: %w", err)
		}
	}
	c := &Config{
		AlpacaKeyID:   os.Getenv("ALPACA_API_KEY_ID"),
		AlpacaSecret:  os.Getenv("ALPACA_SECRET_KEY"),
		AlpacaBaseURL: getEnv("ALPACA_BASE_URL", "https://paper-api.alpaca.markets/v2"),
		AlpacaDataURL: getEnv("ALPACA_DATA_URL", "https://data.alpaca.markets/v2"),
		AlpacaMCPSpec: strings.TrimSpace(os.Getenv("ALPACA_MCP_SPEC")),
		GeminiAPIKey:  os.Getenv("GEMINI_API_KEY"),
		Mode:          Mode(getEnv("MODE", string(ModeSupervised))),
	}
	if poll, err := getInt("POLL_SECONDS", 300); err != nil {
		return nil, err
	} else if poll <= 0 {
		return nil, fmt.Errorf("POLL_SECONDS must be > 0, got %d", poll)
	} else {
		c.PollSeconds = poll
	}
	if mc, err := getFloat("MIN_CONFIDENCE", 0.7); err != nil {
		return nil, err
	} else if mc < 0 || mc > 1 {
		return nil, fmt.Errorf("MIN_CONFIDENCE must be in [0,1], got %g", mc)
	} else {
		c.MinConfidence = mc
	}
	if mp, err := getFloat("MAX_POSITION_USD", 10000); err != nil {
		return nil, err
	} else if mp <= 0 {
		return nil, fmt.Errorf("MAX_POSITION_USD must be > 0, got %g", mp)
	} else {
		c.MaxPositionUSD = mp
	}
	if cmp, err := getFloat("CRYPTO_MAX_POSITION_USD", 0); err != nil {
		return nil, err
	} else if cmp < 0 {
		return nil, fmt.Errorf("CRYPTO_MAX_POSITION_USD must be >= 0, got %g", cmp)
	} else {
		c.CryptoMaxPositionUSD = cmp
	}
	if c.CryptoMaxPositionUSD == 0 {
		c.CryptoMaxPositionUSD = c.MaxPositionUSD // fall back to equity cap
	}
	if pc, err := getFloat("MAX_PORTFOLIO_PCT", 0.20); err != nil {
		return nil, err
	} else if pc <= 0 || pc > 1 {
		return nil, fmt.Errorf("MAX_PORTFOLIO_PCT must be in (0,1], got %g", pc)
	} else {
		c.MaxPortfolioPct = pc
	}
	fw, err := getFactorWeights("FACTOR_WEIGHTS", DefaultFactorWeights)
	if err != nil {
		return nil, err
	}
	c.FactorWeights = fw
	if fms, err := getFloat("FACTOR_MIN_SCORE", 0.6); err != nil {
		return nil, err
	} else if fms < 0 || fms > 1 {
		return nil, fmt.Errorf("FACTOR_MIN_SCORE must be in [0,1], got %g", fms)
	} else {
		c.FactorMinScore = fms
	}
	c.LLMBaseURL = os.Getenv("LLM_BASE_URL")
	// Keep LLM_MODEL empty when unset so the oxlo branch below can apply
	// its own default; llama.cpp keeps its long-standing fallback.
	c.LLMModel = getEnv("LLM_MODEL", "")
	if eh, err := getBool("EXTENDED_HOURS", false); err != nil {
		return nil, err
	} else {
		c.ExtendedHours = eh
	}
	c.ExtendedHoursWindows = strings.TrimSpace(getEnv("EXTENDED_HOURS_WINDOWS", "04:30-09:25,16:05-19:55"))
	if err := validateWindowSpec("EXTENDED_HOURS_WINDOWS", c.ExtendedHoursWindows); err != nil {
		return nil, err
	}
	if f, err := getFloat("EXTENDED_HOURS_MAX_SPREAD_BPS", 0); err != nil {
		return nil, err
	} else if f < 0 {
		return nil, fmt.Errorf("EXTENDED_HOURS_MAX_SPREAD_BPS must be >= 0, got %g", f)
	} else {
		c.ExtendedHoursMaxSpreadBps = f
	}
	if n, err := getInt("EXTENDED_HOURS_MAX_QUOTE_AGE_SECONDS", 300); err != nil {
		return nil, err
	} else if n <= 0 {
		return nil, fmt.Errorf("EXTENDED_HOURS_MAX_QUOTE_AGE_SECONDS must be > 0, got %d", n)
	} else {
		c.ExtendedHoursMaxQuoteAgeSeconds = n
	}
	// PRE_ORDERS queues equity entries as resting limit/GTC brackets while
	// the market is closed (overnight, weekend): sized at the last daily
	// close instead of a live snapshot. Default false — off-hours behavior
	// only changes when explicitly enabled.
	if po, err := getBool("PRE_ORDERS", false); err != nil {
		return nil, err
	} else {
		c.PreOrders = po
	}

	tokens, err := parseNamedTokens("DASHBOARD_TOKENS", os.Getenv("DASHBOARD_TOKENS"))
	if err != nil {
		return nil, err
	}
	c.DashboardTokens = tokens
	c.OIDCIssuer = strings.TrimSpace(getEnv("OIDC_ISSUER", ""))
	c.OIDCAudience = strings.TrimSpace(getEnv("OIDC_AUDIENCE", ""))
	c.OIDCJWKSURL = strings.TrimSpace(getEnv("OIDC_JWKS_URL", ""))
	set := 0
	for _, v := range []string{c.OIDCIssuer, c.OIDCAudience, c.OIDCJWKSURL} {
		if v != "" {
			set++
		}
	}
	if set != 0 && set != 3 {
		return nil, fmt.Errorf("OIDC_ISSUER, OIDC_AUDIENCE and OIDC_JWKS_URL must be set together (got %d of 3)", set)
	}

	if put, err := getBool("OPTIONS_PUTS", true); err != nil {
		return nil, err
	} else {
		c.OptionPuts = put
	}
	if f, err := getFloat("MIN_MARKET_CAP_USD", 0); err != nil {
		return nil, err
	} else if f < 0 {
		return nil, fmt.Errorf("MIN_MARKET_CAP_USD must be >= 0, got %g", f)
	} else {
		c.MinMarketCapUSD = f
	}
	if f, err := getFloat("MAX_MARKET_CAP_USD", 0); err != nil {
		return nil, err
	} else {
		c.MaxMarketCapUSD = f
	}
	if n, err := getInt("MARKET_CAP_MAX_AGE_DAYS", 7); err != nil {
		return nil, err
	} else if n < 1 {
		return nil, fmt.Errorf("MARKET_CAP_MAX_AGE_DAYS must be >= 1, got %d", n)
	} else {
		c.MarketCapMaxAgeDays = n
	}
	if f, err := getFloat("MAX_SPREAD_BPS", 0); err != nil {
		return nil, err
	} else if f < 0 {
		return nil, fmt.Errorf("MAX_SPREAD_BPS must be >= 0, got %g", f)
	} else {
		c.MaxSpreadBps = f
	}
	if f, err := getFloat("MAX_GROSS_EXPOSURE_PCT", 1.0); err != nil {
		return nil, err
	} else if f <= 0 {
		return nil, fmt.Errorf("MAX_GROSS_EXPOSURE_PCT must be > 0, got %g", f)
	} else {
		c.MaxGrossExposurePct = f
	}
	c.FactorMinScoreByClass = map[string]float64{}
	for _, cls := range []string{"equity", "etf", "index", "crypto", "option"} {
		key := "FACTOR_MIN_SCORE_" + strings.ToUpper(cls)
		if f, err := getFloat(key, 0); err != nil {
			return nil, err
		} else if f > 0 {
			c.FactorMinScoreByClass[cls] = f
		}
	}

	// SQLITE_MIRROR keeps a verified SQLite mirror of the journal and state
	// (default on). It is a queryable view, not the write path; set it to
	// false to run with the files alone.
	if mm, err := getBool("SQLITE_MIRROR", true); err != nil {
		return nil, err
	} else {
		c.MirrorEnabled = mm
	}

	switch p := LLMProvider(strings.ToLower(strings.TrimSpace(getEnv("LLM_PROVIDER", "")))); p {
	case ProviderLLamaCPP, ProviderOxlo, ProviderGemini:
		c.LLMProvider = p
	case "":
		// Derived default: local llama.cpp when a base URL is present,
		// otherwise Gemini when its key is set.
		if c.LLMBaseURL != "" {
			c.LLMProvider = ProviderLLamaCPP
		} else if c.GeminiAPIKey != "" {
			c.LLMProvider = ProviderGemini
		}
	default:
		return nil, fmt.Errorf("LLM_PROVIDER must be %q, %q or %q, got %q",
			ProviderLLamaCPP, ProviderOxlo, ProviderGemini, p)
	}
	if c.LLMProvider != ProviderOxlo && strings.TrimSpace(c.LLMModel) == "" {
		c.LLMModel = "qwen3.7b-instruct-q4_k_m"
	}
	c.OxloAPIKey = os.Getenv("OXLO_API_KEY")
	c.OxloBaseURL = getEnv("OXLO_BASE_URL", "https://api.oxlo.ai/v1")
	if c.LLMProvider == ProviderOxlo && strings.TrimSpace(c.LLMModel) == "" {
		c.LLMModel = DefaultOxloModel
	}
	// Learning configuration. Off by default: an autonomous parameter
	// change should be a deliberate act, and the engine needs a real
	// trade history before any estimate is worth applying.
	if le, err := getBool("LEARN_ENABLED", false); err != nil {
		return nil, err
	} else {
		c.LearnEnabled = le
	}
	if n, err := getInt("LEARN_MIN_TRADES", 12); err != nil {
		return nil, err
	} else if n < 2 {
		return nil, fmt.Errorf("LEARN_MIN_TRADES must be >= 2, got %d", n)
	} else {
		c.LearnMinTrades = n
	}
	if n, err := getInt("LEARN_EVERY_TRADES", 5); err != nil {
		return nil, err
	} else if n < 1 {
		return nil, fmt.Errorf("LEARN_EVERY_TRADES must be >= 1, got %d", n)
	} else {
		c.LearnEveryTrades = n
	}
	if f, err := getFloat("LEARN_MIN_SIZE_MULTIPLIER", 0.25); err != nil {
		return nil, err
	} else if f <= 0 || f > 1 {
		return nil, fmt.Errorf("LEARN_MIN_SIZE_MULTIPLIER must be in (0,1], got %g", f)
	} else {
		c.LearnMinSizeMult = f
	}
	if f, err := getFloat("LEARN_MAX_TIGHTEN_FACTOR", 0.75); err != nil {
		return nil, err
	} else if f <= 0 || f > 1 {
		return nil, fmt.Errorf("LEARN_MAX_TIGHTEN_FACTOR must be in (0,1], got %g", f)
	} else {
		c.LearnMaxTightenMult = f
	}
	if f, err := getFloat("LEARN_QUARANTINE_WIN_RATE", 0.25); err != nil {
		return nil, err
	} else if f < 0 || f > 1 {
		return nil, fmt.Errorf("LEARN_QUARANTINE_WIN_RATE must be in [0,1], got %g", f)
	} else {
		c.LearnQuarantineWinPct = f
	}
	if n, err := getInt("LEARN_QUARANTINE_TRADES", 6); err != nil {
		return nil, err
	} else if n < 2 {
		return nil, fmt.Errorf("LEARN_QUARANTINE_TRADES must be >= 2, got %d", n)
	} else {
		c.LearnQuarantineTrades = n
	}
	if n, err := getInt("LEARN_MIN_SAMPLES_PER_CLASS", 8); err != nil {
		return nil, err
	} else if n < 2 {
		return nil, fmt.Errorf("LEARN_MIN_SAMPLES_PER_CLASS must be >= 2, got %d", n)
	} else {
		c.LearnMinPerClass = n
	}
	if n, err := getInt("LEARN_SCORE_BUCKETS", 5); err != nil {
		return nil, err
	} else if n < 2 {
		return nil, fmt.Errorf("LEARN_SCORE_BUCKETS must be >= 2, got %d", n)
	} else {
		c.LearnScoreBuckets = n
	}

	// Self-heal configuration. On by default: leaving an orphaned
	// protective order resting against a position that no longer exists
	// is a short sale waiting to happen, and that should fix itself.
	if sh, err := getBool("SELFHEAL_ENABLED", true); err != nil {
		return nil, err
	} else {
		c.SelfHealEnabled = sh
	}
	if ro, err := getBool("SELFHEAL_REPORT_ONLY", false); err != nil {
		return nil, err
	} else {
		c.SelfHealReportOnly = ro
	}
	if lh, err := getBool("SELFHEAL_LIFT_HALT", false); err != nil {
		return nil, err
	} else {
		c.SelfHealLiftHalt = lh
	}
	if n, err := getInt("SELFHEAL_ORPHAN_MAX_AGE_HOURS", 24); err != nil {
		return nil, err
	} else if n < 0 {
		return nil, fmt.Errorf("SELFHEAL_ORPHAN_MAX_AGE_HOURS must be >= 0, got %d", n)
	} else {
		c.SelfHealOrphanHours = n
	}
	if n, err := getInt("SELFHEAL_EVERY_TICKS", 6); err != nil {
		return nil, err
	} else if n < 1 {
		return nil, fmt.Errorf("SELFHEAL_EVERY_TICKS must be >= 1, got %d", n)
	} else {
		c.SelfHealEveryTicks = n
	}

	// Agent-to-agent bridge configuration. Off by default: an open
	// signal ingress is an attack surface, so it has to be enabled
	// deliberately and needs a token.
	if a2a, err := getBool("A2A_ENABLED", false); err != nil {
		return nil, err
	} else {
		c.A2AEnabled = a2a
	}
	c.A2AToken = os.Getenv("A2A_TOKEN")
	c.A2AAgents = splitCSV(os.Getenv("A2A_AGENTS"))
	c.A2AMode = strings.ToLower(strings.TrimSpace(getEnv("A2A_MODE", "off")))
	switch c.A2AMode {
	case "off", "shadow", "on":
	default:
		return nil, fmt.Errorf("A2A_MODE must be %q, %q or %q, got %q", "off", "shadow", "on", c.A2AMode)
	}
	if mn, err := getFloat("A2A_MAX_NOTIONAL", 10000); err != nil {
		return nil, err
	} else if mn <= 0 {
		return nil, fmt.Errorf("A2A_MAX_NOTIONAL must be > 0, got %g", mn)
	} else {
		c.A2AMaxNotional = mn
	}
	if sp, err := getInt("A2A_SHADOW_PASSES", 3); err != nil {
		return nil, err
	} else if sp < 0 {
		return nil, fmt.Errorf("A2A_SHADOW_PASSES must be >= 0, got %d", sp)
	} else {
		c.A2AShadowPasses = sp
	}
	// Per-peer credentials, "agent:token,agent2:token2". Malformed entries
	// are an error rather than silently skipped: a peer the operator
	// believed was bound to its own token but was not would keep accepting
	// the shared group token.
	peers, err := parsePeerTokens(os.Getenv("A2A_PEER_TOKENS"))
	if err != nil {
		return nil, err
	}
	c.A2APeerTokens = peers
	if c.A2AEnabled && strings.TrimSpace(c.A2AToken) == "" && len(c.A2APeerTokens) == 0 {
		return nil, fmt.Errorf("A2A_ENABLED requires A2A_TOKEN (or A2A_PEER_TOKENS) to be set: the ingress has no anonymous mode")
	}
	c.TradeLog = getEnv("TRADE_LOG", "data/trades.jsonl")
	feed := strings.ToLower(strings.TrimSpace(getEnv("STREAM_FEED", "iex")))
	c.StreamFeed = feed
	if feed != "iex" && feed != "sip" {
		return nil, fmt.Errorf("STREAM_FEED must be %q or %q, got %q", "iex", "sip", feed)
	}
	c.StreamFeed = feed
	if c.AlpacaKeyID == "" || c.AlpacaSecret == "" {
		return nil, fmt.Errorf("ALPACA_API_KEY_ID and ALPACA_SECRET_KEY must be set")
	}
	if c.Mode != ModeSupervised && c.Mode != ModeAutonomous {
		return nil, fmt.Errorf("MODE must be %q or %q, got %q", ModeSupervised, ModeAutonomous, c.Mode)
	}
	// Live trading must never be reached silently: promoting from paper
	// requires BOTH a live ALPACA_BASE_URL and the explicit acknowledgement
	// variable I_ALPACA_LIVE=YES set in the environment.
	if !strings.Contains(c.AlpacaBaseURL, "paper-api") && os.Getenv("I_ALPACA_LIVE") != "YES" {
		return nil, fmt.Errorf(
			"refusing live trading: ALPACA_BASE_URL=%s is not paper; set I_ALPACA_LIVE=YES to acknowledge live mode",
			c.AlpacaBaseURL)
	}
	// Cross-key rules run last, once every value is parsed: MAX_SPREAD_BPS is
	// read further down this function than the extended-hours keys, and a
	// pairing check inlined with either key would be reading a zero that has
	// not been filled in yet — which is how a config validation starts
	// rejecting the very setting it exists to protect.
	//
	// Extended trading without a liquidity bar is extended trading on a quote
	// nobody is refreshing: the session is thinner than the one the strategy
	// was calibrated in, and an unbounded spread turns a signal into a payment
	// to whoever is showing the only quote. Refuse the combination at load.
	if c.ExtendedHours && c.MaxSpreadBps <= 0 && c.ExtendedHoursMaxSpreadBps <= 0 {
		return nil, fmt.Errorf("EXTENDED_HOURS needs MAX_SPREAD_BPS or EXTENDED_HOURS_MAX_SPREAD_BPS: " +
			"the extended session is thinner than the one the strategy was calibrated in, and an " +
			"unbounded spread turns a signal into a payment to whoever is showing the only quote")
	}
	return c, nil
}

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(strings.TrimSpace(k)); !exists {
			os.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return sc.Err()
}

// splitCSV splits a comma-separated list, dropping empty entries.
func splitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parsePeerTokens reads "agent:token,agent2:token2" into a map.
//
// An entry without a colon, or with an empty agent or token, is an error:
// the whole point of a per-peer credential is that it cannot be silently
// absent, and quietly dropping the entry would leave that agent accepting
// the shared token while the operator believes otherwise. A duplicate agent
// is ambiguous for the same reason and is also rejected.
func parsePeerTokens(v string) (map[string]string, error) {
	return parseNamedTokens("A2A_PEER_TOKENS", v)
}

// parseNamedTokens reads "name:token,name2:token2". The env name is a
// parameter so errors name the key the operator set.
func parseNamedTokens(envName, v string) (map[string]string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.Index(part, ":")
		if i <= 0 || i == len(part)-1 {
			return nil, fmt.Errorf("%s entry %q is not name:token", envName, part)
		}
		agent := strings.TrimSpace(part[:i])
		token := strings.TrimSpace(part[i+1:])
		if agent == "" || token == "" {
			return nil, fmt.Errorf("%s entry %q has an empty name or token", envName, part)
		}
		key := strings.ToLower(agent)
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("%s lists %q twice", envName, agent)
		}
		out[key] = token
	}
	return out, nil
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getInt(k string, def int) (int, error) {
	v := os.Getenv(k)
	if strings.TrimSpace(v) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid integer", k, v)
	}
	return n, nil
}

func getFloat(k string, def float64) (float64, error) {
	v := os.Getenv(k)
	if strings.TrimSpace(v) == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid number", k, v)
	}
	return f, nil
}

func getBool(k string, def bool) (bool, error) {
	v := os.Getenv(k)
	if strings.TrimSpace(v) == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a valid boolean", k, v)
	}
	return b, nil
}

// DefaultFactorWeights is used when FACTOR_WEIGHTS is unset. It sums to 1.
var DefaultFactorWeights = map[string]float64{
	"trend":      0.30,
	"momentum":   0.25,
	"volume":     0.20,
	"volatility": 0.15,
	"sentiment":  0.10,
}

// factorWeightTolerance bounds how far the configured weights may drift
// from summing to exactly 1 (floating point / hand-rounded values).
const factorWeightTolerance = 0.01

// getFactorWeights parses FACTOR_WEIGHTS ("trend=0.3,momentum=0.25,...")
// into a map, rejecting unknown factor names and weights that do not sum
// to ~1. An empty/unset value yields def unchanged.
func getFactorWeights(k string, def map[string]float64) (map[string]float64, error) {
	v := os.Getenv(k)
	if strings.TrimSpace(v) == "" {
		return def, nil
	}
	out := make(map[string]float64)
	var sum float64
	for _, pair := range strings.Split(v, ",") {
		kv := strings.TrimSpace(pair)
		if kv == "" {
			continue
		}
		name, val, ok := strings.Cut(kv, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		valStr := strings.TrimSpace(val)
		if !ok || name == "" || valStr == "" {
			return nil, fmt.Errorf("%s: expected k=v pairs like trend=0.3,momentum=0.25, got %q", k, kv)
		}
		w, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a valid number", k, valStr)
		}
		if w < 0 || w > 1 {
			return nil, fmt.Errorf("%s: weight %s=%g must be in [0,1]", k, name, w)
		}
		known := false
		for f := range def {
			if f == name {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("%s: unknown factor %q (known: trend, momentum, volatility, volume, sentiment)", k, name)
		}
		out[name] = w
		sum += w
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no k=v pairs parsed from %q", k, v)
	}
	if math.Abs(sum-1) > factorWeightTolerance {
		return nil, fmt.Errorf("%s: weights sum to %.4f, must be within %.2f of 1", k, sum, factorWeightTolerance)
	}
	return out, nil
}

// FactorMinScoreFor returns the composite floor for one asset class, falling
// back to the global floor. The class string is the picker's class name.
func (c *Config) FactorMinScoreFor(class string) float64 {
	if c == nil {
		return 0
	}
	if f, ok := c.FactorMinScoreByClass[strings.ToLower(strings.TrimSpace(class))]; ok && f > 0 {
		return f
	}
	return c.FactorMinScore
}

// ExtendedSpreadCapBps is the liquidity bar an extended-session entry has to
// clear. The session-specific cap wins when set; otherwise the general
// MAX_SPREAD_BPS applies, so an operator who configured one bar gets it in
// both sessions without having to remember to set the second.
func (c *Config) ExtendedSpreadCapBps() float64 {
	if c == nil {
		return 0
	}
	if c.ExtendedHoursMaxSpreadBps > 0 {
		return c.ExtendedHoursMaxSpreadBps
	}
	return c.MaxSpreadBps
}

// MaxQuoteAge is the staleness bound on an extended-session quote. It is a
// duration so callers compare it against Snapshot.QuoteAge directly instead
// of re-deriving units at each site.
func (c *Config) MaxQuoteAge() time.Duration {
	if c == nil || c.ExtendedHoursMaxQuoteAgeSeconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(c.ExtendedHoursMaxQuoteAgeSeconds) * time.Second
}

// validateWindowSpec checks a "HH:MM-HH:MM,HH:MM-HH:MM" spec locally so a
// typo fails at startup. strategy.ParseWindows does the real parse, but
// config cannot import strategy (strategy imports config), and letting a
// malformed spec surface only when the trading loop builds means a bad
// EXTENDED_HOURS_WINDOWS looks like a bot that stopped trading.
func validateWindowSpec(envName, spec string) error {
	if strings.TrimSpace(spec) == "" {
		return fmt.Errorf("%s: empty window spec", envName)
	}
	seen := false
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		seen = true
		startS, endS, ok := strings.Cut(part, "-")
		if !ok {
			return fmt.Errorf("%s: window %q: want HH:MM-HH:MM", envName, part)
		}
		sm, err := parseWallClock(startS)
		if err != nil {
			return fmt.Errorf("%s: window %q start: %w", envName, part, err)
		}
		em, err := parseWallClock(endS)
		if err != nil {
			return fmt.Errorf("%s: window %q end: %w", envName, part, err)
		}
		if sm >= em {
			return fmt.Errorf("%s: window %q: start must be before end", envName, part)
		}
	}
	if !seen {
		return fmt.Errorf("%s: empty window spec", envName)
	}
	return nil
}

func parseWallClock(s string) (int, error) {
	hh, mm, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, fmt.Errorf("want HH:MM, got %q", s)
	}
	h, err := strconv.Atoi(hh)
	if err != nil || h < 0 || h > 24 {
		return 0, fmt.Errorf("bad hour in %q", s)
	}
	m, err := strconv.Atoi(mm)
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("bad minute in %q", s)
	}
	total := h*60 + m
	if total > 24*60 {
		return 0, fmt.Errorf("time %q exceeds 24:00", s)
	}
	return total, nil
}
