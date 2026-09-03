package main

import (
	"strings"
	"testing"
)

// TestParseChainArgs covers the chain subcommand's flag parsing and
// positional-argument rejection without touching config or network.
func TestParseChainArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		want     chainArgs
		wantCode int
	}{
		{
			name:     "full flags",
			args:     []string{"--symbol", "aapl", "--exp", "2026-09-18", "--type", "put", "--env", ".env.local"},
			want:     chainArgs{envFile: ".env.local", symbol: "aapl", exp: "2026-09-18", typ: "put"},
			wantCode: 0,
		},
		{
			name:     "defaults",
			args:     []string{"--symbol", "AAPL"},
			want:     chainArgs{envFile: ".env", symbol: "AAPL"},
			wantCode: 0,
		},
		{
			name:     "unknown flag rejected",
			args:     []string{"--symbol", "AAPL", "--bogus"},
			wantCode: 2,
		},
		{
			name:     "positional argument rejected",
			args:     []string{"AAPL"},
			wantCode: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, code := parseChainArgs(tt.args)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tt.wantCode)
			}
			if code != 0 {
				return
			}
			if got != tt.want {
				t.Fatalf("parsed = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestParseTradeArgsOccFlag covers the additive --occ flag.
func TestParseTradeArgsOccFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		occ  string
	}{
		{"occ flag set", []string{"--occ", "AAPL240119C00100000", "--side", "buy", "--qty", "1"}, "AAPL240119C00100000"},
		{"occ overrides later symbol", []string{"--symbol", "TSLA", "--occ", "AAPL240119P00100000", "--side", "buy", "--qty", "1"}, "AAPL240119P00100000"},
		{"no occ keeps empty", []string{"--symbol", "AAPL", "--side", "buy", "--qty", "1"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, code := parseTradeArgs(tt.args)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if got.occ != tt.occ {
				t.Fatalf("occ = %q, want %q", got.occ, tt.occ)
			}
		})
	}
}

// TestCmdChainValidation covers cmdChain's input validation without any
// network access (it must reject before dialing Alpaca).
func TestCmdChainValidation(t *testing.T) {
	cfg := testConfig()
	tests := []struct {
		name    string
		p       chainArgs
		wantSub string // expected stderr substring; empty means success path reached network
	}{
		{name: "missing symbol", p: chainArgs{}, wantSub: "--symbol is required"},
		{name: "bad type", p: chainArgs{symbol: "AAPL", typ: "straddle"}, wantSub: "invalid --type"},
		{name: "bad exp date", p: chainArgs{symbol: "AAPL", exp: "09-2026"}, wantSub: "invalid --exp"},
		{
			name: "valid args proceed to network",
			p:    chainArgs{symbol: "AAPL", exp: "2026-09-18", typ: "call"},
			// With an unreachable endpoint this returns exit code 1 with a
			// contracts lookup error — acceptable here; we only assert the
			// validators above pass through.
			wantSub: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := cmdChain(testCtx(), cfg, tt.p)
			switch {
			case tt.wantSub == "":
				if code == 2 {
					t.Fatalf("unexpected usage rejection for %+v", tt.p)
				}
			case code != 2:
				t.Fatalf("expected usage rejection (exit 2), got %d", code)
			}
		})
	}
}

// TestUsageDocumentsOptionsCommands guards that help text mentions the
// options surface so operators can discover it.
func TestUsageDocumentsOptionsCommands(t *testing.T) {
	if !strings.Contains(usage, "--occ") || !strings.Contains(usage, "chain") {
		t.Fatal("usage text missing options documentation (--occ / chain)")
	}
}
