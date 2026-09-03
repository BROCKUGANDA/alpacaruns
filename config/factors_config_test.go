package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setFactorEnv writes FACTOR_* lines into a fresh .env and clears any
// exported leftovers so each case starts clean.
func setFactorEnv(t *testing.T, line string) string {
	t.Helper()
	for _, k := range []string{"FACTOR_WEIGHTS", "FACTOR_MIN_SCORE"} {
		os.Unsetenv(k)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	content := "ALPACA_API_KEY_ID=k\nALPACA_SECRET_KEY=s\n"
	if line != "" {
		content += line + "\n"
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFactorWeightsDefaults(t *testing.T) {
	c, err := Load(setFactorEnv(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.FactorWeights) != 5 {
		t.Fatalf("default weights = %v, want 5 factors", c.FactorWeights)
	}
	var sum float64
	for _, w := range c.FactorWeights {
		sum += w
	}
	if sum < 0.999 || sum > 1.001 {
		t.Fatalf("default weights sum to %f, want ~1", sum)
	}
	if c.FactorMinScore != 0.6 {
		t.Fatalf("default FactorMinScore = %v, want 0.6", c.FactorMinScore)
	}
}

func TestFactorWeightsParsingTable(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantErr string // empty = expect success
		check   func(*testing.T, map[string]float64)
	}{
		{
			name: "valid full set",
			line: "FACTOR_WEIGHTS=trend=0.3,momentum=0.25,volume=0.2,volatility=0.15,sentiment=0.1",
			check: func(t *testing.T, w map[string]float64) {
				if w["trend"] != 0.3 || w["momentum"] != 0.25 || w["sentiment"] != 0.1 {
					t.Fatalf("parsed weights wrong: %v", w)
				}
			},
		},
		{
			name: "partial set summing to one",
			line: "FACTOR_WEIGHTS=trend=1.0,momentum=0,volume=0,volatility=0,sentiment=0",
			check: func(t *testing.T, w map[string]float64) {
				if w["trend"] != 1.0 || w["momentum"] != 0 {
					t.Fatalf("parsed weights wrong: %v", w)
				}
			},
		},
		{
			name:    "unknown key rejected",
			line:    "FACTOR_WEIGHTS=trend=0.5,magic=0.5",
			wantErr: "unknown factor",
		},
		{
			name:    "sum far from one rejected",
			line:    "FACTOR_WEIGHTS=trend=0.3,momentum=0.2",
			wantErr: "must be within",
		},
		{
			name:    "negative weight rejected",
			line:    "FACTOR_WEIGHTS=trend=1.3,momentum=-0.3",
			wantErr: "must be in [0,1]",
		},
		{
			name:    "weight over one rejected",
			line:    "FACTOR_WEIGHTS=trend=1.5,momentum=-0.5",
			wantErr: "must be in [0,1]",
		},
		{
			name:    "malformed pair rejected",
			line:    "FACTOR_WEIGHTS=trend0.5,momentum=0.5",
			wantErr: "expected k=v pairs",
		},
		{
			name:    "non-numeric weight rejected",
			line:    "FACTOR_WEIGHTS=trend=zero,momentum=1",
			wantErr: "not a valid number",
		},
		{
			name: "whitespace tolerated and case normalized",
			line: "FACTOR_WEIGHTS=Trend = 0.5 , MOMENTUM=0.5",
			check: func(t *testing.T, w map[string]float64) {
				if w["trend"] != 0.5 || w["momentum"] != 0.5 {
					t.Fatalf("normalized weights wrong: %v", w)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(setFactorEnv(t, tt.line))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got none (weights=%v)", tt.wantErr, c.FactorWeights)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q missing %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.check != nil {
				tt.check(t, c.FactorWeights)
			}
		})
	}
}

func TestFactorMinScoreValidation(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    float64
		wantErr string
	}{
		{"in range", "FACTOR_MIN_SCORE=0.75", 0.75, ""},
		{"boundary zero", "FACTOR_MIN_SCORE=0", 0, ""},
		{"boundary one", "FACTOR_MIN_SCORE=1", 1, ""},
		{"over one", "FACTOR_MIN_SCORE=1.1", 0, "in [0,1]"},
		{"negative", "FACTOR_MIN_SCORE=-0.5", 0, "in [0,1]"},
		{"garbage", "FACTOR_MIN_SCORE=high", 0, "not a valid number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(setFactorEnv(t, tt.line))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.FactorMinScore != tt.want {
				t.Fatalf("FactorMinScore = %v, want %v", c.FactorMinScore, tt.want)
			}
		})
	}
}
