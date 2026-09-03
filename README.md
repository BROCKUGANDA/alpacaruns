# Alpacaruns — Autonomous MoE Multi-Agent Trading System (Go + Google ADK + Alpaca)

A Mixture-of-Experts style multi-agent system built on **Google ADK for Go**
(`google.golang.org/adk`) that connects to **Alpaca's Trading and Market Data
APIs** via **Alpaca's official MCP server** (`uvx alpaca-mcp-server`).

**Paper trading only by default.** All orders go to
`https://paper-api.alpaca.markets/v2`.

## Architecture

```
                    ┌──────────────┐
   user / tick ───▶ │  GatingRoot  │  (dynamic LLM routing)
                    └──────┬───────┘
        ┌──────────────────┼──────────────────┐
        ▼                  ▼                  ▼
  NLQueryExpert      TradingCycle        MonitorLoop
  (ad-hoc queries)   (SequentialAgent)   (LoopAgent)
                          │                   │
              ┌───────────┴────────┐    HaltableRiskGate
              ▼                    ▼    (exit_loop on breach)
       MarketDataExpert       TradeIdeaExpert
              │                    ▲
              ▼                    │
      AnalysisParallel         RiskManagementExpert
      (ParallelAgent)                │
       ┌─────────┴─────────┐         ▼
       ▼                   ▼   ExecutionExpert
 TechnicalAnalysis   SentimentNews  (paper orders only)
```

- **GatingRoot** — LLM gating network; routes NL questions, cycles, monitoring.
- **TradingCycle** — deterministic `SequentialAgent`:
  MarketData → (Technical ∥ Sentiment) → TradeIdea → Risk → Execution.
- **AnalysisParallel** — `ParallelAgent`; technical + sentiment run concurrently.
- **MonitorLoop** — `LoopAgent`; polls risk each tick and self-exits via
  `exit_loop` when limits are breached.
- Every Alpaca call (bars, quotes, snapshots, news, account, positions,
  paper orders) is a tool from Alpaca's official MCP server — no hand-rolled
  REST logic duplicated here.

## Safety rails

| Control | Mechanism |
|---|---|
| Paper-by-default | `ALPACA_BASE_URL` points at paper API; live requires an explicit config change |
| Supervised mode | `MODE=supervised` — human approval required before execution |
| Autonomous gate | `MODE=autonomous` proceeds only if risk passes AND `confidence ≥ MIN_CONFIDENCE` |
| Hard kill-switch | Ctrl+C during `monitor` closes the halt channel immediately |
| Position caps | `MAX_POSITION_USD`, `MAX_PORTFOLIO_PCT` enforced by RiskManagementExpert |
| Auditability | Each expert's contribution is visible in the event stream per idea |

## Prerequisites

- Go 1.22+
- Python 3.10+ with [uv](https://docs.astral.sh/uv/) (`uvx` on PATH) — required by Alpaca's MCP server
- One of:
  - **Local LLM (default):** llama.cpp `llama-server` running with a Qwen3 GGUF, or
  - Gemini API key (`GEMINI_API_KEY`)
- Alpaca **paper** keys

## Local llama.cpp setup (Qwen3)

```bash
# --jinja is REQUIRED for Qwen3 tool calling: it enables the model's own
# chat template, which serializes tool calls correctly. Mismatched templates
# are the most common cause of malformed tool calls in local agent setups.
llama-server -m Qwen3-4B-Instruct-Q4_K_M.gguf --jinja -c 16384 --port 8080
```

**Recommended model:** Qwen3-4B-Instruct **Q4_K_M** GGUF (default above).
Per the **February 2026 Local Agent Bench** (Mike Veerman's 21-model
tool-calling benchmark for local models), the Qwen3 family leads the
sub-4B class — qwen3:4b tied #1 on agent score — making it the most
reliable tool-caller for CPU-only llama.cpp hosts at this size.
`--jinja` stays REQUIRED: it activates Qwen3's native tool-call chat
template. If the host has >= 12GB RAM, step up to
**Qwen3-8B-Instruct-Q4_K_M** for stronger reasoning at similar tool-call
reliability.

### About the bundled `qwen3-7b-instruct-q4_k_m.gguf`

The local file `C:\Users\HP\Models\qwen3-7b-instruct-q4_k_m.gguf` (4.4 GB)
is misnamed: its GGUF header metadata identifies
`general.architecture=qwen2`, `general.name="Qwen2.5 7B Instruct"` — it is
a Qwen2.5-based 7B merge (Coder + Instruct + Math lineage), **not** Qwen3.
It serves fine through llama-server (`llama-server -m
qwen3-7b-instruct-q4_k_m.gguf --jinja -c 16384 --port 8080`; the
`LLM_MODEL=qwen3.7b-instruct-q4_k_m` name in `.env` is just a label —
llama-server serves whatever file you pass). Context is 32768 per its
metadata, so `-c 16384` fits comfortably. As a qwen2 merge its native
tool-call template support and tool-calling reliability are unproven vs
Qwen3-4B; worth one smoke test via `alpacaruns query "what is AAPL's
latest quote?"`. The recommendation above stays Qwen3-4B-Instruct Q4_K_M;
this 7B is an acceptable fallback.

Then in `.env`:

```
LLM_BASE_URL=http://127.0.0.1:8080
LLM_MODEL=qwen3.7b-instruct-q4_k_m
```

Leave `LLM_BASE_URL` empty to fall back to the Gemini API instead.
The adapter (`model/llamacpp`) implements ADK's `model.LLM` against
llama-server's OpenAI-compatible `/v1/chat/completions`, converting ADK's
function declarations to OpenAI tools and tool_calls back to ADK FunctionCalls.

### Cloud providers

**Oxlo.ai** is supported as a hosted OpenAI-compatible provider through the
same adapter (`model/llamacpp` speaks both llama-server and Oxlo's
`/v1/chat/completions`, including tool calls). Set these env vars:

```
LLM_PROVIDER=oxlo
OXLO_API_KEY=<your key>   # secret; keep in .env (gitignored)
LLM_MODEL=gpt-oss-20b     # optional; this is the default for oxlo
```

**Chosen model: `gpt-oss-20b`.** A live probe of `api.oxlo.ai/v1`
(August 2026) sent a real function-calling request (`get_quote`) to every
small candidate in the catalog. Only three emitted well-formed
`finish_reason=tool_calls` responses — `llama-3.2-3b`, `mistral-7b`, and
`gpt-oss-20b`; `gemma-3-4b` and `llama-3.1-8b` ignored the tools array.
`gpt-oss-20b` had the lowest stable round-trip latency (~1.6–2.4 s,
repeated runs) and correct arguments on every attempt, making it the best
fit for the agent graph's many small tool-calling turns.

Local llama.cpp remains the default: with no `LLM_PROVIDER`, an
`LLM_BASE_URL` selects llamacpp and otherwise Gemini is used when
`GEMINI_API_KEY` is set.

## Quick start

```bash
cp .env .env.local   # or edit .env directly with your keys

# one full trading cycle
go run ./cmd/alpacaruns cycle

# continuous monitoring loop (Ctrl+C = kill switch)
go run ./cmd/alpacaruns monitor

# ad-hoc market question through the gating router
go run ./cmd/alpacaruns query "what is AAPL's latest quote?"
```

## Demo dashboard (`alpacaruns serve`)

A multi-page Next.js dashboard (welcome, live, trades, brain,
controls) is statically exported and embedded into the Go binary at
`api/ui/`. Run it with:

```bash
go run ./cmd/alpacaruns serve --port 8080 --cors-origin "*"
# open http://localhost:8080/welcome
```

The API at `/api/health`, `/api/status`, `/api/account`,
`/api/pnl`, `/api/trades`, `/api/decisions`, `/api/positions`, and
`/api/control/{pause,resume,step}` is the same data the dashboard
fetches. To rebuild the embedded UI from source, see
`dashboard/README.md`.

## Deterministic `auto` mode (no LLM)

`alpacaruns auto` replaces the LLM for execution decisions with a pure,
auditable pipeline: market data in, factor scores computed, threshold
rule applied, sized orders out. Identical inputs always produce identical
decisions. The LLM remains available for `query`/analysis paths only.

```bash
go run ./cmd/alpacaruns auto            # loop: score -> decide -> execute
go run ./cmd/alpacaruns auto --once     # one pass and exit
go run ./cmd/alpacaruns auto --dry-run  # decisions logged, no orders
```

Pipeline per tick (every POLL_SECONDS):

1. **Score** each symbol via the existing multi-factor engine
   (`factors.Engine`; equities from stock bars, crypto from
   `/v1beta3/crypto/us/bars`).
2. **Decide** deterministically — BUY when `composite >= FACTOR_MIN_SCORE`
   AND `trend >= TREND_BUY` AND `momentum >= MOMENTUM_BUY`; SELL a held
   position when `composite <= EXIT_COMPOSITE` OR
   `momentum <= EXIT_MOMENTUM`; otherwise HOLD.
3. **Gate on time** — equities/options only trade inside `TRADING_WINDOWS`
   (ET); crypto trades 24/7 inside its own `CRYPTO_WINDOWS`.
4. **Size** fixed-fractional: budget = portfolio × `POSITION_PCT`, capped
   by `MAX_POSITION_USD`; qty = floor(budget / price); skip if < 1.
5. **Risk gate** — every order passes `agents.NewValidator`, so all caps,
   kill-switch and session rules apply exactly as on the LLM path.
6. **Execute** — equities get server-side OCO brackets
   (`take_profit` = entry×(1+TP_PCT), `stop_loss` = entry×(1−SL_PCT)).
   Crypto does NOT support bracket orders on Alpaca, so levels are
   persisted to `data/strategy-state.json` and enforced locally by the
   position monitor (which also rebuilds missing equity stops after
   restarts). Options overlay: an equity BUY may be replaced by a deep-ITM
   call (~60–70 delta, 30–45 DTE) sized so premium ≤ position budget.
7. **Halt** — daily ≥ DAILY_DD_HALT (default 5%), weekly ≥ WEEKLY_DD_HALT
   (10%) or total ≥ TOTAL_DD_HALT (15%) drawdown engages the shared kill
   switch; nothing further trades.

All new env vars are documented in `deploy/DEPLOY.md`.

### Layer-2 ensemble (optional, off by default)

Setting `ENSEMBLE_ENABLED=true` switches the tick path to a multi-expert
ensemble layered on the same factor engine — still fully deterministic,
no LLM:

1. **Bars fetched ONCE** per tick and shared across all experts.
2. **Six expert voices** score every symbol:
   `trend` (the existing factor rule wrapped as an expert), `meanrev`
   (SMA-20 z-score, ranging regimes only), `breakout` (Donchian-20 with
   volume confirmation), `pairs` (cointegration-lite ratio z-score for
   configured pairs, long-only), `xsmom` (cross-sectional 20-day
   momentum: top-3 buy, held bottom-third sell), `seasonality`
   (turn-of-month + day-of-week tilts; capped below trade-triggering
   confidence).
3. **Vol-regime assessment** (`volregime.go`) classifies LowVol /
   RisingVol / Crisis from benchmark ATR percentile and a realized-vol
   VIX proxy.
4. **Performance-weighted gater**: each voice's weight = base weight ×
   (0.5 + trailing hit-rate over its last 30 resolved signals); RisingVol/
   Crisis scales contrarian voices (meanrev/pairs) down ×0.5/×0 and trend
   voices up ×1.25/×1.5, inverted in LowVol. The winning side must clear
   `MIN_ENSEMBLE_CONFIDENCE` (default 0.55) or the gater holds; a
   directional split without 2:1 dominance trips a Hold circuit breaker
   logged loudly.
5. **Risk budget** before execution: ATR-normalized sizing (portfolio ×
   `RISK_PCT_PER_TRADE` ÷ 2×ATR, MIN-ed against the legacy caps so old
   limits still bind), correlation netting (block buys averaging >0.85
   return-correlation to held positions), and a liquidity cap (entry ≤1%
   of 20-day average dollar volume). Pending signals persist in
   `data/ensemble-state.json` and resolve when price moves ≥0.5×ATR
   favorably within 5 sessions.
6. **Execution** reuses the exact existing window-gate / sizing /
   risk-validator / bracket paths (`handleBuy`/`handleSell`). Every
   ensemble decision is journaled with its full per-expert vote trail.

With `ENSEMBLE_ENABLED=false` or unset, behavior is byte-for-byte the
original single-expert loop.

| Variable | Default | Meaning |
|---|---|---|
| `ENSEMBLE_ENABLED` | `false` | switch the `auto` tick path to the multi-expert ensemble |
| `MIN_ENSEMBLE_CONFIDENCE` | `0.55` | minimum normalized winning vote mass to act |
| `ENSEMBLE_BENCHMARK` | `SPY` | vol-regime reference symbol |
| `ENSEMBLE_PAIRS` | `SPY/QQQ,XLE/XLF` | comma-separated LEG_A/LEG_B pairs for the stat-arb voice (long-only) |
| `RISK_PCT_PER_TRADE` | `0.01` | fraction of portfolio risked per entry at a 2×ATR stop |
| `ENSEMBLE_MAX_CORRELATION` | `0.85` | average return-corr ceiling vs held positions for new buys |
| `ENSEMBLE_LIQUIDITY_PCT` | `0.01` | max entry notional as fraction of 20-day average dollar volume |
| `ENSEMBLE_PERF_WINDOW` | `30` | trailing resolved-signal window per expert hit-rate |

## Tests

```bash
go test ./...
```

Unit tests mock the Alpaca HTTP surface (order placement/cancel, bars, errors);
config tests cover paper-defaults and mode validation.

## Paper → Live (DO NOT skip this warning)

⚠️ **Live trading can lose real money.** To promote: change `ALPACA_BASE_URL`
to `https://api.alpaca.markets/v2` AND swap in live keys AND review every risk
limit. There is no separate "promote" flag in this scaffold by design — the
change must be deliberate in two places.

## Layout

```
agents/       MoE agents + Sequential/Parallel/Loop wiring + MCP bridge
config/       env loading, mode/confidence/risk settings
tools/        legacy direct REST client + tests (kept for reference/tests)
cmd/alpacaruns/  CLI: cycle | monitor | query
```
