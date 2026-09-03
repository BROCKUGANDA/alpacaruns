package main

import "testing"

// TestParseTradeArgs covers the trade subcommand's flag parsing and
// positional-argument rejection without touching config or network.
func TestParseTradeArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		want     tradeArgs
		wantCode int
	}{
		{
			name: "full manual limit order",
			args: []string{"--symbol", "tsla", "--side", "buy", "--qty", "1",
				"--limit", "250.50", "--tif", "day", "--extended-hours", "--env", ".env.local"},
			want: tradeArgs{envFile: ".env.local", symbol: "tsla", side: "buy",
				qty: "1", limit: "250.50", tif: "day", extHours: true},
			wantCode: 0,
		},
		{
			name:     "notional instead of qty",
			args:     []string{"--symbol", "AAPL", "--side", "sell", "--notional", "1500"},
			want:     tradeArgs{envFile: ".env", symbol: "AAPL", side: "sell", notional: "1500"},
			wantCode: 0,
		},
		{
			name:     "unknown flag rejected by flag package",
			args:     []string{"--symbol", "AAPL", "--bogus"},
			wantCode: 2,
		},
		{
			name:     "positional argument rejected",
			args:     []string{"AAPL"},
			wantCode: 2,
		},
		{
			name:     "missing everything still parses (validated later)",
			args:     nil,
			want:     tradeArgs{envFile: ".env"},
			wantCode: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, code := parseTradeArgs(tt.args)
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

func TestParseSymbols(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", []string{"AAPL", "MSFT", "NVDA"}},
		{"  ", []string{"AAPL", "MSFT", "NVDA"}},
		{"aapl,msft", []string{"AAPL", "MSFT"}},
		{"TSLA , NVDA ,", []string{"TSLA", "NVDA"}},
	}
	for _, tt := range tests {
		got := parseSymbols(tt.in)
		if len(got) != len(tt.want) {
			t.Fatalf("parseSymbols(%q) = %v, want %v", tt.in, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("parseSymbols(%q) = %v, want %v", tt.in, got, tt.want)
			}
		}
	}
}

func TestCycleMessage(t *testing.T) {
	got := cycleMessage([]string{"AAPL", "MSFT"})
	want := "Run one full trading cycle for AAPL, MSFT."
	if got != want {
		t.Fatalf("cycleMessage = %q, want %q", got, want)
	}
}
