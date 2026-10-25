// Package picker is the universal signal picker: one place that knows
// what the bot can trade, which venue each instrument lives on, how its
// data must be fetched, and how strong its current signal is.
//
// Before this package the universe was two flat lists — equities and
// crypto — with the asset class sniffed from the symbol text
// ("contains a slash => crypto"). That made ETFs, index trackers and
// options second-class: they could not be configured, discovered or
// ranked alongside everything else.
//
// The picker replaces the sniffing with an explicit classification:
//
//	ClassEquity  a single listed share (AAPL)
//	ClassETF     an exchange-traded fund (SPY)
//	ClassIndex   an index tracker / index exposure (QQQ, IWM)
//	ClassOption  a single option contract (AAPL260116C00150000)
//	ClassCrypto  a crypto pair (BTC/USD)
//
// Every instrument carries the data family, feed and execution window
// needed to fetch and trade it, so a caller never has to re-derive
// "which endpoint do I call for this symbol".
package picker

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/BROCKUGANDA/alpacaruns/options"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// Class is an instrument's asset class.
type Class string

const (
	ClassEquity Class = "equity"
	ClassETF    Class = "etf"
	ClassIndex  Class = "index"
	ClassOption Class = "option"
	ClassCrypto Class = "crypto"
)

// DataFamily names the market-data endpoint family an instrument's
// data comes from.
type DataFamily string

const (
	// FamilyStocks is /v2/stocks/* — equities, ETFs and index trackers.
	FamilyStocks DataFamily = "stocks"
	// FamilyCrypto is /v1beta3/crypto/us/*.
	FamilyCrypto DataFamily = "crypto"
	// FamilyOptions is /v1beta1/options/*.
	FamilyOptions DataFamily = "options"
)

// Instrument is one tradeable thing with everything needed to fetch
// its data and route an order to it.
type Instrument struct {
	// Symbol is the venue symbol: "AAPL", "SPY", "BTC/USD" or an OCC
	// contract symbol such as "AAPL260116C00150000".
	Symbol string `json:"symbol"`
	// AssetID is the broker's stable identifier for the instrument. It
	// survives a ticker change, which is what makes a rename detectable
	// from broker data instead of guessed from a name similarity.
	AssetID string `json:"asset_id,omitempty"`
	// Class is the asset class.
	Class Class `json:"class"`
	// Family is the market-data endpoint family.
	Family DataFamily `json:"family"`
	// Exchange is the broker's venue code (NASDAQ, ARCA, CRYPTO...).
	Exchange string `json:"exchange,omitempty"`
	// Name is the broker's instrument name.
	Name string `json:"name,omitempty"`
	// Tradable reports whether the broker accepts orders for it.
	Tradable bool `json:"tradable"`
	// Fractional reports whether fractional quantities are allowed.
	Fractional bool `json:"fractional"`
	// Shortable reports whether the symbol can be sold short.
	Shortable bool `json:"shortable"`
	// Window names the execution-window set that governs this
	// instrument: "equity" (session hours) or "crypto" (24/7).
	Window string `json:"window"`
	// Underlying is the option's underlying symbol (options only).
	Underlying string `json:"underlying,omitempty"`
	// Expiration is the option's expiry date (options only).
	Expiration string `json:"expiration,omitempty"`
	// Strike is the option's strike price (options only).
	Strike float64 `json:"strike,omitempty"`
	// OptionType is "call" or "put" (options only).
	OptionType string `json:"option_type,omitempty"`
	// Delta is the option's delta at scan time (options only).
	Delta float64 `json:"delta,omitempty"`
	// ImpliedVol is the option's implied volatility (options only).
	ImpliedVol float64 `json:"implied_vol,omitempty"`
	// Premium is the per-contract mid price (options only).
	Premium float64 `json:"premium,omitempty"`
	// Source records where the instrument came from: "config" or
	// "option-chain".
	Source string `json:"source"`
}

// IsCrypto reports whether the instrument trades on the crypto venue.
func (i Instrument) IsCrypto() bool { return i.Class == ClassCrypto }

// IsOption reports whether the instrument is a single option contract.
func (i Instrument) IsOption() bool { return i.Class == ClassOption }

// DataSymbol returns the symbol form the market-data endpoint wants:
// crypto pairs keep BASE/QUOTE, everything else is unchanged.
func (i Instrument) DataSymbol() string {
	if i.Class == ClassCrypto {
		return tools.CryptoDataSymbol(i.Symbol)
	}
	return i.Symbol
}

// OrderSymbol returns the symbol form the trading endpoint wants:
// crypto pairs lose the separator ("BTCUSD"), everything else is
// unchanged.
func (i Instrument) OrderSymbol() string {
	if i.Class == ClassCrypto {
		return tools.CryptoOrderSymbol(i.Symbol)
	}
	return i.Symbol
}

// Spec is the configured universe. Each list is a class.
type Spec struct {
	Equities          []string
	ETFs              []string
	Indices           []string
	Crypto            []string
	OptionUnderlyings []string
	// OptionMinDTE / OptionMaxDTE bound the expiry window used when
	// expanding an underlying into concrete contracts.
	OptionMinDTE int
	OptionMaxDTE int
	// OptionDeltaMin / OptionDeltaMax bound the absolute delta of a
	// selected contract.
	OptionDeltaMin float64
	OptionDeltaMax float64
}

// Registry is the classified universe.
type Registry struct {
	instruments []Instrument
	bySymbol    map[string]Instrument
}

// Instruments returns every instrument, sorted by class then symbol.
func (r *Registry) Instruments() []Instrument {
	out := append([]Instrument(nil), r.instruments...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Class != out[j].Class {
			return out[i].Class < out[j].Class
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out
}

// ByClass returns the instruments of one class.
func (r *Registry) ByClass(c Class) []Instrument {
	var out []Instrument
	for _, i := range r.instruments {
		if i.Class == c {
			out = append(out, i)
		}
	}
	return out
}

// Lookup finds an instrument by symbol (case-insensitive, ignoring the
// crypto separator so "BTCUSD" and "BTC/USD" resolve to the same one).
func (r *Registry) Lookup(symbol string) (Instrument, bool) {
	want := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(symbol), "/", ""))
	for _, i := range r.instruments {
		got := strings.ToLower(strings.ReplaceAll(i.Symbol, "/", ""))
		if got == want {
			return i, true
		}
	}
	return Instrument{}, false
}

// Symbols returns the venue symbols of every instrument of the given
// classes. The auto loop uses it to build the scoring universe.
func (r *Registry) Symbols(classes ...Class) []string {
	want := map[Class]bool{}
	for _, c := range classes {
		want[c] = true
	}
	var out []string
	for _, i := range r.instruments {
		if len(want) == 0 || want[i.Class] {
			out = append(out, i.Symbol)
		}
	}
	sort.Strings(out)
	return out
}

// TradableSymbols returns the configured symbols the broker will actually
// accept an order for.
//
// Symbols() returns the classified universe as configured, which is what the
// picker page should show; this is what the trading loop should SCORE.
// Without the distinction a delisted, halted or renamed ticker keeps being
// scored and sized every tick, and only the broker's rejection reveals it —
// one rejected order per poll, with the level bookkeeping still treating the
// symbol as live.
func (r *Registry) TradableSymbols(classes ...Class) []string {
	want := map[Class]bool{}
	for _, c := range classes {
		want[c] = true
	}
	var out []string
	for _, i := range r.instruments {
		if !i.Tradable {
			continue
		}
		if len(want) == 0 || want[i.Class] {
			out = append(out, i.Symbol)
		}
	}
	sort.Strings(out)
	return out
}

// Untradable lists the configured instruments the broker will not trade, so
// a caller can say why the universe shrank instead of silently trading less.
func (r *Registry) Untradable() []Instrument {
	var out []Instrument
	for _, i := range r.instruments {
		if !i.Tradable {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Symbol < out[b].Symbol })
	return out
}

// Build classifies the configured universe. The broker's asset
// catalogue supplies the metadata (exchange, tradability, fractional,
// shortable, and whether a name is a fund); a symbol the catalogue does
// not know is still admitted with the class it was configured under, so
// a typo cannot silently drop an instrument from the universe — it
// simply reports as not tradable.
func Build(ctx context.Context, c *tools.Client, spec Spec) (*Registry, error) {
	var catalogue map[string]tools.Asset
	if c != nil {
		var err error
		catalogue, err = c.GetAssets(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("asset catalogue: %w", err)
		}
	}
	return BuildWithCatalogue(catalogue, spec), nil
}

// BuildWithCatalogue classifies a universe against an already-fetched
// asset catalogue. Callers that hold the catalogue (or tests) use this
// directly so the classification does not depend on the network.
func BuildWithCatalogue(catalogue map[string]tools.Asset, spec Spec) *Registry {
	r := &Registry{bySymbol: map[string]Instrument{}}

	add := func(sym string, class Class, window string) {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" {
			return
		}
		if class == ClassCrypto {
			sym = tools.CryptoDataSymbol(sym)
		}
		inst := Instrument{
			Symbol:    sym,
			Class:     class,
			Window:    window,
			Tradable:  true,
			Source:    "config",
			Shortable: false,
		}
		switch class {
		case ClassCrypto:
			inst.Family = FamilyCrypto
			inst.Exchange = "CRYPTO"
		case ClassOption:
			inst.Family = FamilyOptions
		default:
			inst.Family = FamilyStocks
		}
		if a, ok := catalogue[strings.ToUpper(sym)]; ok {
			inst.AssetID = a.ID
			inst.Exchange = a.Exchange
			inst.Name = a.Name
			inst.Tradable = a.Tradable
			inst.Fractional = a.Fractional
			inst.Shortable = a.Shortable
			if a.AssetClass == tools.AssetClassOption {
				inst.Class = ClassOption
				inst.Family = FamilyOptions
			}
		} else if class != ClassCrypto {
			// The broker's catalogue does not know this symbol, so its
			// tradability cannot be confirmed. Crypto is exempt: the
			// asset catalogue covers equities and options only, so every
			// crypto pair would otherwise look untradable.
			inst.Tradable = false
		}
		if _, exists := r.bySymbol[sym]; exists {
			return
		}
		r.bySymbol[sym] = inst
		r.instruments = append(r.instruments, inst)
	}

	for _, s := range spec.Equities {
		add(s, ClassEquity, "equity")
	}
	for _, s := range spec.ETFs {
		add(s, ClassETF, "equity")
	}
	for _, s := range spec.Indices {
		add(s, ClassIndex, "equity")
	}
	for _, s := range spec.Crypto {
		add(s, ClassCrypto, "crypto")
	}
	return r
}

// AddOption records a concrete option contract in the registry.
func (r *Registry) AddOption(inst Instrument) {
	inst.Class = ClassOption
	inst.Family = FamilyOptions
	inst.Window = "equity"
	inst.Source = "option-chain"
	if _, exists := r.bySymbol[inst.Symbol]; exists {
		return
	}
	r.bySymbol[inst.Symbol] = inst
	r.instruments = append(r.instruments, inst)
}

// OptionLegs expands option underlyings into concrete contracts,
// keeping the contract closest to the middle of the delta band within
// the expiry window. It returns one leg per underlying.
func OptionLegs(ctx context.Context, c *tools.Client, spec Spec) ([]Instrument, error) {
	if c == nil || len(spec.OptionUnderlyings) == 0 {
		return nil, nil
	}
	oc := options.NewClient(c)
	minDTE, maxDTE := spec.OptionMinDTE, spec.OptionMaxDTE
	if minDTE <= 0 {
		minDTE = 14
	}
	if maxDTE <= minDTE {
		maxDTE = minDTE + 30
	}
	deltaMin, deltaMax := spec.OptionDeltaMin, spec.OptionDeltaMax
	if deltaMin <= 0 {
		deltaMin = 0.55
	}
	if deltaMax <= deltaMin {
		deltaMax = 0.75
	}
	today := timeNowUTC()
	gte := today.AddDate(0, 0, minDTE).Format("2006-01-02")
	lte := today.AddDate(0, 0, maxDTE).Format("2006-01-02")

	var out []Instrument
	for _, under := range spec.OptionUnderlyings {
		under = strings.ToUpper(strings.TrimSpace(under))
		if under == "" {
			continue
		}
		contracts, err := oc.GetAllContracts(ctx, options.ContractsQuery{
			UnderlyingSymbols: []string{under},
			ExpirationDateGTE: gte,
			ExpirationDateLTE: lte,
			Type:              "call",
			Status:            "active",
		})
		if err != nil {
			return nil, fmt.Errorf("option chain %s: %w", under, err)
		}
		if len(contracts) == 0 {
			continue
		}
		snaps, err := oc.ChainSnapshots(ctx, under)
		if err != nil {
			return nil, fmt.Errorf("option snapshots %s: %w", under, err)
		}
		best, ok := pickContract(contracts, snaps, deltaMin, deltaMax)
		if !ok {
			continue
		}
		out = append(out, best)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

// pickContract selects the contract whose absolute delta sits closest
// to the middle of the band. Ties break toward the shorter expiry, so
// the least time decay is paid for the same delta.
func pickContract(contracts []options.Contract, snaps map[string]options.Snapshot, deltaMin, deltaMax float64) (Instrument, bool) {
	target := (deltaMin + deltaMax) / 2
	var best Instrument
	bestDist := 1e9
	found := false
	for _, c := range contracts {
		if !c.Tradable || c.Status != "active" {
			continue
		}
		snap, ok := snaps[c.Symbol]
		if !ok {
			continue
		}
		delta := snap.Greeks.Delta
		if delta <= 0 {
			continue
		}
		if delta < deltaMin || delta > deltaMax {
			continue
		}
		dist := delta - target
		if dist < 0 {
			dist = -dist
		}
		if !found || dist < bestDist {
			best = Instrument{
				Symbol:     c.Symbol,
				Underlying: c.UnderlyingSymbol,
				Expiration: c.ExpirationDate,
				Strike:     c.StrikePrice(),
				OptionType: c.Type,
				Delta:      delta,
				ImpliedVol: snap.ImpliedVolatility,
				Premium:    snap.MidQuote(),
				Tradable:   true,
				Shortable:  false,
				Fractional: false,
				Source:     "option-chain",
				Exchange:   "OPRA",
				Family:     FamilyOptions,
				Class:      ClassOption,
				Window:     "equity",
			}
			bestDist = dist
			found = true
		}
	}
	return best, found
}
