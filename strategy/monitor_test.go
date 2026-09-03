package strategy

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// ---- broker fake: records orders, serves positions/orders ----

type orderCall struct {
	Symbol, Side, Type string
	Qty                string
}

type fakeBroker struct {
	positions []tools.Position
	openStops map[string]bool // symbol -> protective stop exists
	placed    []orderCall
}

func (f *fakeBroker) Positions(ctx context.Context) ([]tools.Position, error) {
	return f.positions, nil
}

func (f *fakeBroker) OpenOrders(ctx context.Context) ([]tools.Order, error) {
	var out []tools.Order
	for sym := range f.openStops {
		out = append(out, tools.Order{Symbol: sym, Type: "stop"})
	}
	return out, nil
}

func (f *fakeBroker) Place(ctx context.Context, req tools.OrderRequest) (*tools.Order, error) {
	f.placed = append(f.placed, orderCall{Symbol: req.Symbol, Side: req.Side, Type: req.Type, Qty: req.Qty})
	return &tools.Order{ID: "fake-" + req.Symbol, Symbol: req.Symbol}, nil
}

func mkMonitor(t *testing.T, dir string, kill *agents.KillSwitch, set Settings) (*PositionMonitor, *fakeBroker) {
	t.Helper()
	jPath := filepath.Join(dir, "trades.jsonl")
	j, err := pnl.Open(jPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	fb := &fakeBroker{openStops: map[string]bool{}}
	m := &PositionMonitor{
		Kill: kill, State: NewStateStore(jPath), Journal: j,
		Settings: set, Log: log.New(os.Stderr, "", 0), Broker: fb,
	}
	return m, fb
}

func defSettings() Settings {
	set, err := LoadSettings(func(string) string { return "" }, 60)
	if err != nil {
		panic(err)
	}
	return set
}

// ---- drawdown halts at exactly each threshold ----

// Drawdown math is exercised through checkDrawdown via exported Tick with
// an empty price map; journal fills drive daily/weekly realized losses.
func TestDrawdownHaltExactThresholds(t *testing.T) {
	tests := []struct {
		name      string
		equity    float64
		peak      float64
		dailyDD   float64
		wantHalt  bool
		wantCount int
	}{
		{"total dd just under halt", 95100, 100000, 0.15, false, 0},
		{"total dd exactly at halt", 85000, 100000, 0.15, true, 1},
		{"total dd beyond halt", 80000, 100000, 0.15, true, 1},
		{"healthy equity", 110000, 100000, 0.15, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			kill := agents.NewKillSwitch()
			set := defSettings()
			set.DailyDD = tt.dailyDD
			m, _ := mkMonitor(t, dir, kill, set)

			st, _ := m.State.Load()
			st.PeakEquity = tt.peak
			if err := m.State.Save(st); err != nil {
				t.Fatal(err)
			}

			res, err := m.Tick(context.Background(), map[string]float64{}, tt.equity)
			if err != nil {
				t.Fatal(err)
			}
			if res.HaltEngaged != tt.wantHalt {
				t.Fatalf("halt = %v (%v), want %v", res.HaltEngaged, res.HaltReasons, tt.wantHalt)
			}
			if len(res.HaltReasons) < tt.wantCount && tt.wantHalt {
				t.Fatalf("expected >= %d reasons, got %v", tt.wantCount, res.HaltReasons)
			}
			if kill.Halted() != tt.wantHalt {
				t.Fatalf("kill switch = %v, want %v", kill.Halted(), tt.wantHalt)
			}
		})
	}
}

// Daily realized loss reaching the halt fraction triggers the switch.
func TestDailyRealizedLossTriggersHalt(t *testing.T) {
	dir := t.TempDir()
	kill := agents.NewKillSwitch()
	set := defSettings() // DAILY_DD_HALT=0.05
	m, _ := mkMonitor(t, dir, kill, set)

	// Peak equity 100k; a 5% realized loss today (5000) must trip it.
	st, _ := m.State.Load()
	st.PeakEquity = 100000
	_ = m.State.Save(st)
	now := time.Now().UTC()
	// Round trip: buy 100 @ 100, sell 100 @ 50 => realized -5000
	// (exactly DAILY_DD_HALT=5% of the 100k peak).
	_ = m.Journal.Append(pnl.Record{
		Kind: pnl.KindFill, TS: now, Symbol: "AAPL", Side: "buy",
		Qty: "100", Price: "100",
	})
	_ = m.Journal.Append(pnl.Record{
		Kind: pnl.KindFill, TS: now.Add(time.Minute), Symbol: "AAPL", Side: "sell",
		Qty: "100", Price: "50",
	})
	res, err := m.Tick(context.Background(), map[string]float64{}, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if !res.HaltEngaged || len(res.HaltReasons) == 0 {
		t.Fatalf("expected daily halt from 5%% realized loss, got %+v", res)
	}
	found := false
	for _, r := range res.HaltReasons {
		if len(r) > 5 && r[:5] == "daily" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a daily reason, got %v", res.HaltReasons)
	}
}

// ---- crypto TP/SL enforcement ----

func TestCryptoLevelEnforcement(t *testing.T) {
	dir := t.TempDir()
	kill := agents.NewKillSwitch()
	set := defSettings()
	m, fb := mkMonitor(t, dir, kill, set)

	lv := PositionLevels{
		Symbol: "BTC/USD", EntryPrice: 50000, TakeProfit: 54000, StopLoss: 48000,
	}
	fb.positions = []tools.Position{{Symbol: "BTC/USD", Qty: "0.1"}}
	if err := m.State.SetLevel(lv); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		price     float64
		wantClose bool
	}{
		{"between levels holds", 50000, false},
		{"at TP boundary closes", 54000, true},
		{"above TP closes", 55000, true},
		{"at SL boundary closes", 48000, true},
		{"below SL closes", 47000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Re-set the level each subtest (Tick clears on close).
			if err := m.State.SetLevel(lv); err != nil {
				t.Fatal(err)
			}
			res, err := m.Tick(context.Background(), map[string]float64{"BTC/USD": tt.price}, 100000)
			if err != nil {
				t.Fatal(err)
			}
			got := len(res.Closed) > 0
			if got != tt.wantClose {
				t.Fatalf("closed = %v (%v), want %v", got, res.Closed, tt.wantClose)
			}
			if got {
				levels, _ := m.State.CryptoLevels()
				if len(levels) != 0 {
					t.Fatalf("level should be cleared after close, still have %+v", levels)
				}
			}
		})
	}
}

var _ = fmt.Sprintf

// ---- equity bracket rebuild ----

func TestEquityBracketRebuild(t *testing.T) {
	dir := t.TempDir()
	kill := agents.NewKillSwitch()
	set := defSettings()
	m, fb := mkMonitor(t, dir, kill, set)

	lv := PositionLevels{Symbol: "AAPL", EntryPrice: 100, TakeProfit: 108, StopLoss: 96}
	if err := m.State.SetLevel(lv); err != nil {
		t.Fatal(err)
	}

	t.Run("protective stop present: no rebuild", func(t *testing.T) {
		fb.openStops["AAPL"] = true
		fb.placed = nil
		if _, err := m.Tick(context.Background(), map[string]float64{"AAPL": 100}, 100000); err != nil {
			t.Fatal(err)
		}
		for _, p := range fb.placed {
			if p.Symbol == "AAPL" && p.Type == "stop" {
				t.Fatalf("unexpected SL rebuild %+v", p)
			}
		}
	})

	t.Run("missing protective stop: rebuilt at stored level", func(t *testing.T) {
		delete(fb.openStops, "AAPL")
		fb.positions = []tools.Position{{Symbol: "AAPL", Qty: "10"}}
		fb.placed = nil
		if _, err := m.Tick(context.Background(), map[string]float64{"AAPL": 100}, 100000); err != nil {
			t.Fatal(err)
		}
		var rebuilt *orderCall
		for i := range fb.placed {
			if fb.placed[i].Symbol == "AAPL" && fb.placed[i].Type == "stop" {
				rebuilt = &fb.placed[i]
			}
		}
		if rebuilt == nil {
			t.Fatalf("expected a stop order rebuild, got %+v", fb.placed)
		}
		if rebuilt.Side != "sell" || rebuilt.Qty != "10" {
			t.Fatalf("rebuild wrong: %+v", rebuilt)
		}
	})

	t.Run("position flat: stale levels dropped", func(t *testing.T) {
		fb.positions = nil
		fb.placed = nil
		if _, err := m.Tick(context.Background(), map[string]float64{}, 100000); err != nil {
			t.Fatal(err)
		}
		st, _ := m.State.Load()
		if _, exists := st.Levels["AAPL"]; exists {
			t.Fatal("stale equity level should be cleared when position is gone")
		}
	})
}
