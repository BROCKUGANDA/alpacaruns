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

// reduceQtyEpsilon absorbs float noise when comparing a reducing order's
// quantity against the live holding. It sits far below any tradable size,
// so a difference this small means "same number", never "still owed".
const reduceQtyEpsilon = 1e-9

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
	// Reducing asserts that this order closes or shrinks exposure the
	// account already holds rather than creating new exposure. It is not a
	// permission slip: when set, the validator PROVES it against a live
	// position read through Holdings, and refuses the order if the
	// quantity would oversell the position or flip its sign.
	//
	// The exemptions it does buy are the ones that must never apply to
	// risk reduction: the kill switch, the confidence floor, the factor
	// gate and the exposure caps. A halt exists to stop the bot taking
	// more risk; blocking the flatten while halted leaves the position
	// unprotected by the very control meant to protect it.
	Reducing bool
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
	// Exposure is the fail-closed form of Positions: it reports whether the
	// book could be read at all. When set, an unreadable book REFUSES the
	// order instead of sizing it against zero — the difference between a
	// cap and a guess. It takes precedence over Positions.
	Exposure func(symbol string) (float64, error)
	// Holdings returns the SIGNED live quantity held in a symbol (+long,
	// -short). It is what makes a Reducing assertion provable rather than
	// self-declared. When Reducing is asserted with no Holdings source
	// wired, the order is REJECTED: an exit that cannot be shown to be
	// reducing is indistinguishable from a short sale.
	Holdings  func(symbol string) (float64, error)
	Portfolio func() (Portfolio, error)
	Prices    PriceSource  // optional: nil uses Alpaca snapshots; fails closed on error
	Factors   FactorScorer // optional: nil keeps single-confidence behavior; when set BOTH gates must pass

	ctx context.Context // per-call context for price fetches; Background when unset
}

// existingExposure returns the USD already committed to a symbol.
//
// Preference order is a safety choice, not style: the error-returning
// Exposure source wins because it can say "unknown", and unknown must
// reject. The plain Positions source is kept for callers that predate it;
// when NEITHER is wired the caller gets an error rather than a silent zero,
// because a cap evaluated against a book nobody looked at is decoration.
func (v *Validator) existingExposure(symbol string) (float64, error) {
	if symbol == "" {
		return 0, nil
	}
	if v.Exposure != nil {
		usd, err := v.Exposure(symbol)
		if err != nil {
			return 0, err
		}
		return math.Abs(usd), nil
	}
	if v.Positions != nil {
		return math.Abs(v.Positions(symbol)), nil
	}
	return 0, fmt.Errorf("no exposure source wired")
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

// alpacaPrices is the default PriceSource: stock snapshots for equities
// and ETFs, crypto snapshots for crypto pairs, option snapshots for OCC
// symbols.
type alpacaPrices struct {
	client *tools.Client
}

func (a alpacaPrices) Price(ctx context.Context, symbol string, isOption bool) (float64, error) {
	if isOption {
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
	if tools.IsCryptoSymbol(symbol) {
		snaps, err := a.client.GetCryptoSnapshots(ctx, []string{symbol})
		if err != nil {
			return 0, err
		}
		// The snapshot map is keyed by the form the caller ASKED for
		// ("BTC/USD"); the broker's order symbol ("BTCUSD") is a different
		// string. Looking up only the order form missed every entry and
		// reported "no crypto snapshot", which rejected every crypto order
		// that needed a price.
		s, ok := snaps[symbol]
		if !ok {
			s, ok = snaps[tools.CryptoOrderSymbol(symbol)]
		}
		if !ok && len(snaps) == 1 {
			for _, v := range snaps {
				s, ok = v, true
			}
		}
		if !ok {
			return 0, fmt.Errorf("no crypto snapshot for %s", symbol)
		}
		px := s.LatestTrade
		if px <= 0 {
			px = s.DailyBarClose
		}
		if px <= 0 {
			return 0, fmt.Errorf("no quotable price for %s", symbol)
		}
		return px, nil
	}
	snaps, err := a.client.GetSnapshots(ctx, []string{symbol})
	if err != nil {
		return 0, err
	}
	s, ok := snaps[symbol]
	if !ok {
		return 0, fmt.Errorf("no snapshot for %s", symbol)
	}
	px := s.MidPrice()
	if px <= 0 {
		return 0, fmt.Errorf("no quotable price for %s", symbol)
	}
	return px, nil
}

// Validate runs every deterministic check against one proposal. It never
// panics and always returns a verdict; any error fetching live state is
// treated as a rejection (fail closed).
func (v *Validator) Validate(p Proposal) Verdict {
	var reasons []string
	if v == nil || v.Cfg == nil {
		return Verdict{Approved: false, Reasons: []string{"validator is not configured"}}
	}
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

	// 1. Kill switch: nothing trades while halted — except an order that
	// reduces risk, which the halt must never block (see Proposal.Reducing).
	if !p.Reducing && v.Kill != nil && v.Kill.Halted() {
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

	// 3b. Proving a reducing order. The claim is the caller's; the position
	// is the broker's. Every exemption below rests on this being true, so
	// an unprovable "reducing" assertion is refused outright.
	if p.Reducing {
		switch {
		case v.Holdings == nil:
			reasons = append(reasons, "reducing order refused: no live holdings source wired, cannot prove it reduces exposure")
		default:
			live, err := v.Holdings(sym)
			held := math.Abs(live)
			switch {
			case err != nil:
				reasons = append(reasons, fmt.Sprintf("reducing order refused: position unreadable (%v)", err))
			case live == 0:
				reasons = append(reasons, fmt.Sprintf("reducing order refused: no %s position to reduce", sym))
			case side == "sell" && live < 0:
				reasons = append(reasons, fmt.Sprintf("reducing sell refused: %s is short %.6f, selling would ADD exposure", sym, live))
			case side == "buy" && live > 0:
				reasons = append(reasons, fmt.Sprintf("reducing buy refused: %s is long %.6f, buying would ADD exposure", sym, live))
			case qty <= 0:
				reasons = append(reasons, "reducing order must carry a positive qty")
			case qty > held+reduceQtyEpsilon:
				reasons = append(reasons, fmt.Sprintf(
					"reducing order oversells: qty %.6f exceeds the %.6f held in %s", qty, held, sym))
			}
		}
	}

	// 3c. Multi-factor gate: when a FactorScorer is wired in, the proposal
	// must clear BOTH the LLM confidence threshold AND the composite
	// factor score (FACTOR_MIN_SCORE). Rejection reasons name every
	// failing factor with its score. Scorer errors reject — fail closed.
	// The gate runs BEFORE the confidence check so that a proposal with
	// no self-reported confidence can inherit the deterministic score.
	//
	// Skipped for a proven reducing order: the factor score decides whether
	// to TAKE exposure. It has no bearing on whether to close exposure, and
	// a stale or failing scorer must not be able to hold a position shut.
	var factorRes *FactorResult
	if v.Factors != nil && !p.Reducing {
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

	// 4. Confidence. Every proposal must carry a confidence value. The
	// agent path has no field to carry one (Alpaca's MCP order schema
	// has no confidence parameter), so the MCP guard derives it from the
	// deterministic factor composite before validating. With no
	// confidence at all there is nothing to judge, so reject.
	//
	// Skipped for a proven reducing order: an exit is not a bet, and a
	// missing confidence number must never strand a position.
	if !p.Reducing {
		if p.Confidence == nil {
			reasons = append(reasons, "missing confidence score")
		} else if *p.Confidence < v.Cfg.MinConfidence {
			reasons = append(reasons, fmt.Sprintf("confidence %.2f < minimum %.2f", *p.Confidence, v.Cfg.MinConfidence))
		}
	}

	// 4b. A proven reducing order stops here. The remaining steps size and
	// cap NEW exposure, and both need a live price and a live account read
	// to do it — a snapshot outage or an account-echo failure must not be
	// able to strand a position that the account already agrees should be
	// closed. The reducing proof above is what makes leaving early safe:
	// the order cannot oversell, cannot flip the position's sign, and adds
	// no exposure by construction.
	if p.Reducing {
		return Verdict{Approved: len(reasons) == 0, Reasons: reasons, Notional: 0, Factors: factorRes}
	}

	// 5. Sizing. The USD exposure of the order must be computed from real
	// prices: a share count is NOT a dollar amount. Treating qty as
	// notional made a 1,000-share order look like a $1,000 order and
	// sailed under the per-position and portfolio caps.
	if isOption && qty > 0 {
		price, err := v.priceSource().Price(v.contextOrBackground(), sym, true)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("option price unavailable (%v)", err))
			return Verdict{Approved: false, Reasons: reasons}
		}
		notional = qty * OptionContractMultiplier * price
	} else if notional > 0 {
		// An explicit notional wins.
	} else if qty > 0 {
		price, err := v.priceSource().Price(v.contextOrBackground(), sym, false)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("price unavailable (%v)", err))
			return Verdict{Approved: false, Reasons: reasons}
		}
		notional = qty * price
	} else {
		reasons = append(reasons, "no positive qty or notional to size the order")
	}

	// 6. Portfolio-level checks need live equity — fail closed on error.
	if v.Portfolio == nil {
		reasons = append(reasons, "account unavailable (no portfolio source configured)")
		return Verdict{Approved: false, Reasons: reasons}
	}
	pf, err := v.Portfolio()
	if err != nil {
		reasons = append(reasons, fmt.Sprintf("account unavailable (%v)", err))
		return Verdict{Approved: false, Reasons: reasons}
	}
	if notional > 0 && !p.Reducing {
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
		if base <= 0 {
			// Without a usable equity base the percentage caps cannot be
			// evaluated — refusing is the only safe reading.
			reasons = append(reasons, "account equity unavailable for percentage caps")
		} else {
			if notional/base > v.Cfg.MaxPortfolioPct {
				reasons = append(reasons, fmt.Sprintf("notional %.2f is %.1f%% of portfolio, exceeds cap %.1f%%",
					notional, 100*notional/base, 100*v.Cfg.MaxPortfolioPct))
			}
			// Additive exposure: existing stake in this symbol plus the new
			// order. With only a per-order percentage, N positions each
			// under the cap produce a book far above it, so this check is
			// the portfolio-level half of the gate rather than a refinement.
			existing, err := v.existingExposure(sym)
			switch {
			case err != nil:
				reasons = append(reasons, fmt.Sprintf("existing exposure unreadable (%v); cannot size against an unknown book", err))
			case existing > 0 || sym != "":
				if (existing+notional)/base > v.Cfg.MaxPortfolioPct {
					reasons = append(reasons, fmt.Sprintf("combined exposure %.2f (%.1f%% of portfolio) exceeds cap %.1f%%",
						existing+notional, 100*(existing+notional)/base, 100*v.Cfg.MaxPortfolioPct))
				}
			}
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

// Context returns the context the validator uses for live lookups
// (context.Background when none was bound).
func (v *Validator) Context() context.Context {
	if v == nil {
		return context.Background()
	}
	return v.contextOrBackground()
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
