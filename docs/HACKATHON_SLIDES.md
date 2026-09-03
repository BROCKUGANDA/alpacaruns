# Alpacaruns — Hackathon Slide Outline

8 slides, 30-second-each speaker notes. Targeted at an Alpaca / quant audience.

---

## Slide 1 — Title

**Title:** Alpacaruns — Multi-Agent Paper Trading on Alpaca
**Subtitle:** Mixture-of-Experts ensemble + ADK multi-agent graph on the official Alpaca MCP server
**Footer:** Go 1.26 · Paper-only · post-2026-08-28 fresh account

**Speaker notes:**
30-second opener. One-sentence positioning: "Alpacaruns is a Go service that
trades Alpaca paper through two cooperating paths — an LLM multi-agent graph
that calls the official `alpaca-mcp-server`, and a deterministic
mixture-of-experts ensemble — both funneled through the same code-enforced
risk gate."

---

## Slide 2 — Two paths, one risk gate

**Title:** LLM path + deterministic path, single pre-trade validator
**Bullets:**
- LLM path: ADK `GatingRoot → TradingCycle → ExecutionExpert → Alpaca MCP` (guarded)
- Deterministic path: `factors.Engine → strategy.Decide → risk.Validator`
- `auto` loop is LLM-free; supervised mode default, autonomous gated on `MIN_CONFIDENCE`
- Manual `trade` commands route through the same validator (`risk/risk.go`)
- Every order — agent, auto, manual — hits `Validator.Validate` before any wire call

**Speaker notes:**
60 seconds. Emphasise the convergence point. Show that no matter which path
emits a decision, the same Go-coded risk gate runs and any error is a
rejection (fail-closed). This is what separates Alpacaruns from a pure LLM
wrapper — the gate is code, not prompt.

---

## Slide 3 — The MoE ensemble

**Title:** Layer-2 Mixture-of-Experts on top of the factor engine
**Bullets:**
- 5-factor composite: trend 0.30 / momentum 0.25 / volume 0.20 / vol 0.15 / sentiment 0.10
- 6 expert voices: trend, mean-rev, breakout, pairs (SPY/QQQ,XLE/XLF), xsmom, seasonality
- One bar fetch per tick (`runner.BuildMarketData`), shared read-only across all experts
- Performance-weighted stacking gater (`weight = baseWeight × (0.5 + hitRate)`)
- Vol-regime modifier down-weights contrarians in RisingVol/Crisis
- Variance breaker forces Hold when experts disagree > 0.15; winning mass must clear 0.55
- Hit-rate tracker persists to `data/ensemble-state.json` (`ENSEMBLE_PERF_WINDOW=30`)

**Speaker notes:**
90 seconds. This is the headline AI piece. Walk through the data flow: bars
fetched once → six experts score in parallel → gater stacks → risk budget →
executor. The key property is "no LLM anywhere" in the ensemble — it's all
deterministic math on real bars, and the hit-rate window makes the weights
adapt to live performance without retraining.

---

## Slide 4 — The ADK multi-agent graph

**Title:** Agent graph on the official Alpaca MCP server
**Bullets:**
- `agents/agents.go`: `GatingRoot` routes `NLQuery | TradingCycle | MonitorLoop`
- Trading cycle = `MarketData → AnalysisParallel (Technical ∥ Sentiment) → TradeIdea → RiskManagementExpert → ExecutionExpert`
- `agents/mcp.go`: spawns `uvx alpaca-mcp-server` with `mcp.CommandTransport`
- Both `data` and `exec` toolsets registered; keys injected via env
- Every order tool is wrapped in `guardedTool` — risk gate runs before MCP wire call
- Pluggable model: llama.cpp local / Gemini / Oxlo.ai via `config.LLMProvider`

**Speaker notes:**
60 seconds. Highlight that we use the **official** Alpaca MCP server, not a
homegrown shim — same `uvx alpaca-mcp-server` binary the docs recommend.
guardedTool is the bridge between the LLM's call and our Go validator.

---

## Slide 5 — Risk gates (the boring-but-correct part)

**Title:** Code-enforced risk gates, fail closed
**Bullets:**
- Per-position notional: `MAX_POSITION_USD` equities / `CRYPTO_MAX_POSITION_USD` crypto
- Per-symbol cap: `existing + notional ≤ MaxPositionUSD`; crypto 3× scale-in room
- Portfolio percentage: `notional / equity ≤ MAX_PORTFOLIO_PCT` (additive exposure)
- Confidence ≥ `MIN_CONFIDENCE` AND composite ≥ `FACTOR_MIN_SCORE` (both gates pass)
- Drawdown halts: 5% daily, 10% weekly, 15% total → engage shared `KillSwitch`
- Session rules: limit/day|gtc only in extended hours; options market always closed outside regular
- Options constraints: whole contracts, no notional, no extended hours, tif day|gtc
- Fail closed: any error fetching state / prices / factor scores = rejection

**Speaker notes:**
90 seconds. The list above is the rule book, written in Go. The audience for
this hackathon cares most about: drawdown halts (kill switch first check),
the additive exposure cap (no double-down), the fact that options are
**client-side-mirrored** constraints that run before they hit Alpaca, and
that the validator cannot be bypassed — it sits between every decision path
and the wire.

---

## Slide 6 — Alpaca infrastructure

**Title:** End-to-end decision → journal → P&L path
**Bullets:**
- Paper endpoint only: `https://paper-api.alpaca.markets/v2` (refused unless `I_ALPACA_LIVE=YES`)
- Typed Go client `tools/alpaca.go` + `options/options.go` (OCC, snapshots, greeks)
- Decision → order: `GatingRoot → ExecutionExpert → guardedTool → mcptoolset → MCP → /v2/orders`
- Auto path: `auto` loop calls typed client directly; same validator
- Live streams: market-data feed (`STREAM_FEED=iex`) + `trade_updates` journal every fill
- JSONL trade log (`data/trades.jsonl`), deduplicated by Alpaca order ID
- `pl [--since DATE]` reports equity, realized (FIFO), unrealized (live marks), win rate, per-symbol

**Speaker notes:**
60 seconds. P&L is real-time from the trade_updates stream; missed fills are
backfilled by `pl`. Show that the JSONL log is the single source of truth
for audit and that the bot survives restarts with `Restart=always` and 10s
back-off (`deploy/alpacaruns.service`).

---

## Slide 7 — Honest limitations

**Title:** What works, what doesn't, what's measured
**Bullets:**
- **Options overlay degrades to equity on free tier** — `/v2/options/snapshots` 404s, bot logs skip and falls through to the equity entry
- Pre-hackathon account `PA3LNUDV231J` (created 2026-08-25, ineligible) replaced with a fresh post-2026-08-28 paper account — P&L below is a fresh series
- All results paper-only; no live capital traded
- Local-LLM tool calling depends on chat template (`--jinja` for Qwen3); malformed tool calls remain the top local-model failure mode
- No backtesting engine; thresholds validated by unit tests + live paper runs

**Speaker notes:**
45 seconds. Don't gloss this. The audience will ask about P&L; the right
answer is "fresh series, no historical claims." The options 404 is the real
demo-worthy limitation because the bot still trades — it just degrades
gracefully.

---

## Slide 8 — Where to look

**Title:** Repo map + reproduction
**Bullets:**
- `agents/` — ADK graph, MCP server spawn, guarded order tools
- `strategy/ensemble/` — six experts, gater, risk budget, performance tracker
- `risk/risk.go` — single validator for all paths, fail closed
- `tools/alpaca.go` + `options/options.go` — typed clients
- `pnl/pnl.go` — JSONL journal + FIFO realized + live-mark unrealized
- `deploy/DEPLOY.md` — systemd unit, Dockerfile, environment reference
- `data/trades.jsonl` — every fill, every decision, every reconcile snapshot

**Speaker notes:**
30-second closer. Tell them where to read first if they have 5 minutes:
`COMPETITION.md` for the compliance table, then `risk/risk.go` for the gate,
then `strategy/ensemble/runner.go` for the MoE flow. Demo the JSONL log
live: `tail -f data/trades.jsonl | jq` shows fills arriving on `trade_updates`.