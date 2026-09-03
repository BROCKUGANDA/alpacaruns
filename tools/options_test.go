package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestIsOCCSymbol covers OCC-format contract symbol detection:
// root (1-6 letters) + YYMMDD + C/P + strike x1000 (8 digits).
func TestIsOCCSymbol(t *testing.T) {
	tests := []struct {
		symbol string
		want   bool
	}{
		{"AAPL240119C00100000", true},
		{"AAPL240119P00100000", true},
		{"TSLA260117P00250000", true},
		{"SPXW241220C05000000", true}, // 5-letter root
		{"aapl240119c00100000", true}, // case-insensitive
		{" AAPL240119C00100000 ", true},
		{"AAPL", false},
		{"AAPL240119X00100000", false}, // bad type char
		{"AAPL24119C00100000", false},  // short date
		{"AAPL240119C001000", false},   // short strike
		{"240119C00100000", false},     // no root
		{"", false},
	}
	for _, tt := range tests {
		if got := IsOCCSymbol(tt.symbol); got != tt.want {
			t.Errorf("IsOCCSymbol(%q) = %v, want %v", tt.symbol, got, tt.want)
		}
	}
}

// TestPlaceOrderOptionsValidations verifies the client-side mirror of
// Alpaca's options-order rules: whole-number qty, no notional,
// time_in_force day|gtc only, extended_hours must be unset.
func TestPlaceOrderOptionsValidations(t *testing.T) {
	tests := []struct {
		name    string
		req     OrderRequest
		wantErr bool
	}{
		{
			name:    "market buy day ok",
			req:     OrderRequest{Symbol: "AAPL240119C00100000", Side: "buy", Qty: "1", Type: "market", TimeInForce: "day"},
			wantErr: false,
		},
		{
			name:    "limit sell gtc ok",
			req:     OrderRequest{Symbol: "AAPL240119P00100000", Side: "sell", Qty: "2", Type: "limit", TimeInForce: "gtc", LimitPrice: "5.50"},
			wantErr: false,
		},
		{
			name:    "notional rejected",
			req:     OrderRequest{Symbol: "AAPL240119C00100000", Side: "buy", Qty: "1", Notional: "500", Type: "market"},
			wantErr: true,
		},
		{
			name:    "extended hours rejected",
			req:     OrderRequest{Symbol: "AAPL240119C00100000", Side: "buy", Qty: "1", Type: "limit", LimitPrice: "5.50", ExtendedHours: true},
			wantErr: true,
		},
		{
			name:    "ioc rejected",
			req:     OrderRequest{Symbol: "AAPL240119C00100000", Side: "buy", Qty: "1", Type: "market", TimeInForce: "ioc"},
			wantErr: true,
		},
		{
			name:    "fop rejected",
			req:     OrderRequest{Symbol: "AAPL240119C00100000", Side: "buy", Qty: "1", Type: "market", TimeInForce: "fop"},
			wantErr: true,
		},
		{
			name:    "fractional qty rejected",
			req:     OrderRequest{Symbol: "AAPL240119C00100000", Side: "buy", Qty: "1.5", Type: "market", TimeInForce: "day"},
			wantErr: true,
		},
		{
			name:    "zero qty rejected",
			req:     OrderRequest{Symbol: "AAPL240119C00100000", Side: "buy", Qty: "0", Type: "market", TimeInForce: "day"},
			wantErr: true,
		},
		{
			name:    "negative qty rejected",
			req:     OrderRequest{Symbol: "AAPL240119C00100000", Side: "buy", Qty: "-2", Type: "market", TimeInForce: "day"},
			wantErr: true,
		},
		{
			name:    "empty qty rejected",
			req:     OrderRequest{Symbol: "AAPL240119C00100000", Side: "buy", Type: "market", TimeInForce: "day"},
			wantErr: true,
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
				t.Fatal("invalid options order must be rejected before reaching the API")
			}
		})
	}
}

// TestPlaceOrderOptionsWireFormat checks a valid option order reaches the
// API with the OCC symbol in the standard /v2/orders payload.
func TestPlaceOrderOptionsWireFormat(t *testing.T) {
	var got OrderRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Order{ID: "opt1", Symbol: req_symbol(&got), Status: "new"})
	})
	o, err := c.PlaceOrder(context.Background(), OrderRequest{
		Symbol: "TSLA260619P00200000", Side: "buy", Qty: "3", Type: "limit",
		TimeInForce: "day", LimitPrice: "12.25",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.ID != "opt1" || got.Symbol != "TSLA260619P00200000" ||
		got.Qty != "3" || got.Type != "limit" || got.TimeInForce != "day" ||
		got.LimitPrice != "12.25" || got.Notional != "" || got.ExtendedHours {
		t.Fatalf("wire payload mismatch: %+v / order %+v", got, o)
	}
}

func req_symbol(r *OrderRequest) string { return r.Symbol }
