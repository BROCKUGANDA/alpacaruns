package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/factors"
)

// TestParseFactorsArgs covers `factors` / `factors explain SYM` argument
// parsing without touching config or network.
func TestParseFactorsArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantEnv string
		wantSym string // "" => bare listing
		wantErr bool
	}{
		{"bare listing", []string{}, ".env", "", false},
		{"bare listing with env", []string{"--env", ".env.local"}, ".env.local", "", false},
		{"explain", []string{"explain", "AAPL"}, ".env", "AAPL", false},
		{"explain lowercased symbol uppered", []string{"explain", "msft"}, ".env", "MSFT", false},
		{"explain with env flag after", []string{"explain", "NVDA", "--env", "x.env"}, "x.env", "NVDA", false},
		{"explain missing symbol", []string{"explain"}, ".env", "", true},
		{"unknown subcommand", []string{"score", "AAPL"}, ".env", "", true},
		{"extra positional", []string{"explain", "AAPL", "extra"}, ".env", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, code := parseFactorsArgs(tt.args)
			if tt.wantErr {
				if code == 0 {
					t.Fatalf("want error, got code 0 (p=%+v)", p)
				}
				return
			}
			if code != 0 {
				t.Fatalf("code = %d, want 0", code)
			}
			if p.envFile != tt.wantEnv {
				t.Errorf("envFile = %q, want %q", p.envFile, tt.wantEnv)
			}
			if p.symbol != tt.wantSym {
				t.Errorf("symbol = %q, want %q", p.symbol, tt.wantSym)
			}
			if p.explain != (tt.wantSym != "") {
				t.Errorf("explain = %t, want %t", p.explain, tt.wantSym != "")
			}
		})
	}
}

// TestCmdFactorsExplainRejectsBadExp checks --exp validation fails before
// any network access.
func TestParseFactorsArgsBadExp(t *testing.T) {
	if _, code := parseFactorsArgs([]string{"--exp", "2026-13-99"}); code == 0 {
		t.Fatal("invalid --exp accepted")
	}
}

func testFactorResult() factors.Result {
	return factors.Result{
		Composite: 0.712,
		Passed:    true,
		Factors: map[string]factors.FactorResult{
			"trend":      {Score: 0.9, Rationale: "close 250.10 above SMA20 245.00 and SMA50 238.50"},
			"momentum":   {Score: 0.6, Rationale: "10-day return +4.2%"},
			"volatility": {Score: 0.5, Rationale: "20-day return stdev 1.8%"},
			"volume":     {Score: 0.8, Rationale: "latest volume 1.6x trailing avg"},
			"sentiment":  {Score: 0.5, Rationale: "12 recent headlines; neutral proxy"},
		},
	}
}

func defaultWeights() map[string]float64 {
	return map[string]float64{
		"trend": 0.30, "momentum": 0.25, "volatility": 0.15,
		"volume": 0.20, "sentiment": 0.10,
	}
}

// TestPrintFactorExplain verifies the table layout and PASS/FAIL verdict
// with an injected engine result — no network, no config.
func TestPrintFactorExplain(t *testing.T) {
	var b bytes.Buffer
	printFactorExplain(&b, "AAPL", testFactorResult(), defaultWeights(), 0.6)
	out := b.String()
	for _, want := range []string{
		"factor explain AAPL",
		"FACTOR", "SCORE", "WEIGHT", "CONTRIB", "RATIONALE",
		"trend",
		"0.900", "0.300", "+0.270", // contribution = score x weight
		"SMA20 245.00",
		"composite 0.712 vs FACTOR_MIN_SCORE 0.600 (PASS)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s---", want, out)
		}
	}
}

// TestPrintFactorExplainFail covers the FAIL verdict path.
func TestPrintFactorExplainFail(t *testing.T) {
	res := factors.Result{Composite: 0.41, Factors: map[string]factors.FactorResult{
		"trend": {Score: 0.41, Rationale: "below both MAs"},
	}}
	var b bytes.Buffer
	printFactorExplain(&b, "TSLA", res, defaultWeights(), 0.6)
	if !strings.Contains(b.String(), "(FAIL)") {
		t.Errorf("expected FAIL verdict, got:\n%s", b.String())
	}
}

// TestPrintFactorConfig verifies the bare `factors` listing output.
func TestPrintFactorConfig(t *testing.T) {
	var b bytes.Buffer
	printFactorConfig(&b, config.DefaultFactorWeights, 0.6)
	out := b.String()
	if !strings.Contains(out, "FACTOR_MIN_SCORE = 0.600") {
		t.Errorf("missing min score line:\n%s", out)
	}
	if strings.Count(out, "\n") < len(config.DefaultFactorWeights)+2 {
		t.Errorf("expected one row per factor plus header/min lines:\n%s", out)
	}
}
