# Alpacaruns

> A deterministic, paper-first multi-asset trading bot for Alpaca with an
> embedded dashboard, a universal signal picker, and a controlled
> agent-to-agent bridge.

**Paper trading only by default.** Every order goes to
`https://paper-api.alpaca.markets/v2` unless you deliberately change
`ALPACA_BASE_URL` **and** set `I_ALPACA_LIVE=YES`. The config loader
refuses to start a live session without that acknowledgement.

- **Code:** `https://github.com/BROCKUGANDA/alpacaruns`
- **Language:** Go 1.26 (single binary, no runtime dependencies) + a
  Next.js dashboard exported into the binary.

---

## Table of contents

1. [What this is](#1-what-this-is)
2. [Architecture](#2-architecture)
3. [The universal signal picker](#3-the-universal-signal-picker)
4. [The agent-to-agent bridge](#4-the-agent-to-agent-bridge)
5. [Safety rails](#5-safety-rails)
6. [Configuration](#6-configuration)
7. [CLI](#7-cli)
8. [HTTP API](#8-http-api)
9. [Dashboard](#9-dashboard)
10. [Data flow and state files](#10-data-flow-and-state-files)
11. [Deployment](#11-deployment)
12. [Tests and gates](#12-tests-and-gates)
13. [What was fixed in this pass](#13-what-was-fixed-in-this-pass)
14. [Layout](#14-layout)

---

## 1. What this is

One Go binary that:

1. **Builds a classified universe** — equities, ETFs, index trackers,
   option contracts and crypto pairs, each with the venue, data family
   and execution window it needs (`picker/`).
2. **Ingests the right data per class** — stock bars for equities/ETFs/
   indices, crypto bars for pairs, option chains with greeks for
   contracts.
3. **Scores every instrument** with a five-factor engine (trend,
   momentum, volatility, volume, news attention) and folds the factors
   into a weighted composite.
4. **Decides deterministically** — identical inputs always produce
   identical orders. There is no probabilistic text-to-trade pipeline.
5. **Validates every order** against a code-enforced risk gate (kill
   switch, position caps, portfolio-percentage cap, drawdown halts,
   confidence floor, market hours, execution windows).
6. **Executes** through the official Alpaca REST surface — equities get
   server-side OCO brackets, crypto gets locally enforced TP/SL, options
   are whole contracts only.
7. **Serves a dashboard** from the same binary on the same port.

Two optional layers sit on top:

- **The LLM agent graph** (`agents/`) — a Google ADK mixture-of-experts
  cycle that reaches Alpaca through the official MCP server. Every
  order it proposes goes through the same deterministic gate.
- **The agent-to-agent bridge** (`a2a/`) — a controlled ingress for
  signals from *other* agents, with an on/off ramp that has to be
  earned before anything is traded.

---

## 2. Architecture

```
                    ┌──────────────────────────┐
   operator ──────▶ │  Dashboard (Next.js)     │  embedded static export
   peer agents ───▶ │  + JSON API              │  served by the Go binary
                    └────────────┬─────────────┘
                                 │
        ┌────────────────────────┼────────────────────────┐
        ▼                        ▼                        ▼
 ┌─────────────┐        ┌───────────────┐        ┌────────────────┐
 │  Picker     │        │  A2A bridge   │        │  LLM agent     │
 │  (universe  │        │  (ramp off/   │        │  graph (ADK +  │
 │   + scores) │        │   shadow/on)  │        │  Alpaca MCP)    │
 └──────┬──────┘        └───────┬───────┘        └────────┬───────┘
        │  symbols + scores     │  signals               │  proposals
        └───────────┬───────────┴───────────┬────────────┘
                    ▼                       ▼
             ┌────────────────────────────────────┐
             │  Deterministic risk gate           │
             │  kill switch · caps · DD halts ·   │
             │  confidence · hours · windows      │
             └──────────────────┬─────────────────┘
                                ▼
             ┌────────────────────────────────────┐
             │  Executor (Alpaca REST)            │
             │  brackets · local TP/SL · contracts │
             └──────────────────┬─────────────────┘
                                ▼
                    data/trades.jsonl  (every fill,
                    decision, reconcile, bridge event)
```

### Packages

| Package | Responsibility |
|---|---|
| `cmd/alpacaruns` | CLI: `auto`, `serve`, `cycle`, `monitor`, `query`, `trade`, `chain`, `factors`, `pl` |
| `picker/` | The universal signal picker: instrument registry, class-aware data ingestion, ranked candidates |
| `a2a/` | The agent-to-agent bridge: signal ingress, ramp state machine, persisted state |
| `agents/` | LLM agent graph (ADK) + the Alpaca MCP tool surface with the risk guard |
| `risk/` | The deterministic pre-trade validator |
| `strategy/` | The deterministic engine: scoring, decisions, brackets, execution, monitoring, TP/SL state |
| `strategy/ensemble/` | Optional multi-expert layer (trend, mean-reversion, breakout, pairs, cross-sectional momentum, seasonality) |
| `factors/` | The five-factor scoring engine |
| `options/` | Option chain, greeks and snapshot client |
| `tools/` | Alpaca REST client (trading + market data) |
| `stream/` | Market-data and trade-update websocket streams |
| `pnl/` | The JSONL journal and FIFO P/L |
| `api/` | HTTP API + embedded dashboard |
| `config/` | `.env` loading and validation |

---

## 3. The universal signal picker

Before the picker existed the universe was two flat lists — equities and
crypto — and the asset class was sniffed from the symbol text
("contains a slash → crypto"). ETFs, index trackers and options could
not be configured, discovered or ranked alongside everything else.

The picker replaces sniffing with an explicit classification.

### Classes

| Class | Example | Data family | Window | Notes |
|---|---|---|---|---|
| `equity` | `AAPL` | `stocks` | equity | Server-side OCO brackets |
| `etf` | `SPY` | `stocks` | equity | Alpaca classifies ETFs as `us_equity` |
| `index` | `QQQ`, `IWM` | `stocks` | equity | Index trackers; same mechanics as an ETF |
| `option` | `AAPL260116C00150000` | `options` | equity | Whole contracts, no notional, no extended hours |
| `crypto` | `BTC/USD` | `crypto` | crypto | 24/7, local TP/SL, `BTCUSD` on the trading side |

### Configuring the universe

```bash
STRATEGY_EQUITY_SYMBOLS=AAPL,MSFT,NVDA
STRATEGY_ETF_SYMBOLS=SPY
STRATEGY_INDEX_SYMBOLS=QQQ,IWM
STRATEGY_CRYPTO_SYMBOLS=BTC/USD,ETH/USD
STRATEGY_OPTION_UNDERLYINGS=AAPL,MSFT
```

Option underlyings are expanded into **concrete contracts**: the picker
walks the chain inside the expiry window (`OPTION_MIN_DTE` ..
`OPTION_MAX_DTE`) and picks the contract whose delta sits closest to the
middle of the band (`OPTION_DELTA_MIN` .. `OPTION_DELTA_MAX`).

### What the picker does

1. **Classifies** every configured symbol against the broker's asset
   catalogue (venue, tradability, fractional support, shortability). A
   symbol the broker does not know stays in the universe but is reported
   as *not tradable* rather than silently vanishing.
2. **Fetches the right data** per class — stock bars, crypto bars, or
   option chain snapshots with greeks.
3. **Scores** every instrument with the same factor engine that gates
   every order, and reports the per-factor breakdown.
4. **Ranks** the candidates and labels each `buy` / `hold` / `sell`.

### Reading it

```bash
curl localhost:8080/api/instruments   # the classified universe
curl localhost:8080/api/signals       # ranked candidates + factors
```

The dashboard's **Picker** page shows both, plus the agent bridge.

### Symbol forms

Alpaca is inconsistent about crypto symbols: the market-data API wants
`BTC/USD`, the trading API wants `BTCUSD`. The conversion lives in one
place (`tools.CryptoOrderSymbol` / `tools.CryptoDataSymbol` /
`strategy.CanonicalSymbol`) and the broker's own form is matched
everywhere a position is looked up — otherwise a crypto position never
looked "held", the per-symbol cap never fired, and the bot re-bought it
every tick.

---

## 4. The agent-to-agent bridge

The bridge lets *other* agents (a research process, another model, a
strategy script) publish trade signals into this bot. An inbound signal
is untrusted input: it must not be able to place an order by itself,
and it must not be able to place orders at all until an operator has
ramped it on.

### The ramp

```
        operator                operator
            │                      │
            ▼                      │
        ┌────────┐   signals    ┌────────┐  earned   ┌──────┐
        │  off   │─────────────▶│ shadow │──────────▶│  on  │
        │ refuse │              │ score │  N passes │ trade│
        └────────┘              └────────┘           └──────┘
            ▲                      │                      │
            └──────────────────────┴──────────────────────┘
                    off ramp — always available, never gated
```

| Mode | What happens to an inbound signal |
|---|---|
| `off` | Refused outright. Nothing is queued, nothing is traded. |
| `shadow` | Accepted, scored and journaled — **no order is ever placed**. The shadow streak is what later earns the right to ramp on. |
| `on` | Flows through the deterministic risk gate and the normal executor, exactly like the bot's own decisions. |

Ramping **up** is earned, not switched: an agent needs
`A2A_SHADOW_PASSES` consecutive shadow evaluations that the risk gate
approved. A rejected evaluation resets the streak, so an agent cannot
bank two good signals and then ride a failing one into production.
Ramping **down** is immediate and never gated — that is the off ramp,
and it must never be blocked.

### Configuration

```bash
A2A_ENABLED=true          # the ingress is opt-in
A2A_TOKEN=<bearer token>  # REQUIRED when enabled — there is no anonymous mode
A2A_AGENTS=research,scanner # allow-list of peer agent ids
A2A_MODE=off              # default ramp position for a newly seen agent
A2A_MAX_NOTIONAL=10000    # per-signal USD cap
A2A_SHADOW_PASSES=3       # approved shadow evaluations needed to ramp on
```

### Posting a signal

```bash
curl -X POST http://localhost:8080/api/a2a/signals \
  -H "Authorization: Bearer $A2A_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"id":"sig-1","agent":"research","symbol":"AAPL","side":"buy",
       "qty":"10","confidence":0.82,"reason":"momentum"}'
```

```json
{"accepted": true, "mode": "shadow", "reason": "accepted in shadow mode; no order will be placed"}
```

The `id` is an idempotency key: a replayed signal is dropped, not
traded twice.

### Managing the ramp

```bash
curl localhost:8080/api/a2a/agents                       # every agent + its position
curl -X POST localhost:8080/api/a2a/agents/research/ramp \
  -H "Authorization: Bearer $DASHBOARD_TOKEN" \
  -d '{"mode":"shadow"}'
```

The ramp endpoint uses the **dashboard** token (it is an operator
action); the signal endpoint uses the **bridge** token (it is a peer
agent). The two credentials are deliberately separate.

### What the bot does with a signal

Every tick, the bot drains the queue and evaluates each signal:

1. The signal is checked against the ramp position of its agent.
2. Its size is compared with the per-position cap.
3. It is planned into an entry (mark price, brackets, sizing).
4. **The deterministic risk gate runs** — the same gate every other
   path uses.
5. Approved in `shadow` → journaled as what the bot *would* have done.
   Approved in `on` → executed through the normal executor.
6. The outcome is fed back into the bridge, so the shadow streak
   reflects reality.

Every evaluation — approved, rejected or refused — is journaled to
`data/trades.jsonl` with the source `a2a:<agent>`.

---

## 5. Safety rails

| Control | Mechanism | Where |
|---|---|---|
| Paper by default | `ALPACA_BASE_URL` points at the paper API; a non-paper URL is refused without `I_ALPACA_LIVE=YES` | `config/config.go` |
| Kill switch | SIGINT/SIGTERM halts new trades; drawdown halts engage the same switch | `risk/`, `strategy/monitor.go` |
| Position caps | `MAX_POSITION_USD`, `CRYPTO_MAX_POSITION_USD`, `MAX_PORTFOLIO_PCT`, per-symbol and additive exposure | `risk/risk.go` |
| Drawdown halts | Daily ≥ 5%, weekly ≥ 10%, total ≥ 15% of peak equity | `strategy/monitor.go` |
| Confidence floor | `MIN_CONFIDENCE` — every proposal carries a confidence; the agent path derives it from the deterministic factor composite | `risk/risk.go` |
| Market hours | The clock gate rejects orders outside the session; extended hours only for limit/day|gtc | `risk/risk.go` |
| Execution windows | Orders fire only inside `TRADING_WINDOWS` / `CRYPTO_WINDOWS` (US/Eastern wall clock) | `strategy/windows.go` |
| Balance check | Every entry verifies the account can fund it before the order is placed | `strategy/execute.go` |
| Fail closed | A missing price, an unreadable account or an unreadable clock rejects the order | `risk/risk.go` |
| Sizing by price | A share count is priced into USD before the caps are applied | `risk/risk.go` |
| MCP tool guard | Alpaca MCP tools are classified default-deny: unknown action tools are blocked, order tools go through the risk gate | `agents/mcp.go` |
| Read-only data session | The market-data MCP session is given read-only toolsets, so it cannot place an order at all | `agents/mcp.go` |
| Venue pinning | The MCP child process gets `ALPACA_PAPER_TRADE` derived from the configured base URL | `agents/mcp.go` |
| Auditability | Every fill, decision, reconcile and bridge event is journaled to `data/trades.jsonl` | `pnl/` |

---

## 6. Configuration

Every knob lives in `.env`; `.env.example` is the canonical list.

### Core

| Variable | Default | Meaning |
|---|---|---|
| `ALPACA_API_KEY_ID` / `ALPACA_SECRET_KEY` | — | Credentials (paper by default) |
| `ALPACA_BASE_URL` | `https://paper-api.alpaca.markets/v2` | Trading API |
| `ALPACA_DATA_URL` | `https://data.alpaca.markets/v2` | Market data API |
| `I_ALPACA_LIVE` | unset | Must be `YES` for a non-paper URL |
| `MODE` | `supervised` | `supervised` needs human approval; `autonomous` proceeds on risk + confidence |
| `POLL_SECONDS` | `300` | Tick interval |
| `MIN_CONFIDENCE` | `0.7` | Confidence floor for every order |
| `MAX_POSITION_USD` | `10000` | Per-position cap (equities, ETFs, indices, options) |
| `CRYPTO_MAX_POSITION_USD` | `MAX_POSITION_USD` | Per-position cap for crypto |
| `MAX_PORTFOLIO_PCT` | `0.20` | Max share of the portfolio in one position |
| `DAILY_DD_HALT` / `WEEKLY_DD_HALT` / `TOTAL_DD_HALT` | `0.05` / `0.10` / `0.15` | Drawdown halts |
| `FACTOR_MIN_SCORE` | `0.6` | Composite factor floor, applied on top of `MIN_CONFIDENCE` |
| `FACTOR_WEIGHTS` | trend .30 / momentum .25 / volume .20 / vol .15 / sent .10 | Factor weights (must sum to 1) |
| `TRADE_LOG` | `data/trades.jsonl` | Journal path |

### Universe

| Variable | Default | Meaning |
|---|---|---|
| `STRATEGY_EQUITY_SYMBOLS` | `AAPL,MSFT,NVDA` | Equity universe |
| `STRATEGY_ETF_SYMBOLS` | *(empty)* | ETF universe |
| `STRATEGY_INDEX_SYMBOLS` | *(empty)* | Index-tracker universe |
| `STRATEGY_CRYPTO_SYMBOLS` | `BTC/USD,ETH/USD` | Crypto universe (BASE/QUOTE) |
| `STRATEGY_OPTION_UNDERLYINGS` | *(empty)* | Option underlyings to expand into contracts |
| `OPTION_DELTA_MIN` / `OPTION_DELTA_MAX` | `0.60` / `0.70` | Contract selection band |
| `OPTION_MIN_DTE` / `OPTION_MAX_DTE` | `30` / `45` | Expiry window |

### Execution

| Variable | Default | Meaning |
|---|---|---|
| `TRADING_WINDOWS` | `09:35-10:15,15:45-16:00` | Equity/option execution windows (US/Eastern) |
| `CRYPTO_WINDOWS` | `00:00-23:59` | Crypto execution windows |
| `POSITION_PCT` | `0.05` | Fixed-fractional sizing |
| `TP_PCT` / `SL_PCT` | `0.08` / `0.04` | Bracket levels |
| `BRACKET_MODE` | `pct` | `pct` or `atr` |
| `PRE_ORDERS` | `false` | Queue resting limit/GTC brackets while the market is closed |
| `EXTENDED_HOURS` | `false` | Allow limit/day|gtc equity orders in the extended session |

### Agent bridge

See [the bridge section](#4-the-agent-to-agent-bridge).

### LLM providers

Priority order: `LLM_BASE_URL` (local llama.cpp — use `--jinja` so
Qwen3's own tool-call template is used) → `LLM_PROVIDER=oxlo` with
`OXLO_API_KEY` → `GEMINI_API_KEY`.

---

## 7. CLI

```bash
cp .env.example .env          # add your paper keys

go run ./cmd/alpacaruns auto            # the deterministic loop
go run ./cmd/alpacaruns auto --once     # one pass and exit
go run ./cmd/alpacaruns auto --dry-run  # decisions logged, no orders
go run ./cmd/alpacaruns serve --port 8080 --cors-origin "*"

go run ./cmd/alpacaruns trade --symbol TSLA --side buy --qty 1
go run ./cmd/alpacaruns chain --symbol AAPL --exp 2026-09-18 --type call
go run ./cmd/alpacaruns factors --symbol AAPL
go run ./cmd/alpacaruns pl --since 2026-08-01
```

`auto` is the recommended entry point. `monitor` (the LLM-gated path),
`cycle` (one LLM cycle) and `query` (ad-hoc market questions) are also
available.

---

## 8. HTTP API

Responses carry the bare typed object on success and
`{"error": "...", "code": "..."}` on failure — the HTTP status carries
the outcome, there is no `success` flag.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/health` | Liveness, version, last poll |
| GET | `/api/status` | Bot state, kill-switch badges, runtime config |
| GET | `/api/account` | Live Alpaca account |
| GET | `/api/positions` | Live Alpaca positions |
| GET | `/api/pnl?since=&until=` | Equity curve and summary |
| GET | `/api/trades?symbol=&since=&until=&path=&limit=&cursor=` | Fills |
| GET | `/api/decisions?since=&path=&limit=&cursor=` | Decision events |
| GET | `/api/config` | The live knobs |
| POST | `/api/config` | Update the knobs |
| POST | `/api/trade/simulate` | Dry-run an order through the risk gate |
| POST | `/api/trade/execute` | Place a manual order through the risk gate |
| POST | `/api/control/{pause,resume,step}` | Operator controls |
| GET | `/api/instruments` | The classified universe (the picker) |
| GET | `/api/signals` | Ranked signals with per-factor scores |
| GET | `/api/a2a` | Whether the agent bridge is enabled |
| POST | `/api/a2a/signals` | Inbound signal (bridge token) |
| GET | `/api/a2a/agents` | Agent ramp positions |
| POST | `/api/a2a/agents/{id}/ramp` | Move an agent along the ramp (dashboard token) |

`path=` filters on `trades` and `decisions` accept `auto` (the
deterministic loop), `agent` (the LLM cycle), `ensemble` and `manual`.

### Security posture

- Control endpoints (config, trade, control, ramp) need a bearer
  token, except same-origin browser fetches (CSRF-safe via the
  Origin/Referer host check). Foreign origins are always 403.
- The bridge ingress always needs its own token; there is no anonymous
  mode.
- Rate limits: 60 requests/60 s per IP overall, 20/min on the
  state-changing endpoints.
- Security headers on every response; CSP on the JSON routes; no
  cookies, no sessions, no upload endpoint.
- The account number is masked to the last four digits.

---

## 9. Dashboard

The Next.js app is statically exported and embedded into the binary at
`api/ui/`. Five pages:

| Page | What it shows |
|---|---|
| Welcome | Health badge, splash, CTA |
| Live | Equity, day/total P&L, open positions, equity curve, kill-switch badges |
| Trades | Cursor-paginated trade log with path filters |
| Brain | Open positions, decision feed, factor weights |
| **Picker** | The classified universe, the ranked signals, and the agent bridge with its ramp controls |
| Controls | Read-only config table + Pause / Resume / Step |

Rebuilding after a dashboard change:

```bash
cd dashboard && npm install && npm run build && cd ..
rm -rf api/ui/* && cp -r dashboard/out/* api/ui/
go build -o alpacaruns.exe ./cmd/alpacaruns
```

The dashboard fetches `/api/*` **same-origin** by default. Set
`NEXT_PUBLIC_API_URL` at build time when the UI is hosted on a
different origin.

---

## 10. Data flow and state files

| File | Written by | Read by |
|---|---|---|
| `data/trades.jsonl` | Every path (bot, agent, dashboard, bridge) | P/L, the dashboard, the stats ledger |
| `data/strategy-state.json` | The bot (heartbeat, TP/SL levels, peak equity) | The dashboard, the bot |
| `data/a2a-state.json` | The bridge (ramp positions, counters, pending signals) | The bot, the dashboard |
| `data/strategy-stats.json` | The stats ledger (per-symbol win/loss) | The bot (confidence bias) |
| `data/paused` | The dashboard's pause control | The bot, the API |

The bot and the dashboard are **two separate processes** sharing these
files. That is why the ramp position, the pause flag and the TP/SL
levels all live on disk rather than in memory.

---

## 11. Deployment

See [`deploy/DEPLOY.md`](deploy/DEPLOY.md) for the full bring-up.

### The short version

```bash
go build -o alpacaruns.linux ./cmd/alpacaruns
scp alpacaruns.linux <host>:/usr/local/bin/alpacaruns
scp .env <host>:/opt/alpacaruns/.env        # mode 640, root:alpacaruns
scp deploy/alpacaruns.service deploy/alpacaruns-api.service <host>:/etc/systemd/system/
ssh <host> 'sudo systemctl daemon-reload && sudo systemctl enable --now alpacaruns alpacaruns-api'
```

The bot and the API run as two independent services with the same
hardening posture (`NoNewPrivileges`, `ProtectSystem=strict`,
`ReadWritePaths=` the data dir, `PrivateTmp`, `ProtectHome`).
Restarting one does not restart the other.

### Behind Caddy

```caddy
run.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

The dashboard and `/api/*` share one origin, so there are no
mixed-content or CORS hops.

---

## 12. Tests and gates

```bash
go test ./...   # every package
go vet ./...
```

The suite mocks the Alpaca HTTP surface (order placement, bars,
snapshots, option chains, errors) and covers the config loader, the
risk validator, the factor engine, the state store, the picker
classification and the bridge ramp.

Gates are written so they can fail:

- The sizing test proves a 1,000-share order is priced into USD and
  rejected by the cap (it fails on the old "qty is notional" code).
- The tool-classification test uses the **real** Alpaca MCP tool names
  read off a live MCP session, so it fails if a new order tool is not
  classified.
- The ramp tests prove an agent cannot be ramped on without earning it,
  that a rejected evaluation resets the streak, and that the off ramp
  is never blocked.

---

## 13. What was fixed in this pass

This pass audited the code, the broker layer, the risk gate, the agent
layer and the API, and fixed the defects that mattered. The full list
with file:line evidence is in the commit history; the highlights:

### Critical — the agent path could trade without a risk gate

The Alpaca MCP server exposes `place_stock_order`, `place_crypto_order`
and `place_option_order`. The guard's allow-list contained invented
names (`place_order`), so **every real order tool bypassed the kill
switch, the caps, the market-hours check and the confidence gate**. The
classification is now default-deny and covers all 72 live tools, and
the read-only market-data session is given read-only toolsets so it
cannot place an order at all (verified against a live MCP session: 30
read tools, 0 order tools).

### Critical — a share count was treated as a dollar amount

A qty-only proposal was sized as `notional = qty`, so a 1,000-share
order ($348k) looked like a $1,000 order and passed both caps. Sizes
are now priced into USD before the caps are applied, and an unpriceable
order is rejected.

### High

| Area | Fix |
|---|---|
| Trading windows | `NewTradingWindows` defaulted to UTC, so every window fired 4–5 h off. Now US/Eastern. |
| Entry windows | `handleBuy` never checked the window — entries fired at any hour the market was open. |
| Pause semantics | The pause flag returned before the monitor pass, silently disabling TP/SL, drawdown halts and the EOD flatten. Pause now blocks entries only. |
| Crypto positions | The broker says `BTCUSD`, the strategy says `BTC/USD`: crypto never looked held, the per-symbol cap never fired and the bot re-bought every tick. |
| Option pricing | The options data root was built as `/v2/v1beta1/...` (404) and the chain path had a bogus `/chain` suffix — every option proposal was rejected and the overlay found zero candidates. |
| Live prices | `json:"latestTrade.p"` is not a thing — every snapshot price was silently zero. |
| Quotes | The quote timestamp was typed `int64` while Alpaca sends RFC3339, so every quote request failed to decode. |
| Pagination | `next_page_token` was dropped: histories were truncated to the first page. |
| Fill journaling | The auto loop journalled no fills, so the drawdown halts saw zero realized P/L forever. |
| Decision journaling | `cfgFromMain` was declared and never assigned: the monitor ran with no market-hours gate, no pacing and no audit trail. |
| Stats ledger | The round-trip replay ran per record on a growing prefix and then added the results on top of the persisted cache, inflating win rate permanently. |
| TP/SL state | Levels were keyed by symbol alone, so an intraday entry overwrote the swing book's brackets. Levels are now track-keyed. |
| TP/SL triggers | A zero take-profit counted as "price ≥ TP" and market-sold a fresh position. |
| `preOrder` | `PRE_ORDERS` was documented and implemented but never called. |
| Pre-orders | `preOrder` was never wired into the tick; now a closed market queues a resting bracket instead of dropping the signal. |
| Dashboard auth | The dashboard attached the bearer token only to `/api/control/*`, so every config save and manual order 401'd on a token-protected deployment. |
| Config race | `POST /api/config` mutated the live config while other handlers read it. |
| Market clock | The dashboard's manual-order path had no market-hours check at all. |
| `X-Forwarded-Host` | The docstring promised it, the code never read it: behind a proxy every control POST was 403. |
| `path=auto` | The dashboard's "auto" tab matched nothing and was permanently empty. |
| Asset catalogue | `/v2/assets` ignores `limit`/`offset`; the paging loop re-fetched the full 14k-asset catalogue 50 times per call. |

---

## 14. Layout

```
a2a/                agent-to-agent bridge (ingress, ramp, state)
agents/             LLM agent graph + Alpaca MCP tool guard
api/                HTTP API + embedded dashboard
  ui/               embedded Next.js static export (build artifact)
cmd/alpacaruns/     CLI: auto | serve | cycle | monitor | query | trade | chain | factors | pl
config/             .env loading and validation
dashboard/          Next.js source (app router)
data/               journal, state, stats, pause flag
deploy/             systemd units, Dockerfile, DEPLOY.md
docs/               hackathon writeup, hardening audit, slide decks
factors/            factor engine
model/llamacpp/     llama.cpp / OpenAI-compatible model adapter
options/            options chain, greeks, snapshots
picker/             universal signal picker (registry, ingestion, ranking)
pnl/                JSONL journal + FIFO P/L
risk/               deterministic pre-trade validator
stream/             market-data + trade-update streams
strategy/           deterministic engine (decide, execute, monitor, state, windows)
strategy/ensemble/  optional multi-expert layer
tools/              Alpaca REST client
```

---

## License

MIT — see [`LICENSE`](LICENSE).
