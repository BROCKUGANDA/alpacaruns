# Alpacaruns

> Mixture-of-Experts autonomous paper-trading bot (Go + Alpaca) with an
> embedded live dashboard, deterministic factor pipeline, and an
> optional six-expert ensemble layer.

**Paper trading only by default.** All orders go to
`https://paper-api.alpaca.markets/v2` until you explicitly change
`ALPACA_BASE_URL` and set `I_ALPACA_LIVE=YES`.

- **Code:** `https://github.com/BROCKUGANDA/alpacaruns`
- **Live demo:** `https://run.svalley.tech/` *(Cloudflare-fronted;
  same origin serves the dashboard and the Go JSON API)*
- **API surface:** `GET /api/health`, `GET /api/status`,
  `GET /api/account`, `GET /api/pnl`, `GET /api/trades`,
  `GET /api/decisions`, `GET /api/positions`,
  `POST /api/control/{pause,resume,step}`

## What this is

A single Go binary that:

1. **Polls** Alpaca market data on a configurable interval
   (`POLL_SECONDS`).
2. **Scores** each configured symbol with the factor engine
   (`factors.Engine`) — trend, momentum, volume, volatility, mean-
   reversion, breakout, cross-sectional momentum, seasonality.
3. **Decides** deterministically — no probabilistic text-to-trade
   pipeline; identical inputs always produce identical orders.
5. **Validates** every order against the risk gate (kill switch,
   position caps, drawdown halts, confidence threshold).
6. **Executes** via the official Alpaca REST surface — equities get
   server-side OCO brackets (TP / SL), options pass through the
   same gate, crypto enforces TP/SL locally.
7. **Serves** a Next.js static dashboard from the same binary, on
   the same port, so `serve` is the only process in production.

The optional **layer-2 ensemble** (`ENSEMBLE_ENABLED=true`) layers
six expert voices on top of the same factor engine with a
performance-weighted gater, vol-regime awareness, ATR-normalized
sizing, and a Hold circuit breaker. Off by default; identical behaviour
when unset.

## Quick start (5-minute)

```bash
cp .env.example .env       # add your paper keys

go run ./cmd/alpacaruns auto            # loop: score → decide → execute
go run ./cmd/alpacaruns auto --once     # one pass and exit
go run ./cmd/alpacaruns auto --dry-run  # decisions logged, no orders

go run ./cmd/alpacaruns serve --port 8080 --cors-origin "*"
# open http://localhost:8080/welcome
```

`auto` is the recommended entry point. `monitor` (LLM-gated path)
and `query` (ad-hoc market questions via the ADK gating root) are
also available.

## Live dashboard (`alpacaruns serve`)

A four-page Next.js dashboard is statically exported and embedded
into the Go binary at `api/ui/`:

| Page     | What it shows                                                              |
|----------|---------------------------------------------------------------------------|
| Welcome  | Health badge, splash, "Enter Live Dashboard" CTA.                         |
| Live     | Equity, day P&L, total P&L, open-position count, equity curve, kill-      |
|          | switch badges, live-status dot. Polls `/api/*` every 5–30 s.              |
| Trades   | Cursor-paginated trade log (path, confidence, factor scores, notional).  |
| Brain    | Open positions, recent decision feed, factor weights.                     |
| Controls | Read-only config table + Pause / Resume / Step controls. Optional bearer |
|          | token via localStorage (for cross-origin deployments only).                |

To rebuild the embedded UI after dashboard changes:

```bash
cd dashboard && npm install && npm run build && cd ..
# Rebuild the Go binary so the new api/ui is embedded:
go build -o alpacaruns.exe ./cmd/alpacaruns
```

The dashboard fetches `/api/*` **same-origin** by default. Set
`NEXT_PUBLIC_API_URL` at dashboard build time when hosting the UI
on a different origin than the API (see [Deployment](#deployment)).

## Configuration

Every knob lives in `.env`. The non-secret keys shipped in
`.env.example` are the contract; see that file for the canonical
list. Highlights:

| Variable | Default | Purpose |
|---|---|---|
| `MODE` | `supervised` | `supervised` requires human approval before execution; `autonomous` proceeds if risk passes AND `confidence ≥ MIN_CONFIDENCE`. |
| `POLL_SECONDS` | `300` | Tick interval. |
| `MIN_CONFIDENCE` | `0.7` | Autonomous-mode confidence gate. |
| `MAX_POSITION_USD` | `10000` | Per-position cap. |
| `MAX_PORTFOLIO_PCT` | `0.20` | Portfolio-percentage cap. |
| `DAILY_DD_HALT` / `WEEKLY_DD_HALT` / `TOTAL_DD_HALT` | `0.05 / 0.10 / 0.15` | Drawdown halts (5% / 10% / 15%). |
| `ENSEMBLE_ENABLED` | `false` | Switch the `auto` tick path to the multi-expert ensemble. |
| `MIN_ENSEMBLE_CONFIDENCE` | `0.55` | Minimum normalised winning-vote mass for the gater to act. **Practical ceiling is ≈ 0.54** with all six experts (Hold voices contribute ×0.5 mass against the buy side), so set this **≤ 0.50** in prod or the bot silently never trades. The shipped `.env.example` uses `0.50` for this reason. |
| `BRACKET_MODE` | `pct` | `pct` (entry × TP_PCT / SL_PCT) or `atr` (ATR-multiple brackets). |
| `SWING_ENABLED` | `true` | Master switch for the multi-day equity pipeline. |
| `INTRADAY_TRACK` | `off` | `off` / `shadow` (log only) / `live` (intraday with its own risk budget). |

LLM providers (priority order):

1. **Local llama.cpp** — set `LLM_BASE_URL` (e.g.
   `http://127.0.0.1:8080`) and `LLM_MODEL`. Run
   `llama-server -m Qwen3-4B-Instruct-Q4_K_M.gguf --jinja -c 16384`
   first. `--jinja` is REQUIRED so Qwen3's native tool-call template
   is used.
2. **Oxlo.ai** — set `LLM_PROVIDER=oxlo` and `OXLO_API_KEY`. The
   chosen model is `gpt-oss-20b` (lowest stable latency, correct
   tool-call arguments in every attempt during August 2026 probe).
3. **Gemini** — set `GEMINI_API_KEY`. Used only if both
   `LLM_BASE_URL` is empty and `LLM_PROVIDER` is unset.

## Safety rails

| Control | Mechanism |
|---|---|
| Paper-by-default | `ALPACA_BASE_URL` points at paper API; live requires an explicit config change. |
| Supervised mode | `MODE=supervised` — human approval required before execution. |
| Autonomous gate | `MODE=autonomous` proceeds only if risk passes AND `confidence ≥ MIN_CONFIDENCE`. |
| Hard kill-switch | SIGINT/SIGTERM during `monitor`/`auto` closes the halt channel immediately. |
| Position caps | `MAX_POSITION_USD`, `MAX_PORTFOLIO_PCT`, per-symbol and per-portfolio, enforced deterministically. |
| Drawdown halts | Daily / weekly / total drawdown thresholds engage a shared halt; nothing further trades. |
| Auditability | Every decision (cycle, ensemble, fill, reconcile, panic recovery) is journaled to `data/trades.jsonl`. |

## Architecture

```
                     ┌───────────────────┐
  user / tick ─────▶ │  Deterministic    │
                     │  Pipeline (auto)  │
                     └─────────┬─────────┘
                               │
       ┌───────────────────────┼───────────────────────┐
       ▼                       ▼                       ▼
  Factor Engine          Risk Gate              Order Executor
  (trend/momentum/       (kill switch,          (Alpaca REST;
   volume/volatility/     position caps,         OCO brackets
   + ensemble: 6          drawdown halts,        for equities;
   experts w/ gater)      confidence)            crypto enforces
                                                 TP/SL locally)
       │                       │                       │
       └───── all state ───────┴──── journaled ────────┘
              ▼
        data/trades.jsonl
              │
              ▼ (same binary, same port)
       ┌──────────────────────┐
       │  Next.js Dashboard   │  embedded static export
       │  (welcome / live /   │  served at "/welcome",
       │   trades / brain /   │  "/live", "/trades",
       │   controls)          │  "/brain", "/controls"
       └──────────────────────┘
```

## Tests

```bash
go test ./...
```

Unit tests mock the Alpaca HTTP surface (order placement/cancel,
bars, errors) and cover config defaults, mode validation, ensemble
math, and the risk validator.

> **Pre-existing failures**: `risk` + `strategy` tests fail
> identically on the pristine tree — not introduced by recent
> changes. Tracked as a separate hardening backlog; touch only with
> a reproducer.

## Deployment

See [`deploy/DEPLOY.md`](deploy/DEPLOY.md) for the full 24/7
bring-up. Short version:

1. Build: `go build -o alpacaruns.linux ./cmd/alpacaruns`
2. Ship to your host (systemd unit template at
   `deploy/alpacaruns.service`).
3. Run behind a reverse proxy or a Cloudflare Tunnel for HTTPS.

### Cloudflare Tunnel (recommended for demo)

The live demo at `run.svalley.tech` is fronted by a Cloudflare
Tunnel so the browser hits `https://run.svalley.tech/api/...` on
the same origin as the dashboard — no mixed-content failures, no
CORS hops, automatic TLS.

To set up a similar tunnel:

```bash
cloudflared tunnel login                                    # one-time
cloudflared tunnel create alpacaruns-demo
cloudflared tunnel route dns alpacaruns-demo run.example.com

# Run locally; cloudflared connects outbound to Cloudflare's edge.
cloudflared tunnel run alpacaruns-demo \
  --url http://localhost:8080     # or use a config file:
#   tunnel: alpacaruns-demo
#   credentials-file: ~/.cloudflared/<UUID>.json
#   ingress:
#     - hostname: run.example.com
#       service: http://localhost:8080
#     - service: http_status:404
```

The Go server's CORS middleware is configured via
`--cors-origin` (default `*`); same-origin browser fetches from the
embedded dashboard are always allowed, so a default CORS of `*` is
fine for the demo.

## Paper → Live (DO NOT skip this warning)

Live trading loses real money. To promote:

1. Change `ALPACA_BASE_URL` to `https://api.alpaca.markets/v2`.
2. Swap in live keys.
3. Set `I_ALPACA_LIVE=YES` (config refuses to start a live session
   without this acknowledgement).
4. Review every risk limit.

There is no separate "promote" flag by design — the change must be
deliberate in three places.

## Layout

```
agents/             MoE agent graph (LLM-gated paths; reference)
config/             env loading, mode/confidence/risk settings
tools/              legacy direct REST client + tests (reference)
factors/            factor engine (trend/momentum/volume/volatility/...)
strategy/           multi-expert ensemble + runner (optional layer 2)
pnl/                P/L aggregation from trades.jsonl
risk/               deterministic pre-trade validator + kill switch
stream/             market-data + trade-updates streams
options/            options overlay (deep-ITM calls on BUY)
cmd/alpacaruns/     CLI: cycle | monitor | query | auto | serve | pl | trade
api/                dashboard HTTP server (handlers, security, embed)
  ui/               embedded Next.js static export (build artifact)
deploy/             systemd units + Dockerfiles + DEPLOY.md
docs/               hackathon writeup, hardening audit, slide decks
```

## License

MIT — see [`LICENSE`](LICENSE).