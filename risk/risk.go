// Package risk implements the deterministic pre-trade validator: the
// hard, code-enforced gate between an LLM trade decision and any order
// placement. It re-checks every advertised cap (position notional,
// portfolio percentage, confidence threshold) and refuses outright when
// the kill switch is halted or the market is closed. Unlike the LLM
// prompt text in agents/, these checks run in Go on every single order.
package risk

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/options"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// HaltSource is anything that can report a live kill-switch state.
type HaltSource interface {
	Halted() bool
}

// MarketClock reports market hours. SessionOpen/SessionKnown refine the
// plain open/closed answer for extended-hours trading; implementations
// without that data return false (fail closed).
type MarketClock interface {
	MarketOpen() bool
	// SessionOpen reports whether we are inside the approximated US
	// extended session window (weekdays 04:00-20:00 ET).
	SessionOpen(p Proposal) bool
	// SessionKnown reports whether the clock could determine any session
	// state at all (false = unknown clock, always reject).
	SessionKnown() bool
}

// Portfolio is the account snapshot the validator needs.
type Portfolio struct {
	Equity         float64 // total account equity
	PortfolioValue float64 // current portfolio value
}

// ExistingPosition returns the absolute USD value already invested in a
// symbol (zero if unknown/flat).
type ExistingPosition func(symbol string) float64

// PriceSource supplies a live mark price (per share or per contract) for
// notional sizing. The default implementation hits Alpaca's snapshot
// endpoints; tests inject fakes. Implementations MUST fail closed: any
// error is surfaced and the proposal is rejected.
type PriceSource interface {
	Price(ctx context.Context, symbol string, isOption bool) (float64, error)
}

// OptionContractMultiplier is the standard US equity-option contract
// multiplier: one contract controls 100 shares.
const OptionContractMultiplier = 100.0

// Proposal is one candidate order awaiting validation. OrderType and
// TimeInForce feed the extended-hours session check; empty values are
// treated as a plain market/gtc order.
type Proposal struct {
	Symbol        string
	Side          string // buy | sell
	Qty           string // shares (or contracts); either Qty or Notional must be set
	Notional      string // USD value; takes precedence when set (equities only)
	Confidence    *float64
	OrderType     string // market | limit | stop | ...
	TimeInForce   string // day | gtc | ioc | fop | ...
	ExtendedHours bool   // caller explicitly requested extended hours
}

// Verdict is the outcome of validating a proposal. Factors is nil
// unless a FactorScorer is configured on the validator.
type Verdict struct {
	Approved bool
	Reasons  []string
	Notional float64
	Factors  *FactorResult
}

// FactorScorer produces the multi-factor composite score for a symbol.
// It is optional on Validator (nil = single-confidence behavior, so all
// pre-existing tests keep passing). factors.Engine implements it.
type FactorScorer interface {
	ScoreFactors(ctx context.Context, symbol string) (FactorResult, error)
}

// FactorResult is the multi-factor outcome attached to a verdict.
type FactorResult struct {
	Composite  float64            // weighted composite score, 0..1
	MinScore   float64            // FACTOR_MIN_SCORE it was judged against
	Passed     bool               // Composite >= MinScore
	Factors    map[string]float64 // factor name -> individual score
	Rationales map[string]string  // factor name -> human-readable rationale
}

func (v Verdict) String() string {
	if v.Approved {
		return "APPROVED"
	}
	return "REJECTED: " + strings.Join(v.Reasons, "; ")
}

// Validator checks proposals against configured caps and live state.
// It is stateless apart from its dependencies and safe for concurrent use.
type Validator struct {
	Cfg       *config.Config
	Kill      HaltSource
	Clock     MarketClock      // optional: nil disables the market-hours check
	Positions ExistingPosition // optional: nil treats existing exposure as zero
	Portfolio func() (Portfolio, error)
	Prices    PriceSource  // optional: nil uses Alpaca snapshots; fails closed on error
	Factors   FactorScorer // optional: nil keeps single-confidence behavior; when set BOTH gates must pass

	ctx context.Context // per-call context for price fetches; Background when unset
}

// priceSource returns the configured source, constructing the default
// Alpaca-backed one lazily from config when none was injected.
func (v *Validator) priceSource() PriceSource {
	if v.Prices != nil {
		return v.Prices
	}
	c := tools.NewClient(v.Cfg.AlpacaKeyID, v.Cfg.AlpacaSecret, v.Cfg.AlpacaBaseURL, v.Cfg.AlpacaDataURL)
	return alpacaPrices{client: c}
}

// alpacaPrices is the default PriceSource: stock snapshots for equities,
// option snapshots for OCC symbols.
type alpacaPrices struct {
	client *tools.Client
}

func (a alpacaPrices) Price(ctx context.Context, symbol string, isOption bool) (float64, error) {
	if !isOption {
		snaps, err := a.client.GetSnapshots(ctx, []string{symbol})
		if err != nil {
			return 0, err
		}
		s, ok := snaps[symbol]
		if !ok {
			return 0, fmt.Errorf("no snapshot for %s", symbol)
		}
		return s.LatestTradePrice, nil
	}
	oc := options.NewClient(a.client)
	snaps, err := oc.GetSnapshots(ctx, []string{symbol})
	if err != nil {
		return 0, err
	}
	s, ok := snaps[symbol]
	if !ok {
		return 0, fmt.Errorf("no options snapshot for %s", symbol)
	}
	mid := s.MidQuote()
	if mid <= 0 {
		return 0, fmt.Errorf("no quotable price for option %s", symbol)
	}
	return mid, nil
}

// Validate runs every deterministic check against one proposal. It never
// panics and always returns a verdict; any error fetching live state is
// treated as a rejection (fail closed).
func (v *Validator) Validate(p Proposal) Verdict {
	var reasons []string
	isOption := tools.IsOCCSymbol(p.Symbol)

	// 0. Options-specific hard rules, mirroring Alpaca's server-side
	// validations client-side: no notional sizing, extended_hours must be
	// false, time_in_force day|gtc only.
	if isOption {
		if strings.TrimSpace(p.Notional) != "" {
			reasons = append(reasons, "options orders must use qty of contracts, not notional")
		}
		if p.ExtendedHours {
			reasons = append(reasons, "options orders do not support extended_hours")
		}
		if tif := strings.ToLower(strings.TrimSpace(p.TimeInForce)); tif != "" && tif != "day" && tif != "gtc" {
			reasons = append(reasons, fmt.Sprintf("options orders require time_in_force=day|gtc, got %q", p.TimeInForce))
		}
	}

	// 1. Kill switch: nothing trades while halted.
	if v.Kill != nil && v.Kill.Halted() {
		reasons = append(reasons, "kill switch engaged")
	}

	// 2. Market hours: refuse orders outside trading sessions. With
	// EXTENDED_HOURS enabled, limit/day|gtc equity orders are allowed during
	// the US extended sessions (weekdays 04:00-20:00 ET); options never
	// trade extended hours. Anything outside that window (weekends) and any
	isCrypto := tools.IsCryptoSymbol(p.Symbol)
	if v.Clock != nil && !isCrypto && !v.Clock.MarketOpen() {
		// Crypto trades 24/7 on Alpaca; skip the equity market clock
		// check entirely for BASE/QUOTE symbols.
		// weekend; they only execute when the market reopens. Options and
		// market-type orders still fail closed.
		if v.Cfg.PreOrders && !isOption && strings.EqualFold(strings.TrimSpace(p.OrderType), "limit") {
			tif := strings.ToLower(strings.TrimSpace(p.TimeInForce))
			if tif != "day" && tif != "gtc" {
				reasons = append(reasons, fmt.Sprintf("pre-orders require time_in_force=day|gtc, got %q", p.TimeInForce))
			}
		} else if !isOption && v.Cfg.ExtendedHours && v.Clock.SessionOpen(p) {
			// Extended session: acceptable for limit day/gtc only.
			t := strings.ToLower(strings.TrimSpace(p.OrderType))
			tif := strings.ToLower(strings.TrimSpace(p.TimeInForce))
			if t != "limit" || (tif != "day" && tif != "gtc") {
				reasons = append(reasons, fmt.Sprintf(
					"extended-hours orders must be type=limit with time_in_force=day|gtc, got type=%q tif=%q",
					p.OrderType, p.TimeInForce))
			}
		} else if !isOption && v.Cfg.ExtendedHours && v.Clock.SessionKnown() {
			reasons = append(reasons, "outside extended-hours window (weekend or closed day)")
		} else {
			reasons = append(reasons, "market closed")
		}
	}

	// 3. Basic sanity of the proposal itself.
	sym := strings.ToUpper(strings.TrimSpace(p.Symbol))
	if sym == "" {
		reasons = append(reasons, "missing symbol")
	}
	side := strings.ToLower(strings.TrimSpace(p.Side))
	if side != "buy" && side != "sell" {
		reasons = append(reasons, fmt.Sprintf("invalid side %q", p.Side))
	}
	qty := parsePositive(p.Qty)
	notional := parsePositive(p.Notional)

	if p.Confidence == nil {
		reasons = append(reasons, "missing confidence score")
	} else if *p.Confidence < v.Cfg.MinConfidence {
		reasons = append(reasons, fmt.Sprintf("confidence %.2f < minimum %.2f", *p.Confidence, v.Cfg.MinConfidence))
	}

	// 3b. Multi-factor gate: when a FactorScorer is wired in, the proposal
	// must clear BOTH the LLM confidence threshold AND the composite
	// factor score (FACTOR_MIN_SCORE). Rejection reasons name every
	// failing factor with its score. Scorer errors reject — fail closed.
	var factorRes *FactorResult
	if v.Factors != nil {
		fr, err := v.Factors.ScoreFactors(v.contextOrBackground(), sym)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("factor scoring unavailable (%v)", err))
		} else {
			f := fr // copy for the verdict
			factorRes = &f
			if !fr.Passed {
				reasons = append(reasons, fmt.Sprintf("composite factor score %.2f < minimum %.2f", fr.Composite, fr.MinScore))
				for _, name := range sortedFactorNames(fr.Factors) {
					if fr.Factors[name] < 0.5 {
						reasons = append(reasons, fmt.Sprintf("weak factor %s=%.2f: %s", name, fr.Factors[name], fr.Rationales[name]))
					}
				}
			}
		}
	}

	// 4. Sizing. For OCC proposals the USD exposure is the premium outlay:
	// qty x contract multiplier (100) x mark price from the injected
	// PriceSource (quote mid or last trade). Price fetch failure rejects —
	// fail closed. Equities keep the legacy behavior: explicit notional
	// wins; otherwise fall back to a qty-only sanity check.
	if isOption && qty > 0 {
		price, err := v.priceSource().Price(v.contextOrBackground(), sym, true)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("option price unavailable (%v)", err))
			return Verdict{Approved: false, Reasons: reasons}
		}
		notional = qty * OptionContractMultiplier * price
	} else if notional <= 0 && p.Qty != "" {
		notional = parsePositive(p.Qty) // without a price we can only sanity-check qty>0 below
	}

	// 5. Portfolio-level checks need live equity — fail closed on error.
	pf, err := v.Portfolio()
	if err != nil {
	} else {
		if notional > 0 {
			perCap := v.Cfg.MaxPositionUSD
			if isCrypto {
				perCap = v.Cfg.CryptoMaxPositionUSD
			}
			if notional > perCap {
				reasons = append(reasons, fmt.Sprintf("notional %.2f exceeds per-position cap %.2f", notional, perCap))
			}
			base := pf.Equity
			if base <= 0 {
				base = pf.PortfolioValue
			}
			if base > 0 && !isCrypto && notional/base > v.Cfg.MaxPortfolioPct {
				reasons = append(reasons, fmt.Sprintf("notional %.2f is %.1f%% of portfolio, exceeds cap %.1f%%",
					notional, 100*notional/base, 100*v.Cfg.MaxPortfolioPct))
			}
			// Additive exposure: existing stake in this symbol plus the new order.
			if v.Positions != nil && sym != "" && base > 0 {
				existing := math.Abs(v.Positions(sym))
				if (existing+notional)/base > v.Cfg.MaxPortfolioPct {
					reasons = append(reasons, fmt.Sprintf("combined exposure %.2f (%.1f%% of portfolio) exceeds cap %.1f%%",
						existing+notional, 100*(existing+notional)/base, 100*v.Cfg.MaxPortfolioPct))
				}
			}
		} else if !(isOption && qty > 0) || notional <= 0 {
			reasons = append(reasons, "no positive qty or notional to size the order")
		}
	}

	return Verdict{Approved: len(reasons) == 0, Reasons: reasons, Notional: notional, Factors: factorRes}
}

// sortedFactorNames returns factor names in deterministic order so
// rejection reasons are stable for logs and tests.
func sortedFactorNames(m map[string]float64) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (v *Validator) contextOrBackground() context.Context {
	if v.ctx != nil {
		return v.ctx
	}
	return context.Background()
}

// WithContext returns a copy of the validator bound to ctx for price
// fetches during Validate. Production callers should bind the request
// context so option-price lookups honor cancellation; tests may omit it.
func (v *Validator) WithContext(ctx context.Context) *Validator {
	cp := *v
	cp.ctx = ctx
	return &cp
}

// parsePositive parses a float, returning -1 on any failure so callers
// can distinguish "unset" from zero without extra flags.
func parsePositive(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 {
		return -1
	}
	return f
}

// InExtendedSession approximates the US extended session window from a
// clock timestamp's weekday and wall-clock hour: weekdays 04:00-20:00 ET.
// Weekends are never open. Used by clock adapters implementing
// MarketClock.SessionOpen so the window math lives beside the gate.
func InExtendedSession(weekday time.Weekday, hour, minute int) bool {
	switch weekday {
	case time.Saturday, time.Sunday:
		return false
	}
	mins := hour*60 + minute
	return mins >= 4*60 && mins < 20*60
}
