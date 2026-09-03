package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestPlaceOrderExtendedHoursConstraint verifies the client-side Alpaca
// rule: extended_hours=true is valid ONLY for type=limit with day or gtc.
func TestPlaceOrderExtendedHoursConstraint(t *testing.T) {
	tests := []struct {
		name    string
		req     OrderRequest
		wantErr bool
	}{
		{
			name:    "limit gtc ok",
			req:     OrderRequest{Symbol: "AAPL", Side: "buy", Qty: "1", Type: "limit", TimeInForce: "gtc", LimitPrice: "180", ExtendedHours: true},
			wantErr: false,
		},
		{
			name:    "limit day ok",
			req:     OrderRequest{Symbol: "AAPL", Side: "buy", Qty: "1", Type: "limit", TimeInForce: "day", LimitPrice: "180", ExtendedHours: true},
			wantErr: false,
		},
		{
			name:    "market extended rejected",
			req:     OrderRequest{Symbol: "AAPL", Side: "buy", Qty: "1", Type: "market", TimeInForce: "gtc", ExtendedHours: true},
			wantErr: true,
		},
		{
			name:    "limit ioc rejected",
			req:     OrderRequest{Symbol: "AAPL", Side: "buy", Qty: "1", Type: "limit", TimeInForce: "ioc", LimitPrice: "180", ExtendedHours: true},
			wantErr: true,
		},
		{
			name:    "limit fop rejected",
			req:     OrderRequest{Symbol: "AAPL", Side: "buy", Qty: "1", Type: "limit", TimeInForce: "fop", LimitPrice: "180", ExtendedHours: true},
			wantErr: true,
		},
		{
			name:    "extended hours off never constrained",
			req:     OrderRequest{Symbol: "AAPL", Side: "buy", Qty: "1", Type: "market", TimeInForce: ""}, // defaults to gtc
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				called = true
				_ = json.NewEncoder(w).Encode(Order{ID: "ok", Status: "new"})
			})
			_, err := c.PlaceOrder(context.Background(), tt.req)
			if tt.wantErr && err == nil {
				t.Fatalf("expected rejection for %+v, got nil (API called=%v)", tt.req, called)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected success for %+v, got %v", tt.req, err)
			}
			if tt.wantErr && called {
				t.Fatal("bad combo must be rejected before reaching the API")
			}
		})
	}
}

// TestPlaceOrderExtendedHoursWireFormat checks that a valid combo actually
// serializes extended_hours=true in the POST body.
func TestPlaceOrderExtendedHoursWireFormat(t *testing.T) {
	var gotExt bool
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req OrderRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotExt = req.ExtendedHours
		_ = json.NewEncoder(w).Encode(Order{ID: "ord-eh", Symbol: req.Symbol, Status: "new"})
	})
	if _, err := c.PlaceOrder(context.Background(), OrderRequest{
		Symbol: "TSLA", Side: "sell", Qty: "2", Type: "limit",
		TimeInForce: "day", LimitPrice: "250.75", ExtendedHours: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !gotExt {
		t.Fatal("extended_hours=true not serialized in request body")
	}
}
