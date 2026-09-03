package main

import (
	"context"

	"github.com/BROCKUGANDA/alpacaruns/config"
)

// testConfig returns a minimal paper-mode config for CLI tests.
func testConfig() *config.Config {
	return &config.Config{
		AlpacaKeyID:     "test-key",
		AlpacaSecret:    "test-secret",
		AlpacaBaseURL:   "https://paper-api.alpaca.markets/v2",
		AlpacaDataURL:   "https://data.alpaca.markets/v1beta1",
		Mode:            config.ModeSupervised,
		PollSeconds:     60,
		MinConfidence:   0.7,
		MaxPositionUSD:  10000,
		MaxPortfolioPct: 0.20,
		TradeLog:        "data/trades.jsonl",
	}
}

func testCtx() context.Context { return context.Background() }
