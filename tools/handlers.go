package tools

import (
	"context"
	"fmt"

	"google.golang.org/adk/agent"
)

type symbolsInput struct {
	Symbols   []string `json:"symbols"`
	Timeframe string   `json:"timeframe,omitempty"`
	Start     string   `json:"start,omitempty"`
	End       string   `json:"end,omitempty"`
	Limit     int      `json:"limit,omitempty"`
}

func (s *Set) getBars(ctx agent.ToolContext, in symbolsInput) (map[string][]Bar, error) {
	if len(in.Symbols) == 0 {
		return nil, fmt.Errorf("symbols required")
	}
	return s.Client.GetBars(context.Background(), in.Symbols, in.Timeframe, in.Start, in.End, in.Limit)
}

func (s *Set) getQuotes(ctx agent.ToolContext, in symbolsInput) (map[string]Quote, error) {
	if len(in.Symbols) == 0 {
		return nil, fmt.Errorf("symbols required")
	}
	return s.Client.GetQuotes(context.Background(), in.Symbols)
}

func (s *Set) getSnapshot(ctx agent.ToolContext, in symbolsInput) (map[string]Snapshot, error) {
	if len(in.Symbols) == 0 {
		return nil, fmt.Errorf("symbols required")
	}
	return s.Client.GetSnapshots(context.Background(), in.Symbols)
}

func (s *Set) getNews(ctx agent.ToolContext, in symbolsInput) ([]NewsItem, error) {
	return s.Client.GetNews(context.Background(), in.Symbols, in.Limit)
}

type orderInput struct {
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`
	Type       string  `json:"type"`
	Qty        float64 `json:"qty,omitempty"`
	Notional   float64 `json:"notional,omitempty"`
	LimitPrice float64 `json:"limitPrice,omitempty"`
	StopPrice  float64 `json:"stopPrice,omitempty"`
}

func fmtF(f float64) string { return fmt.Sprintf("%g", f) }

func (s *Set) placeOrder(ctx agent.ToolContext, in orderInput) (*Order, error) {
	req := OrderRequest{
		Symbol:      in.Symbol,
		Side:        in.Side,
		Type:        in.Type,
		TimeInForce: "gtc",
	}
	switch {
	case in.Qty > 0:
		req.Qty = fmtF(in.Qty)
	case in.Notional > 0:
		req.Notional = fmtF(in.Notional)
	default:
		return nil, fmt.Errorf("qty or notional required")
	}
	if in.Type == "limit" && in.LimitPrice > 0 {
		req.LimitPrice = fmtF(in.LimitPrice)
	}
	if in.Type == "stop" && in.StopPrice > 0 {
		req.StopPrice = fmtF(in.StopPrice)
	}
	return s.Client.PlaceOrder(context.Background(), req)
}
