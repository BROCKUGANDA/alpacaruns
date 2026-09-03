package ensemble

import (
	"fmt"
	"math"
	"strings"
)

// isCryptoLike reports whether the symbol is in BASE/QUOTE form (e.g.
// "BTC/USD"). The strategy package owns the canonical IsCrypto, but we
// re-implement a tiny heuristic here to avoid an import cycle. Crypto is
// special-cased below to skip the liquidity cap because Alpaca's free
// crypto feed returns thinly-traded daily bars (a few BTC/day) which
// would otherwise cap the budget far below the price of 1 whole coin.
func isCryptoLike(sym string) bool { return strings.Contains(sym, "/") }

// riskbudget.go: the last gate between gater verdicts and the existing
// executor.
//
//	1. ATR-normalized sizing: risk budget per position =
//	   portfolioValue x RiskPctPerTrade; qty = floor(budget / (2 x ATR)),
//	   then take the MIN against the legacy caps (MaxPositionUSD /
//	   PositionPct paths) so old limits still bind.
//	2. Correlation netting: block a BUY whose average 20-session-return
//	   correlation to currently-held positions > MaxCorrelation.
//	3. Liquidity cap: entry notional <= LiquidityPct of the symbol's
//	   trailing avgDollarVolume.
//
// The existing kill switch is untouched; this module only sizes and
// vetoes.

const (
	riskPctDefault      = 0.01
	atrRiskMultiple     = 2.0 // stop distance = 2 ATR; risk = qty x that
	corrWindow          = 20  // sessions of returns for correlation
	maxCorrDefault      = 0.85
	liquidityPctDefault = 0.01 // max notional as fraction of avg dollar volume
	liqVolumeWindow     = 20
)

// RiskConfig wires the risk-budget module.
type RiskConfig struct {
	RiskPctPerTrade float64 // fraction of portfolio risked per position (2-ATR stop)
	MaxPositionUSD  float64 // legacy cap (from cfg.MaxPositionUSD)
	PositionPct     float64 // legacy fixed-fractional cap
	MaxCorrelation  float64 // avg-corr ceiling to held positions (default 0.85)
	LiquidityPct    float64 // notional cap vs avg dollar volume (default 1%)
}

// DefaultRiskConfig fills unset fields with paper-safe defaults.
func DefaultRiskConfig() RiskConfig {
	return RiskConfig{RiskPctPerTrade: riskPctDefault,
		MaxCorrelation: maxCorrDefault, LiquidityPct: liquidityPctDefault}
}

// RiskBudget evaluates gater verdicts before execution.
type RiskBudget struct {
	cfg RiskConfig
}

// NewRiskBudget validates config (zero-value fields get defaults).
func NewRiskBudget(cfg RiskConfig) *RiskBudget {
	if cfg.RiskPctPerTrade <= 0 {
		cfg.RiskPctPerTrade = riskPctDefault
	}
	if cfg.MaxCorrelation <= 0 {
		cfg.MaxCorrelation = maxCorrDefault
	}
	if cfg.LiquidityPct <= 0 {
		cfg.LiquidityPct = liquidityPctDefault
	}
	return &RiskBudget{cfg: cfg}
}

// SizedVerdict is a verdict after risk checks.
type SizedVerdict struct {
	Verdict
	Qty        int
	Notional   float64
	Blocked    bool   // true when a check vetoed the action entirely
	BlockWhy   string // human-readable veto reason (journaled)
}

// Apply runs every check over one BUY/SELL verdict.
//
//	heldCloses maps held symbols to their return series (shared bars).
//	data supplies the candidate's ATR, dollar-volume history and the
//	held symbols' return series for correlation netting.
func (rb *RiskBudget) Apply(v Verdict, pfv float64, data MarketData) SizedVerdict {
	sv := SizedVerdict{Verdict: v}
	if v.Action != ActionBuy && v.Action != ActionSell {
		return sv
	}
	sd := data.SD(v.Symbol)
	if sd == nil || len(sd.Bars) == 0 || len(sd.Closes) == 0 {
		sv.Blocked = true
		sv.BlockWhy = "no market data for sizing"
		return sv
	}

	price := sd.Closes[len(sd.Closes)-1]
	if price <= 0 {
		sv.Blocked = true
		sv.BlockWhy = "non-positive mark price"
		return sv
	}
	if v.Action == ActionSell {
		// Exits are always allowed through; executor resolves quantity.
		return sv
	}
// --- liquidity cap (skipped for crypto: see isCryptoLike) ---
	maxNotional := rb.liquidityCap(sd)
	if !isCryptoLike(v.Symbol) && maxNotional <= 0 {
		sv.Blocked = true
		sv.BlockWhy = "insufficient liquidity data"
		return sv
	}
	if isCryptoLike(v.Symbol) {
		// Crypto: use the legacy cap as the only sizing constraint. The
		// liquidity cap relies on 20-day avg $volume which is unreliable
		// on the free crypto feed (a few BTC/day); the legacy MaxPositionUSD
		// cap is the binding constraint for crypto entries.
		maxNotional = math.MaxFloat64
	}
	// --- correlation netting ---
	if avg, ok := rb.avgCorrToHeld(v.Symbol, data); ok && avg > rb.cfg.MaxCorrelation {
		sv.Blocked = true
		sv.BlockWhy = fmt.Sprintf("correlation %.2f > %.2f to held positions (same-bet guard)", avg, rb.cfg.MaxCorrelation)
		return sv
	}



// --- ATR sizing ---
	// Crypto: skip ATR sizing (BTC daily volatility is huge in dollar terms
	// making 2*ATR stop risk larger than the position). Use legacy only.
	var qty, legacy int
	if isCryptoLike(v.Symbol) {
		legacy = rb.legacyQty(pfv, price)
		qty = legacy
	} else {
		qty = rb.atrQty(pfv, sd.ATR14)
		legacy = rb.legacyQty(pfv, price)
		if legacy < qty {
			qty = legacy
		}
	}
	notional := math.Min(float64(qty)*price, maxNotional)
	qty = int(notional / price)

	if qty < 1 {
		sv.Blocked = true
		if legacy < 1 {
			sv.BlockWhy = fmt.Sprintf("sized qty %d below 1 after legacy caps (POSITION_PCT=%.2f, MAX_POSITION_USD=%.2f)",
				qty, rb.cfg.PositionPct, rb.cfg.MaxPositionUSD)
		} else {
			sv.BlockWhy = "risk-sized notional below one share under liquidity cap"
		}
		return sv
	}

	sv.Qty = qty
	sv.Notional = float64(qty) * price
	return sv
}

// atrQty = floor(portfolioValue x RiskPct / (ATRMultiple x ATR)).
func (rb *RiskBudget) atrQty(pfv, atr float64) int {
	if atr <= 0 || pfv <= 0 {
		return math.MaxInt32
	} // no ATR -> let legacy caps decide alone
	budget := pfv * rb.cfg.RiskPctPerTrade
	return int(math.Floor(budget / (atrRiskMultiple * atr)))
}

// legacyQty mirrors strategy.Sizing semantics so the old caps bind:
// min(POSITION_PCT x pfv, MAX_POSITION_USD) / price, floored.
func (rb *RiskBudget) legacyQty(pfv, price float64) int {
	if pfv <= 0 || price <= 0 {
		return 0
	}
	budget := rb.cfg.PositionPct * pfv
	if rb.cfg.MaxPositionUSD > 0 && rb.cfg.MaxPositionUSD < budget {
		budget = rb.cfg.MaxPositionUSD
	}
	return int(budget / price)
}

// liquidityCap = LiquidityPct x 20-day average dollar volume.
func (rb *RiskBudget) liquidityCap(sd *SymbolData) float64 {
	n := len(sd.Bars)
	start := max(0, n-liqVolumeWindow)
	var sum float64
	for i := start; i < n; i++ {
		sum += sd.Bars[i].Close * float64(sd.Bars[i].Volume)
	}
	cnt := n - start
	if cnt == 0 {
		return 0
	}
	return rb.cfg.LiquidityPct * sum / float64(cnt)
}

// avgCorrToHeld averages the candidate's 20-day return correlation
// against every currently-held symbol with usable history.
func (rb *RiskBudget) avgCorrToHeld(candidate string, data MarketData) (float64, bool) {
	cd := data.SD(candidate)
	if cd == nil {
		return 0, false
	}
	candRets := Returns(tailCloses(cd.Closes))
	if len(candRets) < corrWindow/2 {
		return 0, false
	}
	var total float64
	var cnt int
	for sym := range data.Held {
		if sym == candidate {
			continue
		}
		hd := data.SD(sym)
		if hd == nil {
			continue
		}
		r := Returns(tailCloses(hd.Closes))
		if len(r) < corrWindow/2 {
			continue
		}
		total += Corr(candRets, r)
		cnt++
	}
	if cnt == 0 {
		return 0, false
	}
	return total / float64(cnt), true
}

// tailCloses keeps at most corrWindow+1 closes (enough for corrWindow returns).
func tailCloses(c []float64) []float64 {
	if len(c) <= corrWindow+1 {
		return c
	}
	return c[len(c)-corrWindow-1:]
}
