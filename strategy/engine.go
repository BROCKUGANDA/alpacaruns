package strategy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/BROCKUGANDA/alpacaruns/factors"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)


// priceFetcher returns a live mark for one symbol. Production backs it
// with Alpaca snapshots; tests inject fakes.
type priceFetcher interface {
	Price(ctx context.Context, symbol string) (float64, error)
}

// factorsScored is the flat factor triple the deterministic rule needs.
type factorsScored struct {
	Composite float64
	Trend     float64
	Momentum  float64
}

// factorScorer produces the flat score for one symbol; fakes implement it
// in tests.
type factorScorer interface {
	ScoreFactorsFlat(ctx context.Context, symbol string) (factorsScored, error)
}

// FactorScorer adapts *factors.Engine (public API) to factorScorer.
// Trend/momentum come from the per-factor map; a missing key scores 0 so
// the rule can never BUY on absent data.
type FactorScorer struct{ Inner *factors.Engine }

// ScoreFactorsFlat wraps the real multi-factor engine and flattens its
// result to (composite, trend, momentum).
func (f FactorScorer) ScoreFactorsFlat(ctx context.Context, symbol string) (factorsScored, error) {
	res, err := f.Inner.Score(ctx, symbol)
	if err != nil {
		return factorsScored{}, err
	}
	out := factorsScored{Composite: res.Composite}
	if tr, ok := res.Factors["trend"]; ok {
		out.Trend = tr.Score
	}
	if mo, ok := res.Factors["momentum"]; ok {
		out.Momentum = mo.Score
	}
	return out, nil
}

// snapshotPrices implements priceFetcher over tools.Client.Do:
// equities via /v2/stocks/snapshots, crypto via /v1beta3/crypto/us/
// latest/bars (crypto market data lives under v1beta3; GetBars' stocks
// path does not serve BTC/USD style symbols).
type snapshotPrices struct {
	c *tools.Client
}

// NewPriceSource builds the production mark-price source over a client.
func NewPriceSource(c *tools.Client) priceFetcher { return snapshotPrices{c: c} }

type snapResponse struct {
	Snapshots map[string]struct {
		LatestTrade struct {
			P float64 `json:"p"`
		} `json:"latestTrade"`
		DailyBar struct {
			Close float64 `json:"c"`
		} `json:"dailyBar"`
	} `json:"snapshots"`
}

func (s snapshotPrices) Price(ctx context.Context, symbol string) (float64, error) {
	if IsCrypto(symbol) {
		return s.cryptoPrice(ctx, symbol)
	}
	var out snapResponse
	q := url.Values{"symbols": {symbol}}
	if err := s.c.Do(ctx, http.MethodGet, s.c.DataURL+"/stocks/snapshots", q, nil, &out); err != nil {
		return 0, err
	}
	snap, ok := out.Snapshots[symbol]
	if !ok {
		return 0, fmt.Errorf("no snapshot for %s", symbol)
	}
	if snap.LatestTrade.P > 0 {
		return snap.LatestTrade.P, nil
	}
	if snap.DailyBar.Close > 0 {
		return snap.DailyBar.Close, nil
	}
	return 0, fmt.Errorf("snapshot for %s has no usable price", symbol)
}

func (s snapshotPrices) cryptoPrice(ctx context.Context, symbol string) (float64, error) {
	var out struct {
		Bars map[string]struct {
			Close float64 `json:"c"`
		} `json:"bars"`
	}
	q := url.Values{"symbols": {symbol}}
	endpoint := strings.Replace(s.c.DataURL+"/crypto/us/latest/bars", "/v2/", "/v1beta3/", 1)
	if err := s.c.Do(ctx, http.MethodGet, endpoint, q, nil, &out); err != nil {
		return 0, err
	}
	bar, ok := out.Bars[symbol]
	if !ok || bar.Close <= 0 {
		return 0, fmt.Errorf("no crypto bar for %s", symbol)
	}
	return bar.Close, nil
}

// Engine is the deterministic decision engine: fetch bars -> factor score
// -> threshold rule -> sized entry plan. No LLM anywhere.
type Engine struct {
	score       factorScorer
	prices      priceFetcher
	refBars     barSource // optional; powers PlanEntryFromReference + ATR brackets
	threshold   Thresholds
	sizing      Sizing
	tpPct       float64
	slPct       float64
	bracketMode string  // "pct" (default) | "atr"
	atrMultTP   float64 // ATR mode: TP = entry + mult x ATR14
	atrMultSL   float64 // ATR mode: SL = entry - mult x ATR14
	log         *log.Logger
}

// barSource supplies historical bars; satisfied by *MultiVenueBars.
type barSource interface {
	GetBars(ctx context.Context, symbols []string, timeframe, start, end string, limit int) (map[string][]tools.Bar, error)
}

// EngineConfig wires an Engine. All fields required except Log and
// ReferenceBars (needed only for off-hours pre-order planning and ATR
// brackets).
type EngineConfig struct {
	Scorer        factorScorer
	Prices        priceFetcher
	ReferenceBars barSource
	Threshold     Thresholds
	Sizing        Sizing
	TPPct         float64
	SLPct         float64
	BracketMode   string  // "" | "pct" | "atr" ("" and "pct" are equivalent)
	ATRMultTP     float64
	ATRMultSL     float64
	Log           *log.Logger
}

// NewEngine validates wiring and returns a ready engine.
func NewEngine(c EngineConfig) (*Engine, error) {
	if c.Scorer == nil || c.Prices == nil {
		return nil, fmt.Errorf("engine needs a scorer and a price source")
	}
	if c.TPPct <= c.SLPct || c.SLPct <= 0 {
		return nil, fmt.Errorf("bracket pcts invalid: need TP_PCT > SL_PCT > 0, got %g/%g", c.TPPct, c.SLPct)
	}
	mode := strings.ToLower(strings.TrimSpace(c.BracketMode))
	if mode == "" {
		mode = "pct"
	}
	if mode != "pct" && mode != "atr" {
		return nil, fmt.Errorf("bracket mode must be pct|atr, got %q", c.BracketMode)
	}
	lg := c.Log
	if lg == nil {
		lg = log.Default()
	}
	return &Engine{
		score: c.Scorer, prices: c.Prices, refBars: c.ReferenceBars,
		threshold: c.Threshold, sizing: c.Sizing,
		tpPct: c.TPPct, slPct: c.SLPct,
		bracketMode: mode, atrMultTP: c.ATRMultTP, atrMultSL: c.ATRMultSL,
		log: lg,
	}, nil
}

// PriceOf resolves the current mark for one symbol via the engine's
// price source (stock snapshots; crypto latest-bars under v1beta3).
func (e *Engine) PriceOf(ctx context.Context, symbol string) (float64, error) {
	return e.prices.Price(ctx, symbol)
}

// RunTick evaluates every symbol in the universe and returns decisions
// sorted by symbol. held marks currently-open positions. A scoring error
// on one symbol is logged and skipped — one dead feed never blocks the
// others (fail-safe per symbol, never silent portfolio-wide).
func (e *Engine) RunTick(ctx context.Context, universe []string, held map[string]bool) []Decision {
	syms := append([]string(nil), universe...)
	sort.Strings(syms)
	var out []Decision
	for _, sym := range syms {
		fs, err := e.score.ScoreFactorsFlat(ctx, sym)
		if err != nil {
			e.log.Printf("[strategy] %s: scoring unavailable: %v", sym, err)
			continue
		}
		out = append(out, Decide(sym, held[sym], fs.Composite, fs.Trend, fs.Momentum, e.threshold))
	}
	return out
}

// EntryPlan is an executable entry: sized quantity plus (for equities)
// bracket levels computed off the current mark.
type EntryPlan struct {
	Symbol   string
	Side     string // "buy"
	Qty      int    // shares or whole contracts; 0 when Notional is set
	Budget   float64
	Price    float64
	Crypto   bool
	Notional float64 // USD notional for fractional crypto (0 for qty orders)
	Brackets BracketLevels // zero value for crypto (local TP/SL instead)
}
// PlanEntry sizes an equity/crypto BUY at mark price and computes bracket
// levels for equities. Crypto returns Crypto=true with brackets zeroed:
// Alpaca does not support order_class=bracket on crypto, so TP/SL levels
// are persisted locally and enforced by PositionMonitor.
//
// Bracket placement follows the engine's bracketMode: "pct" (default)
// uses fixed TP_PCT/SL_PCT percentages; "atr" scales TP/SL off the
// symbol's ATR14 (volatility-adaptive, swing-friendly breathing stops).
func (e *Engine) PlanEntry(ctx context.Context, d Decision, portfolioValue float64) (EntryPlan, error) {
	price, err := e.prices.Price(ctx, d.Symbol)
	if err != nil {
		return EntryPlan{}, fmt.Errorf("mark price %s: %w", d.Symbol, err)
	}
	sizing := e.sizing
	if d.Sizing.PositionPct > 0 || d.Sizing.MaxPositionUSD > 0 {
		sizing = d.Sizing
	}
	// Crypto entries use notional USD sizing when the decision carries
	// a non-zero Notional flag. This fractional-fill path lets small
	// accounts buy whole coins (e.g. 0.04 BTC = $3000) without the
	// unit-min-qty block that qty-based sizing imposes.
	if d.Notional && IsCrypto(d.Symbol) {
		// When the user has set CRYPTO_NOTIONAL_USD, the notional amount
		// comes from the decision (not the per-call sizing budget). This
		// lets the user cap each crypto entry to a fixed dollar amount
		// independent of MAX_POSITION_USD. Otherwise, fall back to the
		// sized budget (which uses the equity MAX_POSITION_USD as a cap).
		var notional float64
		if d.NotionalUSD > 0 {
			notional = d.NotionalUSD
		} else {
			nr := sizing.SizeNotional(portfolioValue, price)
			if nr.Skip != "" {
				return EntryPlan{}, fmt.Errorf("%s: %s", d.Symbol, nr.Skip)
			}
			notional = nr.Budget
		}
		p := EntryPlan{
			Symbol: d.Symbol, Side: "buy", Qty: 0,
			Budget: notional, Price: price, Crypto: true,
			Notional: notional,
		}
		// Crypto TP/SL: Alpaca rejects server-side brackets on crypto
		// orders, so we compute them locally from the engine's tpPct/
		// slPct and persist them in strategy-state.json. The
		// PositionMonitor reads them every tick and closes the position
		// when price crosses either level. Without this, the monitor
		// never triggers because the stored levels stay at zero.
		lv, err := ComputeBrackets(price, e.tpPct, e.slPct)
		if err == nil {
			p.Brackets = lv
		}
		e.log.Printf("[strategy] %s notional entry: $%.2f at $%.2f tp=%.2f sl=%.2f",
			p.Symbol, notional, price, p.Brackets.TakeProfit, p.Brackets.StopLoss)
		return p, nil
	}
	qr := sizing.SizeForPrice(portfolioValue, price)
	if qr.Qty < 1 {
		return EntryPlan{}, fmt.Errorf("%s: %s", d.Symbol, qr.Skip)
	}
	p := EntryPlan{
		Symbol: d.Symbol, Side: "buy", Qty: qr.Qty,
		Budget: qr.Budget, Price: price, Crypto: IsCrypto(d.Symbol),
	}
	if p.Crypto {
		// Crypto qty-based entry: same TP/SL story as the notional branch.
		lv, err := ComputeBrackets(price, e.tpPct, e.slPct)
		if err == nil {
			p.Brackets = lv
		}
		e.log.Printf("[strategy] %s is crypto: server-side brackets unsupported; TP/SL enforced locally tp=%.2f sl=%.2f",
			p.Symbol, p.Brackets.TakeProfit, p.Brackets.StopLoss)
		return p, nil
	}
	lv, err := e.brackets(ctx, d.Symbol, price)
	if err != nil {
		return EntryPlan{}, err
	}
	p.Brackets = lv
	return p, nil
}

// brackets computes TP/SL for an entry at price per the configured mode.
// ATR mode needs daily bars; it falls back to pct brackets when no bar
// source is wired or bars are unavailable (never blocks an entry on a
// data hiccup — the stop still exists, just percentage-based).
func (e *Engine) brackets(ctx context.Context, symbol string, entry float64) (BracketLevels, error) {
	if e.bracketMode == "atr" && e.refBars != nil {
		bars, err := e.refBars.GetBars(ctx, []string{symbol}, "1Day", "", "", 30)
		if err == nil {
			if atr, ok := ATR14(bars[symbol]); ok && atr > 0 {
				return ComputeATRBrackets(entry, atr, e.atrMultTP, e.atrMultSL)
			}
			e.log.Printf("[strategy] %s: ATR unavailable (%d bars); falling back to pct brackets",
				symbol, len(bars[symbol]))
		} else {
			e.log.Printf("[strategy] %s: ATR fetch failed: %v; falling back to pct brackets", symbol, err)
		}
	}
	return ComputeBrackets(entry, e.tpPct, e.slPct)
}

// PlanEntryFromReference sizes an entry at the symbol's most recent daily
// close instead of a live snapshot — used when the market is closed and
// snapshots carry no usable price (pre-open / post-close). The resulting
// plan is meant to be placed as a resting limit/GTC pre-order; the close
// is a stale-but-honest mark, so the caller must gate this behind
// PRE_ORDERS. Brackets are computed from the same reference price.
func (e *Engine) PlanEntryFromReference(ctx context.Context, d Decision, portfolioValue float64) (EntryPlan, error) {
	if e.refBars == nil {
		return EntryPlan{}, fmt.Errorf("no reference bar source wired (set ReferenceBars)")
	}
	bars, err := e.refBars.GetBars(ctx, []string{d.Symbol}, "1Day", "", "", 5)
	if err != nil {
		return EntryPlan{}, fmt.Errorf("reference price %s: %w", d.Symbol, err)
	}
	bs := bars[d.Symbol]
	if len(bs) == 0 || bs[len(bs)-1].Close <= 0 {
		return EntryPlan{}, fmt.Errorf("reference price %s: no recent daily bar", d.Symbol)
	}
	price := bs[len(bs)-1].Close
	sizing := e.sizing
	if d.Sizing.PositionPct > 0 || d.Sizing.MaxPositionUSD > 0 {
		sizing = d.Sizing
	}
	qr := sizing.SizeForPrice(portfolioValue, price)
	if qr.Qty < 1 {
		return EntryPlan{}, fmt.Errorf("%s: %s", d.Symbol, qr.Skip)
	}
	p := EntryPlan{
		Symbol: d.Symbol, Side: "buy", Qty: qr.Qty,
		Budget: qr.Budget, Price: price, Crypto: IsCrypto(d.Symbol),
	}
	if p.Crypto {
		return p, nil
	}
	lv, err := ComputeBrackets(price, e.tpPct, e.slPct)
	if err != nil {
		return EntryPlan{}, err
	}
	p.Brackets = lv
	return p, nil
}
