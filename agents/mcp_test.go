package agents

import "testing"

// The names below were read off a live `uvx alpaca-mcp-server` session
// (72 tools) rather than guessed. The regression they guard: the guard
// used to match a short list of invented names, so the real order tools
// ("place_stock_order" and friends) reached Alpaca with NO risk gate at
// all — no kill switch, no caps, no market-hours check.
func TestClassifyToolCoversRealAlpacaTools(t *testing.T) {
	orders := []string{
		"place_stock_order",
		"place_crypto_order",
		"place_option_order",
		"replace_order_by_id",
		"exercise_options_position",
	}
	for _, name := range orders {
		if got := classifyTool(name); got != toolOrder {
			t.Errorf("classifyTool(%q) = %v, want toolOrder (it must be risk-gated)", name, got)
		}
	}

	controls := []string{
		"cancel_order_by_id",
		"cancel_all_orders",
		"close_position",
		"close_all_positions",
		"do_not_exercise_options_position",
	}
	for _, name := range controls {
		if got := classifyTool(name); got != toolControl {
			t.Errorf("classifyTool(%q) = %v, want toolControl", name, got)
		}
	}

	reads := []string{
		"get_stock_bars", "get_crypto_bars", "get_option_chain", "get_news",
		"get_all_positions", "get_orders", "get_account_info", "get_asset",
		"search_alpaca_docs", "fetch_alpaca_doc", "list_alpaca_api_endpoints",
	}
	for _, name := range reads {
		if got := classifyTool(name); got != toolRead {
			t.Errorf("classifyTool(%q) = %v, want toolRead", name, got)
		}
	}

	blocked := []string{
		"update_account_config",
		"create_locate",
		"delete_all_orders_unknown",
		"place_order",
	}
	for _, name := range blocked {
		if got := classifyTool(name); got == toolRead {
			t.Errorf("classifyTool(%q) must not be read-only", name)
		}
	}
}

// TestClassifyToolIsDefaultDeny proves an unrecognised ACTION tool is
// blocked rather than passed through.
func TestClassifyToolIsDefaultDeny(t *testing.T) {
	for _, name := range []string{"sell_everything", "place_rocket_order", "wipe_account"} {
		if got := classifyTool(name); got != toolBlocked {
			t.Errorf("classifyTool(%q) = %v, want toolBlocked (default deny)", name, got)
		}
	}
}

// TestProposalConfidenceFallsBackToDeterministicScore proves the agent
// path can carry a confidence value even though Alpaca's MCP order
// schema has no confidence field.
func TestProposalConfidenceFallsBackToDeterministicScore(t *testing.T) {
	p := proposalFromArgs("place_stock_order", map[string]any{
		"symbol": "AAPL", "side": "buy", "qty": "1",
	})
	if p.Confidence != nil {
		t.Fatalf("confidence must be absent when the args carry none: %v", *p.Confidence)
	}
	p2 := proposalFromArgs("place_stock_order", map[string]any{
		"symbol": "AAPL", "side": "buy", "qty": "1", "confidence": 0.82,
	})
	if p2.Confidence == nil || *p2.Confidence != 0.82 {
		t.Fatalf("confidence from args = %v, want 0.82", p2.Confidence)
	}
}

// TestIsPaperVenue pins the venue mapping used to configure the MCP
// child process.
func TestIsPaperVenue(t *testing.T) {
	if !isPaperVenue("https://paper-api.alpaca.markets/v2") {
		t.Fatal("paper base URL must map to the paper venue")
	}
	if isPaperVenue("https://api.alpaca.markets/v2") {
		t.Fatal("live base URL must not map to the paper venue")
	}
}
