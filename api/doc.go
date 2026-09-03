// Package api implements the read/control HTTP surface for the Alpacaruns
// dashboard. The server is deliberately boring: net/http, no external
// router, JSON in/out. It reads the same data/ directory the live bot
// writes to (trades.jsonl + strategy-state.json) and proxies live
// account/position reads to Alpaca's REST API through tools.Client.
//
// Architecture (Option A from the hackathon spec):
//
//	alpacaruns auto   -- writes to data/trades.jsonl + data/strategy-state.json
//	alpacaruns serve  -- reads the same files, never writes them (except pause.flag)
//
// One source of truth: the bot's JSONL log. No separate DB.
package api

// Version is the bot version reported by /api/health. Kept in sync with
// the showcase UI banner; bump together when shipping.
const Version = "0.9.6"