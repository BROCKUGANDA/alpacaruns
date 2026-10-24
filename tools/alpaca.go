// Package tools wraps the Alpaca Trading and Market Data APIs as ADK
// function tools. All order placement goes to whatever ALPACA_BASE_URL
// points at — paper by default.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client is a minimal typed Alpaca REST client.
type Client struct {
	KeyID   string
	Secret  string
	BaseURL string // trading API
	DataURL string // market data API
	HTTP    *http.Client
	Log     func(format string, args ...any) // structured call log
}

func NewClient(keyID, secret, baseURL, dataURL string) *Client {
	return &Client{
		KeyID:   keyID,
		Secret:  secret,
		BaseURL: baseURL,
		DataURL: dataURL,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
		Log:     func(string, ...any) {},
	}
}

// DataRoot returns the market-data host root (scheme + host) with any
// version path segment stripped: "https://data.alpaca.markets". Alpaca
// serves equities/ETFs under /v2, crypto under /v1beta3/crypto/us and
// options under /v1beta1/options — all on the same host — so the version
// has to be re-attached per product family instead of being baked into
// the configured DataURL.
func (c *Client) DataRoot() string {
	root := strings.TrimSuffix(c.DataURL, "/")
	for _, suffix := range []string{"/v2", "/v1", "/v1beta1", "/v1beta2", "/v1beta3"} {
		if strings.HasSuffix(root, suffix) {
			return strings.TrimSuffix(root, suffix)
		}
	}
	return root
}

// CryptoDataURL returns the crypto market-data root
// (e.g. https://data.alpaca.markets/v1beta3/crypto/us).
func (c *Client) CryptoDataURL() string {
	return c.DataRoot() + "/v1beta3/crypto/us"
}

// OptionsDataURL returns the options market-data root
// (e.g. https://data.alpaca.markets/v1beta1/options).
func (c *Client) OptionsDataURL() string {
	return c.DataRoot() + "/v1beta1/options"
}

// TradingRoot returns the trading API root with the /v2 version segment
// stripped (e.g. https://paper-api.alpaca.markets), so versioned
// sub-resources can be addressed explicitly.
func (c *Client) TradingRoot() string {
	root := strings.TrimSuffix(c.BaseURL, "/")
	if strings.HasSuffix(root, "/v2") {
		return strings.TrimSuffix(root, "/v2")
	}
	return root
}

func (c *Client) do(ctx context.Context, method, rawURL string, query url.Values, body any, out any) error {
	u := rawURL
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("APCA-API-KEY-ID", c.KeyID)
	req.Header.Set("APCA-API-SECRET-KEY", c.Secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.Log("alpaca %s %s -> %d (%s)", method, rawURL, resp.StatusCode, time.Since(start))
	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("alpaca %s %s: HTTP %d: %s", method, rawURL, resp.StatusCode, buf.String())
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Do issues one authenticated Alpaca API request through the shared
// transport. Exported so wrapper packages (options/) reuse auth headers,
// logging and error surfacing instead of duplicating them.
func (c *Client) Do(ctx context.Context, method, rawURL string, query url.Values, body any, out any) error {
	return c.do(ctx, method, rawURL, query, body, out)
}

// IsOCCSymbol reports whether s is an OCC-format option contract symbol,
// e.g. AAPL240119C00100000: 1-6 letter root, YYMMDD, C/P, 8-digit strike
// (price x1000). Used to route option orders through option-specific
// validations and risk math.
func IsOCCSymbol(s string) bool {
	return occRe.MatchString(strings.ToUpper(strings.TrimSpace(s)))
}

// IsCryptoSymbol reports whether s is a crypto BASE/QUOTE pair such as
// "BTC/USD". Crypto trades 24/7 on Alpaca, so risk-gate market-clock
// checks must be skipped for these symbols.
//
// Alpaca's ORDER endpoints need the pair without the slash ("BTCUSD") while
// market-data endpoints need it with the slash ("BTC/USD"). These two
// helpers are the only place that conversion is spelled out.
func IsCryptoSymbol(s string) bool {
	return strings.Contains(s, "/")
}

// CryptoOrderSymbol converts a BASE/QUOTE pair ("BTC/USD") to the form
// Alpaca's trading API expects ("BTCUSD"). Symbols that are already in
// trading form are returned unchanged.
func CryptoOrderSymbol(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "/", ""))
}

// CryptoDataSymbol converts a trading-form symbol ("BTCUSD") back to the
// BASE/QUOTE form the market-data endpoints expect ("BTC/USD"). Only the
// five assets Alpaca lists as USD-quoted crypto pairs are split; anything
// else is returned unchanged so equity symbols are never mangled.
func CryptoDataSymbol(s string) string {
	t := strings.ToUpper(strings.TrimSpace(s))
	if strings.Contains(t, "/") {
		return t
	}
	for _, pair := range []string{"BTC/USD", "ETH/USD", "SOL/USD", "DOGE/USD", "AVAX/USD"} {
		if strings.ReplaceAll(pair, "/", "") == t {
			return pair
		}
	}
	return t
}

var occRe = regexp.MustCompile(`^[A-Z]{1,6}\d{6}[CP]\d{8}$`)

// SameSymbol reports whether two venue symbols refer to the same
// instrument. Alpaca formats crypto differently across surfaces — the
// trading API returns "BTCUSD", the market-data API wants "BTC/USD" —
// so the comparison ignores the separator and case. Equity and option
// symbols are compared exactly.
func SameSymbol(a, b string) bool {
	na := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(a), "/", ""))
	nb := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(b), "/", ""))
	return na != "" && na == nb
}

// AssetClass mirrors Alpaca's asset_class vocabulary.
const (
	AssetClassEquity  = "us_equity"
	AssetClassOption  = "us_option"
	AssetClassCrypto  = "crypto"
	AssetClassUnknown = "unknown"
)

// ---- Account & positions ----

type Account struct {
	ID               string `json:"id"`
	Equity           string `json:"equity"`
	Cash             string `json:"cash"`
	BuyingPower      string `json:"buying_power"`
	PortfolioValue   string `json:"portfolio_value"`
	Status           string `json:"status"`
	PatternDayTrader bool   `json:"pattern_day_trader"`
	// Additional fields surfaced for the dashboard API. Alpaca's
	// /v2/account returns them all; the dashboard reads only the
	// ones it needs.
	AccountNumber string    `json:"account_number"`
	Multiplier    string    `json:"multiplier"`
	CreatedAt     time.Time `json:"created_at"`
	LastEquity    string    `json:"last_equity"`
}

type Position struct {
	Symbol       string `json:"symbol"`
	Qty          string `json:"qty"`
	Side         string `json:"side"`
	AvgEntry     string `json:"avg_entry_price"`
	CurrentPrice string `json:"current_price"`
	MarketValue  string `json:"market_value"`
	// Alpaca's /positions endpoint returns numeric fields as JSON
	// strings (same convention as avg_entry_price). Keep these as
	// strings and convert in callers to keep the wire format
	// consistent with the rest of the Position fields.
	UnrealizedPL   string `json:"unrealized_pl"`
	UnrealizedPLPC string `json:"unrealized_plpc"`
	// Dashboard API fields (newer; Alpaca added them to /positions).
	ChangeToday string `json:"change_today"`
	// AssetClass distinguishes us_equity / us_option / crypto positions
	// so callers can apply the right sizing and TP/SL rules.
	AssetClass string `json:"asset_class"`
	// QtyAvailable is the sellable quantity (options and marginable
	// positions report less than the total qty).
	QtyAvailable string `json:"qty_available"`
}

func (c *Client) GetAccount(ctx context.Context) (*Account, error) {
	var a Account
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/account", nil, nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// paperAccountPrefix is the prefix Alpaca assigns to paper-trading account
// ids. Live account ids are UUIDs.
const paperAccountPrefix = "PA"

// VerifyVenue proves that the credentials resolve to the venue the base URL
// claims, and returns the account it checked.
//
// The existing guard only inspects the URL string: a bot configured with
// ALPACA_BASE_URL=https://paper-api.alpaca.markets/v2 and LIVE keys passes
// every check in the process while trading the live account under a paper
// label. "The string looks like paper" and "the account IS paper" are
// different claims, and only the second one is worth relying on. This reads
// the account Alpaca actually resolves the credentials to and refuses when
// it does not look like a paper account.
//
// It fails closed: an unreadable or unidentified account is not proof of
// anything.
func (c *Client) VerifyVenue(ctx context.Context, expectPaper bool) (*Account, error) {
	if c == nil {
		return nil, fmt.Errorf("venue verification: no client")
	}
	a, err := c.GetAccount(ctx)
	if err != nil {
		return nil, fmt.Errorf("venue verification: account unreadable: %w", err)
	}
	if !expectPaper {
		return a, nil // live mode is gated by the URL + I_ALPACA_LIVE ack
	}
	id := strings.ToUpper(strings.TrimSpace(a.ID))
	num := strings.ToUpper(strings.TrimSpace(a.AccountNumber))
	switch {
	case id == "" && num == "":
		return a, fmt.Errorf("venue verification: account has neither id nor account_number; cannot prove it is a paper account")
	case strings.HasPrefix(id, paperAccountPrefix), strings.HasPrefix(num, paperAccountPrefix):
		return a, nil
	default:
		return a, fmt.Errorf("venue verification: the base URL claims paper trading but the credentials resolve to account %q (number %q), which does not look like a paper account — refusing to trade",
			a.ID, a.AccountNumber)
	}
}

// ExpectPaperVenue reports whether a trading base URL claims the paper
// venue. Mirrors the checks in config and the MCP child environment so all
// three agree on what "paper" means.
func ExpectPaperVenue(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "paper-api")
}

// GetAccountRawField returns one extra field on the /v2/account
// response (e.g. last_equity). Used by the dashboard API to compute
// day P/L. Returns "" + nil when the field is absent; callers fall
// back to a zero value.
func (c *Client) GetAccountRawField(ctx context.Context, field string) (string, error) {
	var raw map[string]any
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/account", nil, nil, &raw); err != nil {
		return "", err
	}
	if v, ok := raw[field]; ok {
		switch x := v.(type) {
		case string:
			return x, nil
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64), nil
		}
	}
	return "", nil
}
func (c *Client) GetPositions(ctx context.Context) ([]Position, error) {
	var ps []Position
	err := c.do(ctx, http.MethodGet, c.BaseURL+"/positions", nil, nil, &ps)
	return ps, err
}

// ---- Market data ----

// Bar is one OHLCV bar. Time is unix nanos. The decoder accepts Alpaca's
// RFC3339 string timestamps (v2 stocks REST) as well as numeric epochs
// (v1beta3 crypto) via barsResponse.
type Bar struct {
	Time   int64   `json:"t"`
	Open   float64 `json:"o"`
	High   float64 `json:"h"`
	Low    float64 `json:"l"`
	Close  float64 `json:"c"`
	Volume int64   `json:"v"`
}

func (b *Bar) UnmarshalJSON(data []byte) error {
	type alias struct {
		T      json.RawMessage `json:"t"`
		Open   float64         `json:"o"`
		High   float64         `json:"h"`
		Low    float64         `json:"l"`
		Close  float64         `json:"c"`
		Volume json.RawMessage `json:"v"`
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	b.Open, b.High, b.Low, b.Close = a.Open, a.High, a.Low, a.Close
	// Timestamp: RFC3339 string or numeric epoch (nanos or seconds).
	if len(a.T) > 0 && a.T[0] == '"' {
		var s string
		if err := json.Unmarshal(a.T, &s); err == nil {
			if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
				b.Time = ts.UnixNano()
			}
		}
	} else if len(a.T) > 0 {
		var n int64
		if err := json.Unmarshal(a.T, &n); err == nil {
			b.Time = n
		}
	}
	// Volume: int64 or float (crypto feeds return fractional base volume).
	if len(a.Volume) > 0 {
		var f float64
		if err := json.Unmarshal(a.Volume, &f); err == nil {
			b.Volume = int64(f)
		}
	}
	return nil
}

type barsResponse struct {
	Bars map[string][]Bar `json:"bars"`
	// NextPageToken is Alpaca's pagination cursor. It was previously
	// dropped, which silently truncated every history to the first page.
	NextPageToken string `json:"next_page_token"`
}

// maxBarPages bounds the pagination loop so a pathological cursor chain
// cannot spin forever.
const maxBarPages = 20

func (c *Client) GetBars(ctx context.Context, symbols []string, timeframe, start, end string, limit int) (map[string][]Bar, error) {
	q := url.Values{}
	q.Set("symbols", join(symbols))
	q.Set("timeframe", orDefaultStr(timeframe, "1Day"))
	// Split-adjusted by default. Alpaca's documented default is `raw`, which
	// makes a 10-for-1 split look like a 90% one-day collapse: the trend and
	// momentum factors read it as a crash, and ATR-scaled stops widen to the
	// size of the artefact. Dividends are deliberately NOT adjusted in:
	// price-series continuity is what these factors need, and `all` also
	// rewrites history for payouts, which changes the levels a decision was
	// made against.
	if q.Get("adjustment") == "" {
		q.Set("adjustment", "split")
	}
	if start == "" {
		// Alpaca returns an EMPTY page when `start` is omitted — inject
		// a ~8-month daily lookback by default (same workaround as
		// strategy/bars.go). Callers passing explicit ranges are untouched.
		start = time.Now().AddDate(0, -8, 0).Format("2006-01-02")
	}
	q.Set("start", start)
	if end != "" {
		q.Set("end", end)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	out := map[string][]Bar{}
	for page := 0; page < maxBarPages; page++ {
		var br barsResponse
		if err := c.do(ctx, http.MethodGet, c.DataURL+"/stocks/bars", q, nil, &br); err != nil {
			return nil, err
		}
		for sym, bars := range br.Bars {
			out[sym] = append(out[sym], bars...)
		}
		if br.NextPageToken == "" {
			break
		}
		q.Set("page_token", br.NextPageToken)
	}
	return out, nil
}

// GetCryptoBars fetches crypto daily/intraday bars from the crypto
// market-data endpoint. Alpaca serves crypto on a different path family
// than equities: /v1beta3/crypto/us/bars, with BASE/QUOTE symbols.
func (c *Client) GetCryptoBars(ctx context.Context, symbols []string, timeframe, start, end string, limit int) (map[string][]Bar, error) {
	if len(symbols) == 0 {
		return nil, nil
	}
	conv := make([]string, 0, len(symbols))
	for _, s := range symbols {
		conv = append(conv, CryptoDataSymbol(s))
	}
	q := url.Values{}
	q.Set("symbols", join(conv))
	q.Set("timeframe", orDefaultStr(timeframe, "1Day"))
	if start == "" {
		// Without an explicit start Alpaca returns only the current bar,
		// which is not enough history to score anything. Use the same
		// ~8-month daily lookback the equities path uses.
		start = time.Now().AddDate(0, -8, 0).Format("2006-01-02")
	}
	q.Set("start", start)
	if end != "" {
		q.Set("end", end)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	out := map[string][]Bar{}
	for page := 0; page < maxBarPages; page++ {
		var br barsResponse
		if err := c.do(ctx, http.MethodGet, c.CryptoDataURL()+"/bars", q, nil, &br); err != nil {
			return nil, err
		}
		for sym, bars := range br.Bars {
			// Key the result by the symbol the caller asked for, not by
			// the venue's form: a caller that asked for "BTC/USD" must
			// find its bars under "BTC/USD".
			out[cryptoKeyFor(sym, symbols)] = append(out[cryptoKeyFor(sym, symbols)], bars...)
		}
		if br.NextPageToken == "" {
			break
		}
		q.Set("page_token", br.NextPageToken)
	}
	return out, nil
}

// cryptoKeyFor maps a venue crypto symbol back to the form the caller
// used, so the returned map is keyed the same way the request was.
func cryptoKeyFor(venueSymbol string, requested []string) string {
	venue := CryptoOrderSymbol(venueSymbol)
	for _, r := range requested {
		if CryptoOrderSymbol(r) == venue {
			return r
		}
	}
	return venueSymbol
}

// CryptoSnapshot is one crypto market snapshot. The crypto endpoint
// answers with a symbol-keyed map of nested objects.
type CryptoSnapshot struct {
	Symbol         string  `json:"-"`
	LatestTrade    float64 `json:"-"`
	BidPrice       float64 `json:"-"`
	AskPrice       float64 `json:"-"`
	DailyBarClose  float64 `json:"-"`
	PrevDailyClose float64 `json:"-"`
}

type rawCryptoSnapshot struct {
	LatestTrade struct {
		P float64 `json:"p"`
	} `json:"latestTrade"`
	LatestQuote struct {
		BP float64 `json:"bp"`
		AP float64 `json:"ap"`
	} `json:"latestQuote"`
	DailyBar struct {
		C float64 `json:"c"`
	} `json:"dailyBar"`
	PrevDailyBar struct {
		C float64 `json:"c"`
	} `json:"prevDailyBar"`
}

// GetCryptoSnapshots fetches crypto snapshots keyed by trading-form
// symbol ("BTCUSD").
func (c *Client) GetCryptoSnapshots(ctx context.Context, symbols []string) (map[string]CryptoSnapshot, error) {
	if len(symbols) == 0 {
		return nil, nil
	}
	conv := make([]string, 0, len(symbols))
	for _, s := range symbols {
		conv = append(conv, CryptoDataSymbol(s))
	}
	q := url.Values{"symbols": {join(conv)}}
	var wrapper struct {
		Snapshots map[string]rawCryptoSnapshot `json:"snapshots"`
	}
	if err := c.do(ctx, http.MethodGet, c.CryptoDataURL()+"/snapshots", q, nil, &wrapper); err != nil {
		return nil, err
	}
	out := make(map[string]CryptoSnapshot, len(wrapper.Snapshots))
	for sym, rs := range wrapper.Snapshots {
		key := cryptoKeyFor(sym, symbols)
		out[key] = CryptoSnapshot{
			Symbol:         key,
			LatestTrade:    rs.LatestTrade.P,
			BidPrice:       rs.LatestQuote.BP,
			AskPrice:       rs.LatestQuote.AP,
			DailyBarClose:  rs.DailyBar.C,
			PrevDailyClose: rs.PrevDailyBar.C,
		}
	}
	return out, nil
}

// Asset is one entry from Alpaca's /v2/assets endpoint. It carries the
// asset class the picker needs to route a symbol to the right data
// family and order rules.
//
// Alpaca names the fields "class" and "fractionable" (not "asset_class"
// and "fractional_tradable"), so the JSON tags below match the wire
// format exactly.
type Asset struct {
	ID          string `json:"id"`
	Symbol      string `json:"symbol"`
	Name        string `json:"name"`
	Exchange    string `json:"exchange"`
	AssetClass  string `json:"class"`
	Status      string `json:"status"`
	Tradable    bool   `json:"tradable"`
	Fractional  bool   `json:"fractionable"`
	Marginable  bool   `json:"marginable"`
	Shortable   bool   `json:"shortable"`
	MinOrderQty string `json:"min_order_qty"`
}

// IsETF reports whether the asset looks like an exchange-traded fund.
// Alpaca classifies ETFs as plain us_equity, so the only signal
// available is the fund's name.
func (a Asset) IsETF() bool {
	n := strings.ToUpper(a.Name)
	return strings.Contains(n, "ETF") || strings.Contains(n, "TRUST") ||
		strings.Contains(n, "FUND") || strings.Contains(n, "INDEX")
}

// GetAssets returns asset metadata for the given symbols. Alpaca's
// /v2/assets endpoint IGNORES a symbols filter and always answers with
// the full catalogue, so the request fetches the catalogue and filters
// locally. Callers that need this repeatedly should cache the result.
func (c *Client) GetAssets(ctx context.Context, symbols []string) (map[string]Asset, error) {
	all, err := c.ListAssets(ctx)
	if err != nil {
		return nil, err
	}
	if len(symbols) == 0 {
		return all, nil
	}
	out := make(map[string]Asset, len(symbols))
	for _, s := range symbols {
		if a, ok := all[strings.ToUpper(strings.TrimSpace(s))]; ok {
			out[strings.ToUpper(strings.TrimSpace(s))] = a
		}
	}
	return out, nil
}

// ListAssets returns the full asset catalogue keyed by symbol.
//
// Alpaca's /v2/assets endpoint IGNORES limit and offset — it always
// answers with the whole catalogue (33k assets, ~14.7k active) — so the
// catalogue is fetched in ONE request. A paging loop over it re-fetches
// the same payload over and over, which is how a single "list my
// instruments" call turned into 50 half-second round trips.
//
// Callers that need this repeatedly should cache the result.
func (c *Client) ListAssets(ctx context.Context) (map[string]Asset, error) {
	q := url.Values{"status": {"active"}}
	var raw []Asset
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/assets", q, nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]Asset, len(raw))
	for _, a := range raw {
		out[strings.ToUpper(a.Symbol)] = a
	}
	return out, nil
}

type Quote struct {
	BidPrice float64 `json:"bp"`
	AskPrice float64 `json:"ap"`
	BidSize  int     `json:"bs"`
	AskSize  int     `json:"as"`
	// Timestamp is the quote time. Alpaca's v2 stocks REST returns it as
	// an RFC3339 string while the crypto feeds return numeric epochs, so
	// it is decoded through a permissive unmarshaller instead of a plain
	// int64 field (which failed to decode on every equities response).
	Timestamp int64 `json:"-"`
}

// UnmarshalJSON accepts both the RFC3339 string and the numeric epoch
// forms Alpaca uses for quote timestamps.
func (q *Quote) UnmarshalJSON(data []byte) error {
	type alias struct {
		BidPrice float64         `json:"bp"`
		AskPrice float64         `json:"ap"`
		BidSize  int             `json:"bs"`
		AskSize  int             `json:"as"`
		TS       json.RawMessage `json:"t"`
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	q.BidPrice, q.AskPrice, q.BidSize, q.AskSize = a.BidPrice, a.AskPrice, a.BidSize, a.AskSize
	q.Timestamp = parseFlexibleTimestamp(a.TS)
	return nil
}

// parseFlexibleTimestamp decodes an Alpaca timestamp that may be an
// RFC3339 string or a numeric epoch (seconds or nanoseconds).
func parseFlexibleTimestamp(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0
		}
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts.UnixNano()
		}
		return 0
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0
	}
	return n
}

type quotesResponse struct {
	Quotes map[string]Quote `json:"quotes"`
}

func (c *Client) GetQuotes(ctx context.Context, symbols []string) (map[string]Quote, error) {
	q := url.Values{"symbols": {join(symbols)}}
	var qr quotesResponse
	if err := c.do(ctx, http.MethodGet, c.DataURL+"/stocks/quotes/latest", q, nil, &qr); err != nil {
		return nil, err
	}
	return qr.Quotes, nil
}

// Snapshot is one /stocks/snapshots entry. The price fields live inside
// nested objects, so they are decoded with nested structs — the previous
// dotted json tags ("latestTrade.p") were silently ignored by
// encoding/json, leaving every price at zero.
type Snapshot struct {
	LatestTradePrice float64 `json:"-"`
	LatestQuoteBid   float64 `json:"-"`
	LatestQuoteAsk   float64 `json:"-"`
	DailyBar         Bar     `json:"dailyBar"`
	PrevDailyBar     Bar     `json:"prevDailyBar"`
}

type rawSnapshotStock struct {
	LatestTrade struct {
		P float64 `json:"p"`
	} `json:"latestTrade"`
	LatestQuote struct {
		BP float64 `json:"bp"`
		AP float64 `json:"ap"`
	} `json:"latestQuote"`
	DailyBar     Bar `json:"dailyBar"`
	PrevDailyBar Bar `json:"prevDailyBar"`
}

func (s *Snapshot) UnmarshalJSON(data []byte) error {
	var raw rawSnapshotStock
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.LatestTradePrice = raw.LatestTrade.P
	s.LatestQuoteBid = raw.LatestQuote.BP
	s.LatestQuoteAsk = raw.LatestQuote.AP
	s.DailyBar = raw.DailyBar
	s.PrevDailyBar = raw.PrevDailyBar
	return nil
}

// MidPrice returns the most usable price in the snapshot: the last trade,
// else the quote midpoint, else the daily bar close.
func (s Snapshot) MidPrice() float64 {
	if s.LatestTradePrice > 0 {
		return s.LatestTradePrice
	}
	if s.LatestQuoteBid > 0 && s.LatestQuoteAsk > 0 {
		return (s.LatestQuoteBid + s.LatestQuoteAsk) / 2
	}
	if s.DailyBar.Close > 0 {
		return s.DailyBar.Close
	}
	return 0
}

func (c *Client) GetSnapshots(ctx context.Context, symbols []string) (map[string]Snapshot, error) {
	q := url.Values{"symbols": {join(symbols)}}
	var out map[string]Snapshot
	if err := c.do(ctx, http.MethodGet, c.DataURL+"/stocks/snapshots", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type NewsItem struct {
	ID        int64     `json:"id"`
	Headline  string    `json:"headline"`
	Summary   string    `json:"summary"`
	Source    string    `json:"source"`
	Published time.Time `json:"created_at"`
	Symbols   []string  `json:"symbols"`
}

func (c *Client) GetNews(ctx context.Context, symbols []string, limit int) ([]NewsItem, error) {
	q := url.Values{}
	if len(symbols) > 0 {
		q.Set("symbols", join(symbols))
	}
	q.Set("limit", strconv.Itoa(orDefault(limit, 10)))
	var out struct {
		News []NewsItem `json:"news"`
	}
	if err := c.do(ctx, http.MethodGet, c.DataURL+"/news", q, nil, &out); err != nil {
		return nil, err
	}
	return out.News, nil
}

// ---- Orders ----

type OrderRequest struct {
	Symbol      string `json:"symbol"`
	Qty         string `json:"qty,omitempty"`
	Notional    string `json:"notional,omitempty"`
	Side        string `json:"side"` // buy | sell
	Type        string `json:"type"` // market | limit | stop
	TimeInForce string `json:"time_in_force"`
	LimitPrice  string `json:"limit_price,omitempty"`
	StopPrice   string `json:"stop_price,omitempty"`
	// ExtendedHours requests execution outside regular trading hours.
	// Per the Alpaca POST /v2/orders contract this is honored ONLY for
	// type=limit orders with time_in_force day or gtc; PlaceOrder enforces
	// that constraint client-side so bad combos never reach the API.
	ExtendedHours bool   `json:"extended_hours,omitempty"`
	ClientOrderID string `json:"client_order_id,omitempty"`
}

type Order struct {
	ID             string    `json:"id"`
	Symbol         string    `json:"symbol"`
	Qty            string    `json:"qty"`
	FilledQty      string    `json:"filled_qty"`
	Side           string    `json:"side"`
	Type           string    `json:"type"`
	Status         string    `json:"status"`
	FilledAvgPrice string    `json:"filled_avg_price"`
	CreatedAt      time.Time `json:"created_at"`
	// AssetClass, Notional and OrderClass let the reconciler tell a
	// crypto fill from an equity bracket and a notional order from a
	// share order.
	AssetClass string `json:"asset_class"`
	Notional   string `json:"notional"`
	OrderClass string `json:"order_class"`
	// LimitPrice and StopPrice are the protective levels actually
	// resting on an open order. Without them the monitor cannot tell
	// whether a resting pair still matches the position's real entry,
	// so a re-anchored position would keep protecting at the old prices.
	LimitPrice string `json:"limit_price"`
	StopPrice  string `json:"stop_price"`
	// ClientOrderID is the local identifier the order was submitted with.
	// It is what lets a fill that arrives AFTER the submitting response be
	// traced back to the intent that created it — and therefore to the
	// score that justified the trade. Without it the reconciliation pass can
	// recover the money but not the evidence.
	ClientOrderID string `json:"client_order_id"`
	// GroupID and GroupRole identify bracket/OCO CHILD orders. Alpaca only
	// returns them under status=all: a `stop_loss` leg sits in status
	// "held" until its entry fills, so a listing that omits grouped orders
	// silently hides the protection the bot believes is resting.
	GroupID   string `json:"group_id"`
	GroupRole string `json:"group_role"`
	// PositionIntent distinguishes sell_to_close from sell_to_open. A
	// reducing order and a short-sale instruction look identical in every
	// other field, and treating the second as the first is how a bot
	// opens exposure it never intended.
	PositionIntent string    `json:"position_intent"`
	SubmittedAt    time.Time `json:"submitted_at"`
}

// PlaceOrder submits an order, defaulting time_in_force to gtc when unset.
// It enforces the Alpaca constraint that extended_hours=true is valid only
// for type=limit orders with day or gtc time in force — rejecting invalid
// combinations client-side so they never reach the API. Option orders
// (OCC-format symbol) additionally enforce Alpaca's options rules:
// whole-number qty, no notional, time_in_force day|gtc only, and
// extended_hours must be false.
func (c *Client) PlaceOrder(ctx context.Context, req OrderRequest) (*Order, error) {
	if req.TimeInForce == "" {
		req.TimeInForce = "gtc"
	}
	tif := strings.ToLower(req.TimeInForce)
	if req.ExtendedHours {
		if !strings.EqualFold(req.Type, "limit") {
			return nil, fmt.Errorf("extended_hours requires type=limit, got type=%q", req.Type)
		}
		if tif != "day" && tif != "gtc" {
			return nil, fmt.Errorf("extended_hours requires time_in_force=day or gtc, got %q", req.TimeInForce)
		}
	}
	if IsOCCSymbol(req.Symbol) {
		if strings.TrimSpace(req.Notional) != "" {
			return nil, fmt.Errorf("options orders must not set notional; use qty of contracts")
		}
		if req.ExtendedHours {
			return nil, fmt.Errorf("options orders do not support extended_hours")
		}
		if tif != "day" && tif != "gtc" {
			return nil, fmt.Errorf("options orders require time_in_force=day or gtc, got %q", req.TimeInForce)
		}
		if q, err := strconv.ParseFloat(strings.TrimSpace(req.Qty), 64); err != nil || q <= 0 || q != math.Trunc(q) {
			return nil, fmt.Errorf("options orders require a positive whole-number qty, got %q", req.Qty)
		}
	}
	var o Order
	if err := c.do(ctx, http.MethodPost, c.BaseURL+"/orders", nil, req, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// OCOClose describes a protective pair placed against a position that
// is ALREADY open. Alpaca's `oco` order class is a bracket without an
// entry leg: a take-profit limit and a stop-loss, where filling either
// one cancels the other. It exists precisely for the case a bot's own
// entry orders do not cover — a position the bot did not open, or one
// whose bracket was never placed.
type OCOClose struct {
	Symbol      string
	Qty         string
	Side        string // "sell" closes a long, "buy" closes a short
	TimeInForce string // day | gtc
	TakeProfit  float64
	StopLoss    float64
	// ClientOrderID makes the placement idempotent: a retry after a
	// timeout is rejected by the broker instead of stacking a second
	// protective pair on the same position.
	ClientOrderID string
}

// PlaceOCOClose submits the protective pair. The payload shape is the
// one Alpaca accepts: verified against the paper API, which rejects an
// oco order whose take-profit is expressed as the parent limit price
// ("oco orders require take_profit.limit_price").
func (c *Client) PlaceOCOClose(ctx context.Context, req OCOClose) (*Order, error) {
	if req.TakeProfit <= 0 || req.StopLoss <= 0 {
		return nil, fmt.Errorf("oco close for %s needs both a take-profit and a stop price, got tp=%.4f sl=%.4f",
			req.Symbol, req.TakeProfit, req.StopLoss)
	}
	tif := req.TimeInForce
	if tif == "" {
		tif = "gtc"
	}
	body := map[string]any{
		"symbol":        req.Symbol,
		"qty":           req.Qty,
		"side":          req.Side,
		"type":          "limit",
		"time_in_force": tif,
		"order_class":   "oco",
		"take_profit":   map[string]string{"limit_price": strconv.FormatFloat(req.TakeProfit, 'f', 2, 64)},
		"stop_loss":     map[string]string{"stop_price": strconv.FormatFloat(req.StopLoss, 'f', 2, 64)},
	}
	if req.ClientOrderID != "" {
		body["client_order_id"] = req.ClientOrderID
	}
	var o Order
	if err := c.do(ctx, http.MethodPost, c.BaseURL+"/orders", nil, body, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// CancelOrdersForSymbol cancels every open order on one symbol. Used
// before a locally-enforced exit fills, so a resting protective pair
// cannot fire after the position is already closed and open a short by
// accident.
func (c *Client) CancelOrdersForSymbol(ctx context.Context, symbol string) (int, error) {
	q := url.Values{"symbols": {symbol}, "status": {"open"}}
	var os []Order
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/orders", q, nil, &os); err != nil {
		return 0, err
	}
	n := 0
	var unconfirmed []string
	for _, o := range os {
		if err := c.CancelOrderConfirmed(ctx, o.ID, 2*time.Second); err != nil {
			// Report what was not confirmed instead of counting a 2xx as a
			// cancellation. The caller re-places protection; doing that on
			// top of a pair that is still resting is how a position ends up
			// with two live exits.
			unconfirmed = append(unconfirmed, o.ID+": "+err.Error())
			continue
		}
		n++
	}
	if len(unconfirmed) > 0 {
		return n, fmt.Errorf("cancel not confirmed for %d of %d order(s): %s",
			len(unconfirmed), len(os), strings.Join(unconfirmed, "; "))
	}
	return n, nil
}

func (c *Client) GetOrderStatus(ctx context.Context, orderID string) (*Order, error) {
	var o Order
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/orders/"+url.PathEscape(orderID), nil, nil, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

func (c *Client) CancelOrder(ctx context.Context, orderID string) error {
	return c.do(ctx, http.MethodDelete, c.BaseURL+"/orders/"+url.PathEscape(orderID), nil, nil, nil)
}

// CancelOrderConfirmed asks the broker to cancel an order and then verifies
// that it actually reached a terminal state.
//
// A 2xx from DELETE is an ACKNOWLEDGEMENT, not a cancellation: measured on
// the live paper account, DELETE returned 204 while the order still listed
// as "accepted". Callers that treated the status code as the outcome marked
// protective orders "cancelled" while they were still resting — and a live
// protective leg over a closed position is a short sale in waiting.
//
// The poll is bounded and short. "Not confirmed yet" is reported as an
// error rather than assumed either way.
func (c *Client) CancelOrderConfirmed(ctx context.Context, orderID string, wait time.Duration) error {
	if err := c.CancelOrder(ctx, orderID); err != nil {
		return err
	}
	if wait <= 0 {
		wait = 3 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		o, err := c.GetOrderStatus(ctx, orderID)
		if err != nil {
			return fmt.Errorf("cancel %s: confirmation read failed: %w", orderID, err)
		}
		switch strings.ToLower(strings.TrimSpace(o.Status)) {
		case "canceled", "cancelled", "expired", "rejected", "failed", "filled":
			// filled is terminal too: a cancel that raced a fill did not
			// cancel anything, but it is no longer resting either. The
			// caller sees the status and decides what to do about the fill.
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cancel %s: still %q after %s (request acknowledged, cancellation not confirmed)",
				orderID, o.Status, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ---- Clock ----

// Clock is the market clock from /v2/clock.
type Clock struct {
	Timestamp string `json:"timestamp"`
	IsOpen    bool   `json:"is_open"`
	NextOpen  string `json:"next_open"`
	NextClose string `json:"next_close"`
}

// GetClock returns the current market clock.
func (c *Client) GetClock(ctx context.Context) (*Clock, error) {
	var cl Clock
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/clock", nil, nil, &cl); err != nil {
		return nil, err
	}
	return &cl, nil
}

// ListOrders returns orders, newest first. status: open|closed|all
// (empty = all). Used by the P/L journal to backfill fills.
func (c *Client) ListOrders(ctx context.Context, status string, limit int) ([]Order, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var os []Order
	err := c.do(ctx, http.MethodGet, c.BaseURL+"/orders", q, nil, &os)
	return os, err
}

// ListRecentOrders returns every order in the broker's recent history,
// including bracket and OCO CHILD orders.
//
// status=all is not a courtesy: measured against the live paper account,
// `status=open` lists only the OCO parents, and the stop leg — the one
// that actually protects the position — is visible only as a `held` child
// under status=all. Code that reasons about protection from status=open
// alone believes a stop is missing when it is resting.
func (c *Client) ListRecentOrders(ctx context.Context, limit int) ([]Order, error) {
	q := url.Values{}
	q.Set("status", "all")
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var os []Order
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/orders", q, nil, &os); err != nil {
		return nil, err
	}
	return os, nil
}

// IsDuplicateClientOrderID reports whether err is Alpaca rejecting a
// client_order_id that already exists.
//
// Measured: a second POST with a used id returns HTTP 422 with code
// 42210000 "client_order_id must be unique". That is the broker's
// idempotency guarantee, so a caller retrying an intent whose response was
// lost must read this as "your order is already in", not as a failure —
// treating it as a failure is how a retry loop ends up placing a second
// order under a fresh id.
func IsDuplicateClientOrderID(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "42210000") ||
		(strings.Contains(s, "http 422") && strings.Contains(s, "client_order_id"))
}

// ---- helpers ----

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func orDefaultStr(s string, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orDefault(i int, def int) int {
	if i == 0 {
		return def
	}
	return i
}
