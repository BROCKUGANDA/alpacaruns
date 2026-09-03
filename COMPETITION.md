# Alpacaruns — Competition Submission

## 1. System Overview

Alpacaruns is an autonomous trading system for Alpaca's Trading API, written in Go.
It operates in two modes: a Mixture-of-Experts multi-agent graph built on Google ADK
for Go (`agents/`) that trades through Alpaca's official MCP server
(`uvx alpaca-mcp-server`, spawned in `agents/mcp.go`), and a fully deterministic
no-LLM pipeline (`strategy/`, `factors/`). Paper trading is the hard default — all
orders go to `https://paper-api.alpaca.markets/v2` and a non-paper base URL is
refused unless `I_ALPACA_LIVE=YES` is set (`deploy/DEPLOY.md`, `config/config.go`).
P&L is measured over time from an append-only JSONL journal of every fill,
decision, and reconciliation snapshot (`pnl/pnl.go`).

## 2. Compliance Checklist

| Requirement | Status | Where |
|---|---|---|
| Alpaca Trading API | Yes | Typed REST client `tools/alpaca.go` (orders, bars, quotes, snapshots, news, account, positions); options endpoints in `options/options.go` |
| MCP server | Yes | `agents/mcp.go` spawns Alpaca's official `uvx alpaca-mcp-server` (separate data + exec toolsets); every agent Alpaca call is an MCP tool |
| Options trading | Yes | Typed contracts/snapshots/greeks client (`options/options.go`); manual `trade --occ <OCC>` / auto-detected OCC symbol (`cmd/alpacaruns/main.go` usage); read-only `chain` command; autonomous overlay of ~60–70 delta deep-ITM calls, 30–45 DTE (`strategy/settings.go`), gated by the same validator |
| Autonomous operation | Yes | `auto` mode: pure deterministic loop, no LLM (`strategy/decide.go`, `cmd/alpacaruns/main.go:79-82`); LLM modes run supervised (`MODE=supervised`, default) or autonomous (`MODE=autonomous`: risk pass AND confidence ≥ `MIN_CONFIDENCE`) |
| P&L measurement | Yes | JSONL journal (`TRADE_LOG`, default `data/trades.jsonl`), FIFO realized + live-mark unrealized + win rate (`pnl/pnl.go` `Compute`, `Unrealized`), reported by `pl [--since DATE]` with backfill of fills missed while down |

## 3. Architecture

```
LLM path (cycle / monitor)                    Deterministic path (auto)
--------------------------                    -------------------------
        user / tick                                  tick (POLL_SECONDS)
             |                                              |
       GatingRoot (LLM router)                     factors.Engine.Score
       NLQuery | TradingCycle | MonitorLoop         5 factors -> composite
             |                                              |
   MarketData -> AnalysisParallel            strategy.Decide (thresholds)
   (Technical || Sentiment) -> TradeIdea          BUY / SELL / HOLD
             |                                              |
     RiskManagementExpert                    time-window gate (TRADING_WINDOWS /
             |                               CRYPTO_WINDOWS) + fixed-fractional sizing
     ExecutionExpert (MCP exec tools)                       |
             |                                       risk.Validator.Validate   <---+
             |                                              |                      |
             +------------------+---------------------------+      shared single point
                                |                                  of enforcement
                        Alpaca paper API  <--- trade --occ / --symbol manual orders -+
```

Both paths funnel every order through the same Go-coded pre-trade gate:
`risk.NewValidator` (`risk/risk.go`), used by the agent execution path, the
deterministic `auto` loop, and manual `trade` commands alike.

## 4. Strategy Specification

Five factors (`factors/factors.go`), each scored 0..1 with a human-readable
rationale; composite = weighted mean (`config/config.go` `DefaultFactorWeights`,
must sum to ~1 or config rejects it):

- trend 0.30 — close vs SMA20/SMA50 of daily bars
- momentum 0.25 — 10-day return mapped linearly (+10% → 1.0, −10% → 0.0)
- volume 0.20 — latest volume vs trailing 20-day average
- volatility 0.15 — stdev of daily returns vs threshold (default 3%)
- sentiment 0.10 — news-attention proxy; never errors, degrades neutral

Decision rule (`strategy/decide.go` `Decide`; boundary-inclusive):

- **BUY** when composite ≥ `FACTOR_MIN_SCORE` (default 0.6) AND trend ≥ `TREND_BUY` (0.7) AND momentum ≥ `MOMENTUM_BUY` (0.6)
- **SELL** a held position when composite ≤ `EXIT_COMPOSITE` (0.4) OR momentum ≤ `EXIT_MOMENTUM` (0.35)
- else **HOLD**. Identical inputs always produce identical outputs.

Windows & sizing (`strategy/settings.go`): equities/options fire only inside
`TRADING_WINDOWS` (ET, default `09:35-10:15,15:45-16:00`); crypto uses
`CRYPTO_WINDOWS` (24/7). Budget = portfolio × `POSITION_PCT` (0.05), capped by
`MAX_POSITION_USD`; qty = floor(budget/price), skipped if < 1.

Brackets: equities get server-side OCO brackets — TP = entry×(1+`TP_PCT`=0.08),
SL = entry×(1−`SL_PCT`=0.04) (`ComputeBrackets`, requires TP > SL > 0).
Crypto does not support brackets on Alpaca, so levels are persisted to
`data/strategy-state.json` and enforced locally each tick; missing equity stops
are rebuilt after restarts.

Options overlay (`OPTIONS_ENABLED=true`): an equity BUY may be replaced by a call
with delta ∈ [0.60, 0.70] and DTE ∈ [30, 45], sized so premium outlay stays within
the position budget.

Drawdown halts: daily ≥ `DAILY_DD_HALT` (5%), weekly ≥ `WEEKLY_DD_HALT` (10%),
or total ≥ `TOTAL_DD_HALT` (15%) of peak equity engages the shared kill switch;
nothing further trades until restart.

## 5. Risk Controls

The deterministic validator (`risk/risk.go` `Validator.Validate`) runs on every
order from any path:

- position notional cap (`MAX_POSITION_USD`) and portfolio-percentage cap (`MAX_PORTFOLIO_PCT`)
- confidence ≥ `MIN_CONFIDENCE` plus the multi-factor gate (composite ≥ `FACTOR_MIN_SCORE`)
- kill switch consulted before anything proceeds
- market-hours/session rules via `MarketClock` (regular session, extended-session window weekdays 04:00–20:00 ET when `EXTENDED_HOURS` allows)
- options constraints: whole contracts only, no notional sizing, tif day|gtc only, no extended hours, exposure = premium outlay = qty × 100 × mark (quote mid / last trade; a price-fetch failure rejects the order)

Fail-closed semantics: any error fetching live state, prices, or factor scores is
a rejection, never a pass (`risk/risk.go`, `factors/factors.go`).

## 6. Observability & Audit

- Append-only JSONL trade log records every fill, decision (`strategy:auto`,
  `cli:trade`, `cli:chain`), and boot reconcile snapshot; fills are deduplicated
  by Alpaca order ID (`pnl/pnl.go` `Journal`).
- Live websocket streams during `monitor`: market-data feed (`STREAM_FEED`,
  default iex) and account trade_updates — each fill journaled the moment it arrives.
- Boot reconciliation compares local FIFO books against broker positions and
  journals drift (`pnl.ComparePositions`).
- `pl [--since DATE] [--json]` prints equity, realized P/L (FIFO), unrealized
  (live marks), win rate, per-symbol breakdown; works while monitor runs and
  backfills missed fills.
- `factors explain SYMBOL` prints each factor score and rationale for full
  decision transparency.

## 7. Operations

Docker (distroless, non-root, state in `/app/data` volume) and systemd unit
(`Restart=always`, `RestartSec=10`) under `deploy/`. SIGINT/SIGTERM sets the
kill switch: no new trades start, in-flight order polling finishes before exit.
Panics in the monitoring loop are recovered and logged; the process keeps
running. State (trade log, crypto bracket levels) survives restarts; boot-time
reconciliation re-syncs from Alpaca.

## 8. Honest Limitations

- Options snapshot data requires plan enablement; 404s were observed on the free
  tier, so the autonomous options overlay degrades to equity entries there.
- Local-LLM tool calling depends heavily on model choice and chat-template setup
  (`--jinja` for Qwen3; see README model notes) — malformed tool calls remain the
  top local-model failure mode.
- All results to date are paper-only; no live capital has been traded.
- No backtesting engine yet: thresholds are validated by unit tests and live
  paper runs, not historical replay.
