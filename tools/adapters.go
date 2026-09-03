// Package tools adapts the Alpaca client into ADK function tools.
package tools

import (
	"context"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// Set bundles every Alpaca capability exposed to agents.
type Set struct {
	Client *Client
}

type emptyArgs struct{}

type orderIDArgs struct {
	OrderID string `json:"orderId"`
}

type cancelResult struct {
	OK bool `json:"ok"`
}

// All returns the full ADK tool list: account, positions, market data,
// and paper order placement/status/cancel.
func (s *Set) All() ([]tool.Tool, error) {
	acct, err := functiontool.New(functiontool.Config{
		Name:        "get_account",
		Description: "Get the Alpaca PAPER account: equity, cash, buying power, status.",
	}, func(ctx agent.ToolContext, _ emptyArgs) (*Account, error) {
		return s.Client.GetAccount(context.Background())
	})
	if err != nil {
		return nil, err
	}
	pos, err := functiontool.New(functiontool.Config{
		Name:        "get_positions",
		Description: "List all open positions with qty, entry price, current price.",
	}, func(ctx agent.ToolContext, _ emptyArgs) ([]Position, error) {
		return s.Client.GetPositions(context.Background())
	})
	if err != nil {
		return nil, err
	}
	bars, err := functiontool.New(functiontool.Config{
		Name:        "get_bars",
		Description: "Historical bars. Args: symbols []string, timeframe (1Day|1Hour|...), start/end RFC3339, limit int.",
	}, s.getBars)
	if err != nil {
		return nil, err
	}
	quotes, err := functiontool.New(functiontool.Config{
		Name:        "get_quotes",
		Description: "Latest bid/ask quotes. Args: symbols []string.",
	}, s.getQuotes)
	if err != nil {
		return nil, err
	}
	snap, err := functiontool.New(functiontool.Config{
		Name:        "get_snapshot",
		Description: "Full snapshots (trade+quote+daily bars). Args: symbols []string.",
	}, s.getSnapshot)
	if err != nil {
		return nil, err
	}
	news, err := functiontool.New(functiontool.Config{
		Name:        "get_news",
		Description: "Recent news headlines. Args: symbols []string optional, limit int.",
	}, s.getNews)
	if err != nil {
		return nil, err
	}
	order, err := functiontool.New(functiontool.Config{
		Name:        "place_order",
		Description: "Place a PAPER order. Args: symbol, side buy|sell, type market|limit|stop, qty OR notional (USD), limitPrice, stopPrice.",
	}, s.placeOrder)
	if err != nil {
		return nil, err
	}
	status, err := functiontool.New(functiontool.Config{
		Name:        "get_order_status",
		Description: "Fetch one order's fill status by id. Args: orderId string.",
	}, func(ctx agent.ToolContext, in orderIDArgs) (*Order, error) {
		return s.Client.GetOrderStatus(context.Background(), in.OrderID)
	})
	if err != nil {
		return nil, err
	}
	cancel, err := functiontool.New(functiontool.Config{
		Name:        "cancel_order",
		Description: "Cancel an open order by id. Args: orderId string.",
	}, func(ctx agent.ToolContext, in orderIDArgs) (cancelResult, error) {
		if err := s.Client.CancelOrder(context.Background(), in.OrderID); err != nil {
			return cancelResult{}, err
		}
		return cancelResult{OK: true}, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.Tool{acct, pos, bars, quotes, snap, news, order, status, cancel}, nil
}
