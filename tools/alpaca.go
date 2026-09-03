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
func IsCryptoSymbol(s string) bool {
	return strings.Contains(s, "/")
}

var occRe = regexp.MustCompile(`^[A-Z]{1,6}\d{6}[CP]\d{8}$`)

// ---- Account & positions ----

type Account struct {
	ID               string `json:"id"`
	Equity           string `json:"equity"`
	Cash             string `json:"cash"`
	BuyingPower      string `json:"buying_power"`
	PortfolioValue   string `json:"portfolio_value"`
	Status           string `json:"status"`
	PatternDayTrader bool   `json:"pattern_day_trader"`
}

type Position struct {
	Symbol         string  `json:"symbol"`
	Qty            string  `json:"qty"`
	Side           string  `json:"side"`
	AvgEntry       string  `json:"avg_entry_price"`
	CurrentPrice   string  `json:"current_price"`
	MarketValue    string  `json:"market_value"`
	// Alpaca's /positions endpoint returns numeric fields as JSON
	// strings (same convention as avg_entry_price). Keep these as
	// strings and convert in callers to keep the wire format
	// consistent with the rest of the Position fields.
	UnrealizedPL   string  `json:"unrealized_pl"`
	UnrealizedPLPC string  `json:"unrealized_plpc"`
}

func (c *Client) GetAccount(ctx context.Context) (*Account, error) {
	var a Account
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/account", nil, nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
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
}

func (c *Client) GetBars(ctx context.Context, symbols []string, timeframe, start, end string, limit int) (map[string][]Bar, error) {
	q := url.Values{}
	q.Set("symbols", join(symbols))
	q.Set("timeframe", orDefaultStr(timeframe, "1Day"))
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
	var br barsResponse
	if err := c.do(ctx, http.MethodGet, c.DataURL+"/stocks/bars", q, nil, &br); err != nil {
		return nil, err
	}
	return br.Bars, nil
}

type Quote struct {
	BidPrice  float64 `json:"bp"`
	AskPrice  float64 `json:"ap"`
	BidSize   int     `json:"bs"`
	AskSize   int     `json:"as"`
	Timestamp int64   `json:"t"`
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

type Snapshot struct {
	LatestTradePrice float64 `json:"latestTrade.p"`
	LatestQuoteBid   float64 `json:"latestQuote.bp"`
	LatestQuoteAsk   float64 `json:"latestQuote.ap"`
	DailyBar         Bar     `json:"dailyBar"`
	PrevDailyBar     Bar     `json:"prevDailyBar"`
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
