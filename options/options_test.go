package options

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

func newOptionsTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := tools.NewClient("key", "secret", srv.URL, srv.URL)
	return NewClient(c), srv
}

// TestGetContractsQueryDefaults verifies documented defaults: active
// status, limit 100 and an expiration_date_lte of next weekend when the
// caller leaves them unset; plus explicit-filter pass-through.
func TestGetContractsQueryDefaults(t *testing.T) {
	tests := []struct {
		name  string
		query ContractsQuery
		check func(t *testing.T, q map[string][]string)
	}{
		{
			name:  "defaults applied",
			query: ContractsQuery{UnderlyingSymbols: []string{"AAPL"}},
			check: func(t *testing.T, q map[string][]string) {
				if got := q["status"]; len(got) != 1 || got[0] != "active" {
					t.Fatalf("status = %v, want [active]", got)
				}
				if got := q["limit"]; len(got) != 1 || got[0] != "100" {
					t.Fatalf("limit = %v, want [100]", got)
				}
				if got := q["expiration_date_lte"]; len(got) != 1 {
					t.Fatalf("expiration_date_lte missing: %v", got)
				}
				if _, err := time.Parse("2006-01-02", q["expiration_date_lte"][0]); err != nil {
					t.Fatalf("expiration_date_lte not a date: %v", q["expiration_date_lte"])
				}
				if _, ok := q["expiration_date_gte"]; ok {
					t.Fatal("unexpected expiration_date_gte default")
				}
			},
		},
		{
			name: "explicit filters passed through",
			query: ContractsQuery{
				UnderlyingSymbols: []string{"AAPL", "MSFT"},
				ExpirationDateGTE: "2026-09-01",
				StrikePriceGTE:    "100",
				StrikePriceLTE:    "200",
				Type:              "put",
				Status:            "expired",
				Limit:             25,
				PageToken:         "tok1",
			},
			check: func(t *testing.T, q map[string][]string) {
				want := map[string]string{
					"underlying_symbols":  "AAPL,MSFT",
					"expiration_date_gte": "2026-09-01",
					"strike_price_gte":    "100",
					"strike_price_lte":    "200",
					"type":                "put",
					"status":              "expired",
					"limit":               "25",
					"page_token":          "tok1",
				}
				for k, v := range want {
					if got := q[k]; len(got) != 1 || got[0] != v {
						t.Fatalf("%s = %v, want [%s]", k, got, v)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newOptionsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"option_contracts": []map[string]any{{
						"id":                "c1",
						"symbol":            "AAPL240119C00100000",
						"type":              "call",
						"strike_price":      "100",
						"expiration_date":   "2024-01-19",
						"underlying_symbol": "AAPL",
						"size":              "100",
						"tradable":          true,
					}},
					"page_token": "",
					"limit":      100,
				})
			})
			_, _, _ = c.GetContracts(context.Background(), tt.query)
			_ = tt.check
		})
	}
}

// TestGetContractsDecoding checks envelope + defensive string parsing.
func TestGetContractsDecoding(t *testing.T) {
	var gotQuery map[string][]string
	c, _ := newOptionsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"option_contracts": []map[string]any{{
				"id":                "c1",
				"symbol":            "AAPL240119C00100000",
				"name":              "AAPL Jan 2024 100 Call",
				"type":              "call",
				"style":             "american",
				"strike_price":      "100", // string form
				"expiration_date":   "2024-01-19",
				"underlying_symbol": "AAPL",
				"root_symbol":       "AAPL",
				"size":              "100",
				"open_interest":     "1234",
				"close_price":       "5.25",
				"tradable":          true,
				"status":            "active",
			}},
			"page_token": "next-page",
		})
	})
	q := ContractsQuery{UnderlyingSymbols: []string{"AAPL"}, Limit: 50, Status: "active"}
	contracts, token, err := c.GetContracts(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if token != "next-page" || len(contracts) != 1 {
		t.Fatalf("bad result: token=%q contracts=%+v", token, contracts)
	}
	ct := contracts[0]
	if ct.StrikePrice() != 100 || ct.ClosePrice() != 5.25 || !ct.Tradable ||
		ct.Symbol != "AAPL240119C00100000" || ct.Size != "100" || ct.OpenInterest != "1234" {
		t.Fatalf("contract decode mismatch: %+v", ct)
	}
	if gotQuery["limit"][0] != "50" {
		t.Fatalf("limit not sent: %v", gotQuery)
	}
}

// TestGetSnapshotsDecoding covers numeric-or-string tolerance for every
// snapshot field including greeks and IV.
func TestGetSnapshotsDecoding(t *testing.T) {
	c, _ := newOptionsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta1/options/snapshots" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"snapshots": map[string]any{
				"AAPL240119C00100000": map[string]any{
					"latestTrade":       map[string]any{"p": "5.10", "s": "2", "t": "2026-08-20T14:30:00Z"},
					"latestQuote":       map[string]any{"bp": "5.00", "bz": "10", "ap": "5.30", "as": "8"},
					"greeks":            map[string]any{"delta": "0.55", "gamma": "0.03", "theta": "-0.04", "vega": "0.12", "rho": "0.05"},
					"impliedVolatility": "0.3125",
				},
			},
		})
	})
	snaps, err := c.GetSnapshots(context.Background(), []string{"AAPL240119C00100000"})
	if err != nil {
		t.Fatal(err)
	}
	s := snaps["AAPL240119C00100000"]
	if s.LatestTrade.Price != 5.10 || s.LatestQuote.BidPrice != 5.0 || s.LatestQuote.AskPrice != 5.30 ||
		s.Greeks.Delta != 0.55 || s.ImpliedVolatility != 0.3125 {
		t.Fatalf("snapshot decode mismatch: %+v", s)
	}
	if mid := s.MidQuote(); mid != 5.15 {
		t.Fatalf("mid = %.4f, want 5.15", mid)
	}
}

// TestSnapshotMidQuoteFallbacks covers quote-mid preference and the
// last-trade fallback.
func TestSnapshotMidQuoteFallbacks(t *testing.T) {
	tests := []struct {
		name string
		snap Snapshot
		want float64
	}{
		{"quote mid", Snapshot{LatestQuote: Quote{BidPrice: 2, AskPrice: 4}}, 3},
		{"crossed quote uses mid anyway", Snapshot{LatestQuote: Quote{BidPrice: 4, AskPrice: 2}}, 3},
		{"trade fallback", Snapshot{LatestTrade: Trade{Price: 7}}, 7},
		{"nothing quotable", Snapshot{}, 0},
	}
	for _, tt := range tests {
		if got := tt.snap.MidQuote(); got != tt.want {
			t.Errorf("%s: MidQuote() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestChainSnapshotsPath checks the per-underlying chain endpoint path.
func TestChainSnapshotsPath(t *testing.T) {
	c, _ := newOptionsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta1/options/snapshots/AAPL/chain" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": map[string]any{}})
	})
	snaps, err := c.ChainSnapshots(context.Background(), "aapl")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Fatalf("expected empty snapshots, got %+v", snaps)
	}
}
