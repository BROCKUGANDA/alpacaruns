package universe

// fetch.go builds the cap table from SEC EDGAR (shares outstanding) times the
// broker's latest close. Both inputs are authoritative; the product is a real
// market capitalisation, not an estimate.
//
// EDGAR is US-listed single-name equities. Funds (ETFs/trusts) do not report
// dei:EntityCommonStockSharesOutstanding, and crypto has no share count — both
// classes are exempt from the screen rather than fed a fabricated number.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// EDGAR endpoints. The ticker map is a single static document; the concept
// endpoint is per filer.
const (
	edgarTickerMap = "https://www.sec.gov/files/company_tickers.json"
	edgarConcept   = "https://data.sec.gov/api/xbrl/companyconcept/CIK%010d/dei/EntityCommonStockSharesOutstanding.json"
)

// DefaultEDGARUserAgent identifies this bot to the SEC, which requires a
// descriptive agent with contact details. Override with EDGAR_USER_AGENT.
const DefaultEDGARUserAgent = "alpacaruns research (ops@example.com)"

// CloseSource returns the latest close for a symbol.
type CloseSource func(ctx context.Context, symbol string) (float64, error)

// FetchOptions bounds one refresh.
type FetchOptions struct {
	UserAgent string
	// Pause is the delay between EDGAR requests. The SEC asks for a rate that
	// stays under 10 requests/second; a handful of symbols never gets close,
	// but the pause keeps a large refresh polite.
	Pause time.Duration
	Now   func() time.Time
}

// Fetch builds the cap table for symbols.
//
// A symbol whose shares cannot be read is OMITTED and reported, not defaulted:
// a missing cap must look missing, because the screen fails closed on it.
func Fetch(ctx context.Context, client *http.Client, symbols []string, closeOf CloseSource, opts FetchOptions) (Caps, []string, error) {
	if client == nil {
		return Caps{}, nil, fmt.Errorf("universe: no http client")
	}
	if opts.UserAgent == "" {
		opts.UserAgent = DefaultEDGARUserAgent
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ciks, err := fetchTickerMap(ctx, client, opts.UserAgent)
	if err != nil {
		return Caps{}, nil, err
	}
	out := Caps{Caps: map[string]float64{}, AsOf: opts.Now().UTC(),
		Source: "SEC EDGAR shares outstanding (dei) x Alpaca latest close"}
	var skipped []string
	for _, raw := range symbols {
		sym := strings.ToUpper(strings.TrimSpace(raw))
		if sym == "" {
			continue
		}
		cik, ok := ciks[sym]
		if !ok {
			skipped = append(skipped, sym+": not an SEC filer (fund or non-US listing)")
			continue
		}
		shares, err := fetchShares(ctx, client, opts.UserAgent, cik)
		if err != nil {
			skipped = append(skipped, sym+": "+err.Error())
			continue
		}
		close, err := closeOf(ctx, sym)
		if err != nil || close <= 0 {
			skipped = append(skipped, fmt.Sprintf("%s: latest close unavailable (%v)", sym, err))
			continue
		}
		out.Caps[sym] = shares * close
		if opts.Pause > 0 {
			select {
			case <-ctx.Done():
				return out, skipped, ctx.Err()
			case <-time.After(opts.Pause):
			}
		}
	}
	return out, skipped, nil
}

func getJSON(ctx context.Context, client *http.Client, ua, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fetchTickerMap(ctx context.Context, client *http.Client, ua string) (map[string]int, error) {
	var raw map[string]struct {
		CIK    int    `json:"cik_str"`
		Ticker string `json:"ticker"`
	}
	if err := getJSON(ctx, client, ua, edgarTickerMap, &raw); err != nil {
		return nil, fmt.Errorf("EDGAR ticker map: %w", err)
	}
	out := make(map[string]int, len(raw))
	for _, v := range raw {
		if v.Ticker != "" && v.CIK > 0 {
			out[strings.ToUpper(v.Ticker)] = v.CIK
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("EDGAR ticker map was empty")
	}
	return out, nil
}

func fetchShares(ctx context.Context, client *http.Client, ua string, cik int) (float64, error) {
	var doc struct {
		Units struct {
			Shares []SharesRecord `json:"shares"`
		} `json:"units"`
	}
	if err := getJSON(ctx, client, ua, fmt.Sprintf(edgarConcept, cik), &doc); err != nil {
		return 0, fmt.Errorf("EDGAR shares: %w", err)
	}
	shares, _, err := LatestShares(doc.Units.Shares)
	return shares, err
}

// SortedSymbols returns the table's keys, for display.
func (c Caps) SortedSymbols() []string {
	out := make([]string, 0, len(c.Caps))
	for s := range c.Caps {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
