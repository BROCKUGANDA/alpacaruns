package ensemble

// settings.go: ENSEMBLE_* environment knobs, parsed in the same injectable
// getenv style as strategy/settings.go. Production passes os.Getenv after
// config.Load has materialized the .env file.

import (
	"fmt"
	"strings"
)

const (
	defMinConfidence  = 0.55
	defRiskPct        = 0.01
	defMaxCorr        = 0.85
	defLiquidityPct   = 0.01
	defBenchmark      = "SPY"
	defPairs          = "SPY/QQQ,XLE/XLF"
	defPerfWindow     = 30
	defPerfPathSuffix = "ensemble-state.json"
)

// Config holds every ENSEMBLE_* knob.
type Config struct {
	Enabled bool

	MinConfidence float64 // MIN_ENSEMBLE_CONFIDENCE: winning-mass floor
	RiskPct       float64 // RISK_PCT_PER_TRADE: portfolio fraction per 2-ATR position
	MaxCorr       float64 // ENSEMBLE_MAX_CORRELATION: avg-corr ceiling to held positions
	LiquidityPct  float64 // ENSEMBLE_LIQUIDITY_PCT: notional cap vs avg dollar volume
	Benchmark     string  // ENSEMBLE_BENCHMARK: vol-regime reference symbol
	Pairs         []Pair  // ENSEMBLE_PAIRS: A/B pairs (slash-separated legs)
	PerfWindow    int     // ENSEMBLE_PERF_WINDOW: trailing resolved signals per expert
}

// LoadConfig parses every ENSEMBLE_* variable. getenv is injectable for
// tests; production passes strategy.OsGetenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		Enabled:   envBool(getenv, "ENSEMBLE_ENABLED", false),
		Benchmark: firstNonEmptyStr(getenv("ENSEMBLE_BENCHMARK"), defBenchmark),
	}
	var err error
	if c.PerfWindow, err = envInt(getenv, "ENSEMBLE_PERF_WINDOW", defPerfWindow); err != nil {
		return c, err
	}
	if c.MinConfidence, err = envFloat(getenv, "MIN_ENSEMBLE_CONFIDENCE", defMinConfidence); err != nil {
		return c, err
	}
	if c.RiskPct, err = envFloat(getenv, "RISK_PCT_PER_TRADE", defRiskPct); err != nil {
		return c, err
	}
	if c.MaxCorr, err = envFloat(getenv, "ENSEMBLE_MAX_CORRELATION", defMaxCorr); err != nil {
		return c, err
	}
	if c.LiquidityPct, err = envFloat(getenv, "ENSEMBLE_LIQUIDITY_PCT", defLiquidityPct); err != nil {
		return c, err
	}

	pairsRaw := firstNonEmptyStr(getenv("ENSEMBLE_PAIRS"), defPairs)
	for _, spec := range strings.Split(pairsRaw, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		legs := strings.Split(spec, "/")
		if len(legs) != 2 {
			return c, fmt.Errorf("ENSEMBLE_PAIRS entry %q must be LEG_A/LEG_B", spec)
		}
		c.Pairs = append(c.Pairs, Pair{A: strings.ToUpper(strings.TrimSpace(legs[0])),
			B: strings.ToUpper(strings.TrimSpace(legs[1]))})
	}
	if len(c.Pairs) == 0 {
		return c, fmt.Errorf("ENSEMBLE_PAIRS parsed empty")
	}

	for name, v := range map[string]float64{
		"MIN_ENSEMBLE_CONFIDENCE":  c.MinConfidence,
		"RISK_PCT_PER_TRADE":       c.RiskPct,
		"ENSEMBLE_MAX_CORRELATION": c.MaxCorr,
	} {
		if v <= 0 || v > 1 {
			return c, fmt.Errorf("%s must be in (0,1], got %g", name, v)
		}
	}
	if c.LiquidityPct <= 0 || c.LiquidityPct > 1 {
		return c, fmt.Errorf("ENSEMBLE_LIQUIDITY_PCT must be in (0,1], got %g", c.LiquidityPct)
	}
	if c.PerfWindow <= 0 {
		return c, fmt.Errorf("ENSEMBLE_PERF_WINDOW must be >= 1, got %d", c.PerfWindow)
	}
	return c, nil
}

// ---- small helpers mirroring strategy/settings.go ----

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func envFloat(getenv func(string) string, key string, def float64) (float64, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%g", &f); err != nil {
		return def, fmt.Errorf("%s=%q: %w", key, raw, err)
	}
	return f, nil
}

func envInt(getenv func(string) string, key string, def int) (int, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &n); err != nil {
		return def, fmt.Errorf("%s=%q: %w", key, raw, err)
	}
	return n, nil
}

func envBool(getenv func(string) string, key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
