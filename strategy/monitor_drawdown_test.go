package strategy

// monitor_drawdown_test.go: the daily/weekly halts must measure REALISED
// P/L, not cash flow.
//
// The regression this pins is expensive: summing fill cash flow made every
// BUY look like a same-day loss (money going out, not money lost), so a bot
// deploying capital into new positions could halt itself on its best day.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
)

func drawdownMonitor(t *testing.T, peak float64) *PositionMonitor {
	t.Helper()
	dir := t.TempDir()
	set := defSettings()
	set.DailyDD = 0.05
	set.WeeklyDD = 0.10
	set.TotalDD = 0.15
	m, _ := mkMonitor(t, dir, agents.NewKillSwitch(), set)
	st, err := m.State.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.PeakEquity = peak
	if err := m.State.Save(st); err != nil {
		t.Fatalf("save state: %v", err)
	}
	return m
}

func addFill(t *testing.T, m *PositionMonitor, sym, side string, qty, price float64, at time.Time) {
	t.Helper()
	if err := m.Journal.Append(pnl.Record{
		Kind: pnl.KindFill, OrderID: sym + "-" + at.Format(time.RFC3339Nano),
		Symbol: sym, Side: side,
		Qty:   strconv.FormatFloat(qty, 'f', -1, 64),
		Price: strconv.FormatFloat(price, 'f', -1, 64),
		TS:    at,
	}); err != nil {
		t.Fatalf("append fill: %v", err)
	}
}

// Opening a $50k position on a $100k account is a 50% cash outflow and a 0%
// loss. The old math read it as -50% and halted.
func TestEntryCashFlowIsNotADrawdown(t *testing.T) {
	m := drawdownMonitor(t, 100000)
	addFill(t, m, "AAPL", "buy", 500, 100, time.Now().UTC().Add(-time.Hour))

	reasons := m.checkDrawdown(100000)
	if len(reasons) != 0 {
		t.Fatalf("opening a position was treated as a loss: %v", reasons)
	}
}

// A realised loss inside the window must be MEASURED: below the threshold
// it halts nothing, and past the threshold it fires. Both matter — the old
// cash-flow math fired on a threshold that had nothing to do with profit.
func TestRealizedLossTriggersDailyHalt(t *testing.T) {
	now := time.Now().UTC()

	small := drawdownMonitor(t, 100000)
	addFill(t, small, "AAPL", "buy", 500, 100, now.Add(-6*time.Hour))
	addFill(t, small, "AAPL", "sell", 500, 99.9, now.Add(-time.Hour)) // -$50
	if got := small.checkDrawdown(99950); len(got) != 0 {
		t.Fatalf("a $50 loss halted a $100k account: %v", got)
	}

	big := drawdownMonitor(t, 100000)
	addFill(t, big, "AAPL", "buy", 500, 100, now.Add(-6*time.Hour))
	addFill(t, big, "AAPL", "sell", 500, 90, now.Add(-time.Hour)) // -$5,000 = 5%
	reasons := big.checkDrawdown(95000)
	if len(reasons) == 0 {
		t.Fatal("a realised 5% loss did not fire the daily halt")
	}
	if !strings.HasPrefix(reasons[0], "daily") {
		t.Fatalf("expected the daily halt, got %v", reasons)
	}
}

// A profitable close must never contribute to drawdown, however large the
// cash that moved.
func TestProfitableCloseIsNotADrawdown(t *testing.T) {
	m := drawdownMonitor(t, 100000)
	now := time.Now().UTC()
	addFill(t, m, "AAPL", "buy", 500, 100, now.Add(-2*time.Hour))
	addFill(t, m, "AAPL", "sell", 500, 110, now.Add(-time.Hour))
	if reasons := m.checkDrawdown(105000); len(reasons) != 0 {
		t.Fatalf("a winning round trip read as a drawdown: %v", reasons)
	}
}

// A position opened yesterday and closed today contributes to TODAY's
// window, not yesterday's: the loss is realised when it is realised.
func TestCloseCountsOnTheDayItHappens(t *testing.T) {
	m := drawdownMonitor(t, 100000)
	now := time.Now().UTC()
	addFill(t, m, "MSFT", "buy", 1000, 50, now.AddDate(0, 0, -3)) // -$50k, 3 days ago
	// Still open: three days of "cash flow" must not have halted anything.
	if reasons := m.checkDrawdown(100000); len(reasons) != 0 {
		t.Fatalf("an open position drove a window drawdown: %v", reasons)
	}
	addFill(t, m, "MSFT", "sell", 1000, 45, now.Add(-time.Minute)) // -$5,000 today
	if reasons := m.checkDrawdown(95000); len(reasons) == 0 {
		t.Fatal("a close today did not count toward today's drawdown")
	}
}

// Shorts realise the other way round; a short covered at a loss is a loss.
func TestShortCloseRealisesCorrectly(t *testing.T) {
	now := time.Now().UTC()
	// 600 contracts at 400, covered at 410: -$6,000 = 6% of a $100k account.
	m := drawdownMonitor(t, 100000)
	addFill(t, m, "TSLA", "sell", 600, 400, now.Add(-2*time.Hour)) // open short
	addFill(t, m, "TSLA", "buy", 600, 410, now.Add(-time.Hour))    // cover at a loss
	if reasons := m.checkDrawdown(94000); len(reasons) == 0 {
		t.Fatal("a short covered at a loss did not fire the daily halt")
	}
	// And the profitable direction: no drawdown at all.
	m2 := drawdownMonitor(t, 100000)
	addFill(t, m2, "TSLA", "sell", 600, 400, now.Add(-2*time.Hour))
	addFill(t, m2, "TSLA", "buy", 600, 390, now.Add(-time.Hour)) // +$6,000
	if got := m2.checkDrawdown(106000); len(got) != 0 {
		t.Fatalf("a profitable short read as a drawdown: %v", got)
	}
}

// The total-drawdown halt still uses marked equity, including unrealised
// moves — that half was correct and must stay that way.
func TestTotalDrawdownStillUsesMarkedEquity(t *testing.T) {
	m := drawdownMonitor(t, 100000)
	// No fills at all: the loss is unrealised, and the total halt must see it.
	reasons := m.checkDrawdown(84000) // 16% below peak, halt at 15%
	if len(reasons) == 0 {
		t.Fatal("total drawdown ignored marked equity")
	}
	found := false
	for _, r := range reasons {
		if strings.HasPrefix(r, "total") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a total-drawdown reason, got %v", reasons)
	}
}
