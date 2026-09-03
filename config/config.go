// Package config loads Alpacaruns settings from environment / .env.
package config

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
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
	AlpacaKeyID         string
	AlpacaSecret        string
	AlpacaBaseURL       string // trading API; paper by default
	AlpacaDataURL       string // market data API
	GeminiAPIKey        string
	Mode                Mode
	PollSeconds         int     // monitoring loop interval
	MinConfidence       float64 // autonomous mode gate
	MaxPositionUSD      float64 // per-trade cap for equities
	CryptoMaxPositionUSD float64 // per-trade cap for crypto notional (BTC/ETH need bigger)
	MaxPortfolioPct     float64 // max % of portfolio in one position

	ExtendedHours   bool    // default extended_hours=true on agent orders
	PreOrders       bool    // queue resting limit/GTC bracket pre-orders while market closed
	StreamFeed      string  // market-data stream feed: iex | sip
	TradeLog        string  // path to the JSONL trade log

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
	// PRE_ORDERS queues equity entries as resting limit/GTC brackets while
	// the market is closed (overnight, weekend): sized at the last daily
	// close instead of a live snapshot. Default false — off-hours behavior
	// only changes when explicitly enabled.
	if po, err := getBool("PRE_ORDERS", false); err != nil {
		return nil, err
	} else {
		c.PreOrders = po
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
