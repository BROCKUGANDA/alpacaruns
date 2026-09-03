package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient("key", "secret", srv.URL, srv.URL)
	return c, srv
}

func TestGetAccount(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("APCA-API-KEY-ID") != "key" {
			t.Fatal("missing key header")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "acc1", "equity": "100000", "cash": "50000", "status": "ACTIVE",
		})
	})
	a, err := c.GetAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Equity != "100000" || a.Status != "ACTIVE" {
		t.Fatalf("unexpected account: %+v", a)
	}
}

func TestPlaceAndCancelOrder(t *testing.T) {
	cancelled := false
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req OrderRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Symbol != "AAPL" || req.Side != "buy" || req.Type != "limit" {
				t.Fatalf("bad order request: %+v", req)
			}
			_ = json.NewEncoder(w).Encode(Order{ID: "ord1", Symbol: req.Symbol, Status: "new"})
		case http.MethodDelete:
			cancelled = true
			w.WriteHeader(http.StatusNoContent)
		}
	})
	o, err := c.PlaceOrder(context.Background(), OrderRequest{
		Symbol: "AAPL", Side: "buy", Type: "limit", Qty: "10", LimitPrice: "180.50",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.ID != "ord1" {
		t.Fatalf("unexpected order %+v", o)
	}
	if err := c.CancelOrder(context.Background(), "ord1"); err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Fatal("cancel not called")
	}
}

func TestGetBars(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("symbols"); got != "AAPL,MSFT" {
			t.Fatalf("symbols query = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"bars": map[string]any{
				"AAPL": []map[string]any{{"t": 1, "o": 1.0, "h": 2.0, "l": 0.5, "c": 1.5, "v": 100}},
			},
		})
	})
	bars, err := c.GetBars(context.Background(), []string{"AAPL", "MSFT"}, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(bars["AAPL"]) != 1 || bars["AAPL"][0].Close != 1.5 {
		t.Fatalf("unexpected bars: %+v", bars)
	}
}

func TestHTTPErrorSurfaced(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":40110000,"message":"bad key"}`, http.StatusUnauthorized)
	})
	if _, err := c.GetAccount(context.Background()); err == nil {
		t.Fatal("expected error for HTTP 401")
	}
}
