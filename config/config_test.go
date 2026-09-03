package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromEnvFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	os.WriteFile(p, []byte(
		"ALPACA_API_KEY_ID=testkey\n"+
			"ALPACA_SECRET_KEY=testsecret\n"+
			"MODE=autonomous\n"+
			"MIN_CONFIDENCE=0.8\n"+
			"MAX_POSITION_USD=5000\n"), 0644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.AlpacaKeyID != "testkey" || c.Mode != ModeAutonomous {
		t.Fatalf("unexpected config: %+v", c)
	}
	if c.MinConfidence != 0.8 || c.MaxPositionUSD != 5000 {
		t.Fatalf("numeric parse failed: %+v", c)
	}
	if c.AlpacaBaseURL != "https://paper-api.alpaca.markets/v2" {
		t.Fatalf("paper default not applied: %s", c.AlpacaBaseURL)
	}
}

func TestLoadMissingKeys(t *testing.T) {
	os.Unsetenv("ALPACA_API_KEY_ID")
	os.Unsetenv("ALPACA_SECRET_KEY")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error when keys missing")
	}
}

func TestLoadInvalidMode(t *testing.T) {
	os.Unsetenv("MODE") // earlier subtests may have exported it
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	os.WriteFile(p, []byte("ALPACA_API_KEY_ID=k\nALPACA_SECRET_KEY=s\nMODE=live\n"), 0644)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestLiveTradingRequiresAcknowledgement(t *testing.T) {
	os.Unsetenv("MODE")
	t.Setenv("ALPACA_API_KEY_ID", "k")
	t.Setenv("ALPACA_SECRET_KEY", "s")
	t.Setenv("I_ALPACA_LIVE", "")
	dir := t.TempDir()

	// Paper default: loads fine.
	p := filepath.Join(dir, "paper.env")
	os.WriteFile(p, []byte("ALPACA_BASE_URL=https://paper-api.alpaca.markets/v2\n"), 0644)
	if _, err := Load(p); err != nil {
		t.Fatalf("paper URL should load: %v", err)
	}
	// Live URL without acknowledgement: refused. Note: loadEnvFile exports
	// .env keys into the process env, so drop what the paper Load leaked.
	os.Unsetenv("ALPACA_BASE_URL")
	live := filepath.Join(dir, "live.env")
	os.WriteFile(live, []byte("ALPACA_BASE_URL=https://api.alpaca.markets/v2\n"), 0644)
	if _, err := Load(live); err == nil {
		t.Fatal("live URL without I_ALPACA_LIVE=YES must be refused")
	}

	// Live URL with acknowledgement: allowed.
	t.Setenv("I_ALPACA_LIVE", "YES")
	if _, err := Load(live); err != nil {
		t.Fatalf("live URL with I_ALPACA_LIVE=YES should load: %v", err)
	}
}

func TestConfigRangeValidation(t *testing.T) {
	os.Unsetenv("MODE")
	t.Setenv("ALPACA_API_KEY_ID", "k")
	t.Setenv("ALPACA_SECRET_KEY", "s")

	tests := []struct {
		name    string
		line    string
		wantErr string
	}{
		{"malformed int", "POLL_SECONDS=abc\n", "not a valid integer"},
		{"zero poll", "POLL_SECONDS=0\n", "must be > 0"},
		{"negative poll", "POLL_SECONDS=-5\n", "must be > 0"},
		{"letter-O number", "MAX_POSITION_USD=10O00\n", "not a valid number"},
		{"comma decimal", "MIN_CONFIDENCE=0,7\n", "not a valid number"},
		{"negative position usd", "MAX_POSITION_USD=-5\n", "must be > 0"},
		{"pct over one", "MAX_PORTFOLIO_PCT=5\n", "in (0,1]"},
		{"zero pct", "MAX_PORTFOLIO_PCT=0\n", "in (0,1]"},
		{"confidence over one", "MIN_CONFIDENCE=1.5\n", "in [0,1]"},
		{"negative confidence", "MIN_CONFIDENCE=-0.1\n", "in [0,1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, ".env")
			os.WriteFile(p, []byte(tt.line), 0644)
			// loadEnvFile only sets keys not already exported; clear all
			// knobs so each case starts clean.
			for _, k := range []string{"POLL_SECONDS", "MIN_CONFIDENCE", "MAX_POSITION_USD", "MAX_PORTFOLIO_PCT"} {
				os.Unsetenv(k)
			}
			_, err := Load(p)
			if err == nil {
				t.Fatalf("expected error containing %q for line %q", tt.wantErr, tt.line)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}

	// Boundary values must be accepted.
	t.Run("boundaries accepted", func(t *testing.T) {
		for _, k := range []string{"POLL_SECONDS", "MIN_CONFIDENCE", "MAX_POSITION_USD", "MAX_PORTFOLIO_PCT", "ALPACA_BASE_URL"} {
			os.Unsetenv(k)
		}
		dir := t.TempDir()
		p := filepath.Join(dir, ".env")
		os.WriteFile(p, []byte(
			"POLL_SECONDS=1\n"+
				"MIN_CONFIDENCE=1\n"+
				"MAX_POSITION_USD=0.01\n"+
				"MAX_PORTFOLIO_PCT=1\n"), 0644)
		c, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if c.PollSeconds != 1 || c.MinConfidence != 1 || c.MaxPositionUSD != 0.01 || c.MaxPortfolioPct != 1 {
			t.Fatalf("unexpected config: %+v", c)
		}
	})
}
