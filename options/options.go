// Package options is a typed client for Alpaca's options contracts and
// market-data endpoints. It wraps the existing tools.Client transport
// (same keys, same logging) rather than duplicating REST plumbing.
//
// Endpoints (per Alpaca's official docs):
//   - GET /v2/options/contracts            — searchable contract chain
package options

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// Contract is one option contract from /v2/options/contracts. Numeric
// fields are kept as raw strings because Alpaca returns them as JSON
// strings or numbers depending on the payload; use the accessors for
// typed values.
type Contract struct {
	ID               string `json:"id"`
	Symbol           string `json:"symbol"` // OCC format, e.g. AAPL240119C00100000
	Name             string `json:"name"`
	Status           string `json:"status"`
	Tradable         bool   `json:"tradable"`
	ExpirationDate   string `json:"expiration_date"` // YYYY-MM-DD
	RootSymbol       string `json:"root_symbol"`
	UnderlyingSymbol string `json:"underlying_symbol"`
	Type             string `json:"type"`  // call | put
	Style            string `json:"style"` // american | european
	StrikePriceRaw   string `json:"strike_price"`
	Size             string `json:"size"` // multiplier as string, "100"
	OpenInterest     string `json:"open_interest"`
	ClosePriceRaw    string `json:"close_price"`
}

// StrikePrice parses the strike; 0 on any failure.
func (c Contract) StrikePrice() float64 { return parseDefensiveFloat(c.StrikePriceRaw) }

// ClosePrice parses the previous close; 0 on any failure.
func (c Contract) ClosePrice() float64 { return parseDefensiveFloat(c.ClosePriceRaw) }

func parseDefensiveFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// contractsResponse wraps the list endpoint envelope.
type contractsResponse struct {
	Contracts []Contract `json:"option_contracts"`
	PageToken string     `json:"page_token"`
	Limit     int        `json:"limit"`
}

// ContractsQuery filters the option chain lookup. Zero values are omitted.
type ContractsQuery struct {
	UnderlyingSymbols []string // required by the API
	ExpirationDateGTE string   // YYYY-MM-DD
	ExpirationDateLTE string   // YYYY-MM-DD
	StrikePriceGTE    string
	StrikePriceLTE    string
	Type              string // call | put
	Status            string // active | expired (default active)
	Limit             int    // default 100, max 10000
	PageToken         string // continuation from a previous PageToken
}

func (q ContractsQuery) values() url.Values {
	v := url.Values{}
	if len(q.UnderlyingSymbols) > 0 {
		v.Set("underlying_symbols", strings.Join(q.UnderlyingSymbols, ","))
	}
	setStr := func(key, val string) {
		if s := strings.TrimSpace(val); s != "" {
			v.Set(key, s)
		}
	}
	setStr("expiration_date_gte", q.ExpirationDateGTE)
	setStr("expiration_date_lte", q.ExpirationDateLTE)
	setStr("strike_price_gte", q.StrikePriceGTE)
	setStr("strike_price_lte", q.StrikePriceLTE)
	setStr("type", q.Type)
	setStr("status", q.Status)
	setStr("page_token", q.PageToken)
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	return v
}

// GetContracts searches option contracts. When Status is empty it defaults
// to "active"; when Limit is empty it defaults to 100; when both
// ExpirationDate bounds are unset it caps the window at next weekend —
// mirroring Alpaca's documented defaults so ad-hoc queries stay small.
func (c *Client) GetContracts(ctx context.Context, q ContractsQuery) ([]Contract, string, error) {
	if q.Status == "" {
		q.Status = "active"
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.ExpirationDateGTE == "" && q.ExpirationDateLTE == "" {
		// Next Saturday from today, formatted YYYY-MM-DD.
		now := time.Now().UTC()
		days := (int(time.Saturday) - int(now.Weekday()) + 7) % 7
		if days == 0 {
			days = 7
		}
		q.ExpirationDateLTE = now.AddDate(0, 0, days).Format("2006-01-02")
	}
	var out contractsResponse
	if err := c.Do(ctx, http.MethodGet, c.BaseURL+"/options/contracts", q.values(), nil, &out); err != nil {
		return nil, "", err
	}
	return out.Contracts, out.PageToken, nil
}

// GetContract fetches a single contract by OCC symbol or contract ID.
func (c *Client) GetContract(ctx context.Context, symbolOrID string) (*Contract, error) {
	var out Contract
	path := c.BaseURL + "/options/contracts/" + strings.TrimSpace(symbolOrID)
	if err := c.Do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Greeks is the option greeks block of a snapshot. All fields optional;
// Alpaca omits them when no model data is available.
type Greeks struct {
	Delta float64 `json:"delta"`
	Gamma float64 `json:"gamma"`
	Theta float64 `json:"theta"`
	Vega  float64 `json:"vega"`
	Rho   float64 `json:"rho"`
}

// Trade is the latest trade block of an options snapshot.
type Trade struct {
	Price float64   `json:"p"`
	Size  float64   `json:"s"`
	Time  time.Time `json:"t"`
}

// Quote is the latest NBBO quote block of an options snapshot.
type Quote struct {
	BidPrice float64 `json:"bp"`
	BidSize  float64 `json:"bz"`
	AskPrice float64 `json:"ap"`
	AskSize  float64 `json:"as"`
}

// Snapshot is the per-contract options market snapshot.
type Snapshot struct {
	LatestTrade       Trade   `json:"latestTrade"`
	LatestQuote       Quote   `json:"latestQuote"`
	Greeks            Greeks  `json:"greeks"`
	ImpliedVolatility float64 `json:"impliedVolatility"`
}

// snapshotsResponse wraps the map-keyed snapshot payload.
type snapshotsResponse struct {
	Snapshots map[string]Snapshot `json:"snapshots"`
}

// GetSnapshots fetches option snapshots for OCC symbols via
// GET /v1beta1/options/snapshots?symbols=... .
func (c *Client) GetSnapshots(ctx context.Context, symbols []string) (map[string]Snapshot, error) {
	v := url.Values{"symbols": {strings.Join(symbols, ",")}}
	var raw struct {
		Snapshots map[string]rawSnapshot `json:"snapshots"`
	}
	if err := c.Do(ctx, http.MethodGet, c.DataURL+"/v1beta1/options/snapshots", v, nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]Snapshot, len(raw.Snapshots))
	for sym, rs := range raw.Snapshots {
		out[sym] = rs.decode()
	}
	return out, nil
}

// ChainSnapshots fetches every contract snapshot for one underlying via
// GET /v1beta1/options/snapshots/{underlying}/chain, keyed by OCC symbol.
func (c *Client) ChainSnapshots(ctx context.Context, underlying string) (map[string]Snapshot, error) {
	var raw struct {
		Snapshots map[string]rawSnapshot `json:"snapshots"`
	}
	path := c.DataURL + "/v1beta1/options/snapshots/" + url.PathEscape(strings.ToUpper(strings.TrimSpace(underlying))) + "/chain"
	if err := c.Do(ctx, http.MethodGet, path, nil, nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]Snapshot, len(raw.Snapshots))
	for sym, rs := range raw.Snapshots {
		out[sym] = rs.decode()
	}
	return out, nil
}

// rawSnapshot tolerates Alpaca returning numeric snapshot fields either
// as JSON numbers or strings ("fields are strings" defensiveness).
type rawSnapshot struct {
	LatestTrade struct {
		P any `json:"p"`
		S any `json:"s"`
		T any `json:"t"`
	} `json:"latestTrade"`
	LatestQuote struct {
		BP any `json:"bp"`
		BZ any `json:"bz"`
		AP any `json:"ap"`
		AS any `json:"as"`
	} `json:"latestQuote"`
	Greeks struct {
		Delta any `json:"delta"`
		Gamma any `json:"gamma"`
		Theta any `json:"theta"`
		Vega  any `json:"vega"`
		Rho   any `json:"rho"`
	} `json:"greeks"`
	IV any `json:"impliedVolatility"`
}

func anyFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	default:
		return 0
	}
}

func (rs rawSnapshot) decode() Snapshot {
	s := Snapshot{ImpliedVolatility: anyFloat(rs.IV)}
	s.LatestTrade.Price = anyFloat(rs.LatestTrade.P)
	s.LatestTrade.Size = anyFloat(rs.LatestTrade.S)
	if t, ok := rs.LatestTrade.T.(string); ok {
		s.LatestTrade.Time, _ = time.Parse(time.RFC3339Nano, t)
	}
	s.LatestQuote.BidPrice = anyFloat(rs.LatestQuote.BP)
	s.LatestQuote.BidSize = anyFloat(rs.LatestQuote.BZ)
	s.LatestQuote.AskPrice = anyFloat(rs.LatestQuote.AP)
	s.LatestQuote.AskSize = anyFloat(rs.LatestQuote.AS)
	s.Greeks.Delta = anyFloat(rs.Greeks.Delta)
	s.Greeks.Gamma = anyFloat(rs.Greeks.Gamma)
	s.Greeks.Theta = anyFloat(rs.Greeks.Theta)
	s.Greeks.Vega = anyFloat(rs.Greeks.Vega)
	s.Greeks.Rho = anyFloat(rs.Greeks.Rho)
	return s
}

// MidQuote returns the quote midpoint; falls back to last-trade price,
// then close price, then 0 when nothing is quotable.
func (s Snapshot) MidQuote() float64 {
	if s.LatestQuote.BidPrice > 0 && s.LatestQuote.AskPrice > 0 {
		return (s.LatestQuote.BidPrice + s.LatestQuote.AskPrice) / 2
	}
	if s.LatestTrade.Price > 0 {
		return s.LatestTrade.Price
	}
	return 0
}

// Client reuses tools.Client's transport (auth headers, logging, error
// surfacing) for the options endpoints.
type Client struct {
	c       *tools.Client
	BaseURL string // trading API root, e.g. https://paper-api.alpaca.markets/v2
	DataURL string // market data root, e.g. https://data.alpaca.markets/v1beta1
}

// NewClient adapts an existing tools.Client, deriving the trading and
// market-data roots from it.
func NewClient(c *tools.Client) *Client {
	if c == nil {
		panic("options.NewClient: nil tools.Client")
	}
	return &Client{
		c:       c,
		BaseURL: strings.TrimSuffix(c.BaseURL, "/"),
		DataURL: strings.TrimSuffix(c.DataURL, "/"),
	}
}

// Do forwards to tools.Client.Do — exported so this package (and tests)
// issue typed requests through the shared transport.
func (c *Client) Do(ctx context.Context, method, rawURL string, query url.Values, body, out any) error {
	return c.c.Do(ctx, method, rawURL, query, body, out)
}
