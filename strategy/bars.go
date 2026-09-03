package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// MultiVenueBars satisfies factors.BarSource across BOTH venues:
// equities via the stock bars endpoint and crypto via
// /v1beta3/crypto/us/bars. Two live-API quirks are handled here so the
// factor engine never sees empty or undecodable data:
//
//   - Alpaca's bars endpoints return an EMPTY page when `start` is
//     omitted, so a ~8-month daily lookback is injected by default
//     (same fix as cmd/alpacaruns/explain.go's datedBars adapter).
//   - The v1beta3 crypto feed returns bar timestamps as RFC3339 strings
//     while tools.Bar expects numeric epoch; decoding is tolerant of
//     either form. Time is normalized to unix nanos for the engine's
//     chronological sort.
type MultiVenueBars struct {
	C *tools.Client
}

// NewMultiVenueBars builds the routing bar source.
func NewMultiVenueBars(c *tools.Client) *MultiVenueBars { return &MultiVenueBars{C: c} }

func (m *MultiVenueBars) GetBars(ctx context.Context, symbols []string, timeframe, start, end string, limit int) (map[string][]tools.Bar, error) {
	if start == "" {
		start = time.Now().AddDate(0, -8, 0).Format("2006-01-02")
	}
	var stocks, crypto []string
	for _, s := range symbols {
		if IsCrypto(s) {
			crypto = append(crypto, s)
		} else {
			stocks = append(stocks, s)
		}
	}
	out := map[string][]tools.Bar{}
	if len(stocks) > 0 {
		res, err := m.stockBars(ctx, stocks, timeframe, start, end, limit)
		if err != nil {
			return nil, fmt.Errorf("stock bars: %w", err)
		}
		for k, v := range res {
			out[k] = v
		}
	}
	if len(crypto) > 0 {
		res, err := m.cryptoBars(ctx, crypto, timeframe, start, end, limit)
		if err != nil {
			return nil, fmt.Errorf("crypto bars: %w", err)
		}
		for k, v := range res {
			out[k] = v
		}
	}
	return out, nil
}

func (m *MultiVenueBars) stockBars(ctx context.Context, symbols []string, timeframe, start, end string, limit int) (map[string][]tools.Bar, error) {
	q := url.Values{}
	q.Set("symbols", strings.Join(symbols, ","))
	q.Set("timeframe", orDefault(timeframe, "1Day"))
	q.Set("start", start)
	if end != "" {
		q.Set("end", end)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	// Local decode: v2 REST timestamps arrive as RFC3339 strings which
	// tools.Bar's int64 Time cannot absorb directly.
	var br struct {
		Bars map[string][]datedBar `json:"bars"`
	}
	if err := m.C.Do(ctx, http.MethodGet, m.C.DataURL+"/stocks/bars", q, nil, &br); err != nil {
		return nil, err
	}
	out := make(map[string][]tools.Bar, len(br.Bars))
	for sym, bars := range br.Bars {
		ts := make([]tools.Bar, len(bars))
		for i, b := range bars {
			ts[i] = tools.Bar{Time: b.unixNano(), Open: b.Open, High: b.High,
				Low: b.Low, Close: b.Close, Volume: b.volume()}
		}
		out[sym] = ts
	}
	return out, nil
}

func (m *MultiVenueBars) cryptoBars(ctx context.Context, symbols []string, timeframe, start, end string, limit int) (map[string][]tools.Bar, error) {
	q := url.Values{}
	q.Set("symbols", strings.Join(symbols, ","))
	q.Set("timeframe", orDefault(timeframe, "1Day"))
	q.Set("start", start)
	if end != "" {
		q.Set("end", end)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var br struct {
		Bars map[string][]datedBar `json:"bars"`
	}
	endpoint := strings.Replace(m.C.DataURL+"/crypto/us/bars", "/v2/", "/v1beta3/", 1)
	if err := m.C.Do(ctx, http.MethodGet, endpoint, q, nil, &br); err != nil {
		return nil, err
	}
	out := make(map[string][]tools.Bar, len(br.Bars))
	for sym, bars := range br.Bars {
		ts := make([]tools.Bar, len(bars))
		for i, b := range bars {
			ts[i] = tools.Bar{Time: b.unixNano(), Open: b.Open, High: b.High,
				Low: b.Low, Close: b.Close, Volume: b.volume()}
		}
		out[sym] = ts
	}
	return out, nil
}

// datedBar tolerates the encodings seen across Alpaca feeds: RFC3339
// string OR numeric timestamps, and int64 OR float volumes (the
// v1beta3 crypto feed returns e.g. 1.447382512 BTC).
type datedBar struct {
	Raw   json.RawMessage `json:"t"`
	Open  float64         `json:"o"`
	High  float64         `json:"h"`
	Low   float64         `json:"l"`
	Close float64         `json:"c"`
	Vol   json.Number     `json:"v"`
}

// unixNano normalizes Raw to unix nanoseconds; 0 on any parse failure.
func (b datedBar) unixNano() int64 {
	var num json.Number
	if err := json.Unmarshal(b.Raw, &num); err == nil {
		if secs, err := num.Int64(); err == nil {
			return secs * 1e9
		}
		if f, err := num.Float64(); err == nil {
			return int64(f * 1e9)
		}
		return 0
	}
	var s string
	if err := json.Unmarshal(b.Raw, &s); err == nil {
		if p, err := time.Parse(time.RFC3339, s); err == nil {
			return p.UnixNano()
		}
	}
	return 0
}

// volume normalizes Vol to int64 (fractional crypto volumes floor);
// 0 on any parse failure.
func (b datedBar) volume() int64 {
	if n, err := b.Vol.Int64(); err == nil {
		return n
	}
	if f, err := b.Vol.Float64(); err == nil {
		return int64(f)
	}
	return 0
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
