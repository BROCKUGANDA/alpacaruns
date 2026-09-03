package strategy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseWindows(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		wantErr bool
		want    []Window
	}{
		{"single", "09:35-10:15", false, []Window{{575, 615}}},
		{"two", "09:35-10:15,15:45-16:00", false, []Window{{575, 615}, {945, 960}}},
		{"whitespace", " 09:35 - 10:15 , 15:45-16:00 ", false, []Window{{575, 615}, {945, 960}}},
		{"full day", "00:00-24:00", false, []Window{{0, 1440}}},
		{"start after end", "10:15-09:35", true, nil},
		{"equal times", "09:35-09:35", true, nil},
		{"bad hour", "25:00-26:00", true, nil},
		{"no dash", "0935", true, nil},
		{"empty", "", true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseWindows(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d windows, want %d (%+v)", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("window[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestInWindowsBoundaries(t *testing.T) {
	ws := []Window{{StartMinute: 575, EndMinute: 615}} // 09:35-10:15
	tests := []struct {
		minute int
		want   bool
	}{
		{574, false}, // 09:34 just before
		{575, true},  // 09:35 start inclusive
		{600, true},  // inside
		{615, true},  // end inclusive
		{616, false}, // just after
		{0, false},
		{1439, false},
	}
	for _, tt := range tests {
		if got := InWindows(tt.minute, ws); got != tt.want {
			t.Fatalf("minute %d: got %v, want %v", tt.minute, got, tt.want)
		}
	}
}

// ET handling with an injectable clock: fixed EST (-05:00).
func TestTradingWindowsCanTrade(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	mk := func(h, m int) func() time.Time {
		return func() time.Time { return time.Date(2026, 8, 25, h, m, 0, 0, time.UTC) } // UTC in, ET out via In()
	}
	tw, err := NewTradingWindows("09:35-10:15", "00:00-23:59", loc, mk(14, 30))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		hh, mm int // UTC clock returned by fake now
		sym    string
		want   bool
	}{
		{"equity inside morning window", 14, 40, "AAPL", true}, // 09:40 ET
		{"equity before window", 14, 30, "AAPL", false},        // 09:30 ET
		{"equity at start boundary", 14, 35, "AAPL", true},     // 09:35 ET
		{"equity at end boundary", 15, 15, "AAPL", true},       // 10:15 ET inclusive
		{"equity after window", 15, 16, "AAPL", false},         // 10:16 ET
		{"crypto always on", 3, 0, "BTC/USD", true},            // 22:00 prev day ET still within 00:00-23:59
		{"option follows equity windows", 14, 40, "AAPL260918C00100000", true},
		{"option outside equity hours", 20, 0, "AAPL260918C00100000", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tw.now = mk(tt.hh, tt.mm)
			if got := tw.CanTrade(tt.sym); got != tt.want {
				t.Fatalf("CanTrade(%s) = %v, want %v", tt.sym, got, tt.want)
			}
		})
	}
}

// ---- settings loader ----

func TestLoadSettingsValidation(t *testing.T) {
	base := map[string]string{
		"STRATEGY_EQUITY_SYMBOLS": "aapl, msft",
		"POSITION_PCT":            "0.05",
		"TP_PCT":                  "0.08",
		"SL_PCT":                  "0.04",
	}
	getenv := func(k string) string { return base[k] }

	set, err := LoadSettings(getenv, 60)
	if err != nil {
		t.Fatal(err)
	}
	if set.EquitySymbols[0] != "AAPL" || set.EquitySymbols[1] != "MSFT" {
		t.Fatalf("symbol normalization failed: %v", set.EquitySymbols)
	}
	if set.CryptoSymbols[0] != "BTC/USD" || set.CryptoSymbols[1] != "ETH/USD" {
		t.Fatalf("crypto defaults wrong: %v", set.CryptoSymbols)
	}
	if set.DailyDD != 0.05 || set.WeeklyDD != 0.10 || set.TotalDD != 0.15 {
		t.Fatalf("drawdown defaults wrong: %v/%v/%v", set.DailyDD, set.WeeklyDD, set.TotalDD)
	}

	bad := map[string][]string{
		"TP <= SL":       {"TP_PCT=0.04", "SL_PCT=0.04"},
		"negative SL":    {"SL_PCT=-0.01"},
		"pct over one":   {"POSITION_PCT=1.5"},
		"delta inverted": {"OPTION_DELTA_MIN=0.8", "OPTION_DELTA_MAX=0.7"},
		"DTE inverted":   {"OPTION_MIN_DTE=50", "OPTION_MAX_DTE=45"},
		"dd over one":    {"DAILY_DD_HALT=2"},
	}
	for name, kvs := range bad {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			for _, kv := range kvs {
				k, v, _ := strings.Cut(kv, "=")
				env[k] = v
			}
			if _, err := LoadSettings(func(k string) string { return env[k] }, 60); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

func TestStateStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStateStore(filepath.Join(dir, "trades.jsonl"))

	lv := PositionLevels{
		Symbol: "BTC/USD", EntryPrice: 50000, TakeProfit: 54000,
		StopLoss: 48000, Qty: "0.1", Since: time.Now().UTC(),
	}
	if err := st.SetLevel(lv); err != nil {
		t.Fatal(err)
	}
	// Crypto flag auto-derived.
	got, err := st.CryptoLevels()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Symbol != "BTC/USD" || !got[0].Crypto {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got[0].TakeProfit != 54000 || got[0].StopLoss != 48000 {
		t.Fatalf("levels corrupted: %+v", got[0])
	}

	// Equity levels must NOT appear in crypto enforcement list.
	if err := st.SetLevel(PositionLevels{
		Symbol: "AAPL", EntryPrice: 100, TakeProfit: 108, StopLoss: 96,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.CryptoLevels()
	if len(got) != 1 {
		t.Fatalf("equity leaked into crypto levels: %+v", got)
	}

	// Persistence survives a fresh store instance (restart simulation).
	reopened := NewStateStore(filepath.Join(dir, "trades.jsonl"))
	s2, err := reopened.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Levels) != 2 {
		t.Fatalf("expected 2 persisted levels, got %d", len(s2.Levels))
	}

	if err := reopened.ClearLevel("BTC/USD"); err != nil {
		t.Fatal(err)
	}
	s3, _ := reopened.Load()
	if _, exists := s3.Levels["BTC/USD"]; exists {
		t.Fatal("ClearLevel did not persist")
	}
	if _, exists := s3.Levels["AAPL"]; !exists {
		t.Fatal("ClearLevel removed unrelated entry")
	}
}

// ---- IsCrypto ----

func TestIsCrypto(t *testing.T) {
	for sym, want := range map[string]bool{
		"BTC/USD": true, "eth/usd": true, "SOL/USD": true,
		"AAPL": false, "MSFT": false, "AAPL260918C00100000": false,
	} {
		if got := IsCrypto(sym); got != want {
			t.Fatalf("IsCrypto(%q) = %v, want %v", sym, got, want)
		}
	}
}

var _ = os.Getenv // keep os import if future assertions need it
