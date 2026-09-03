# Deploying Alpacaruns for 24/7 operation

Paper trading is the default and must stay that way unless you *deliberately*
point `ALPACA_BASE_URL` at the live API **and** set `I_ALPACA_LIVE=YES`.
Config refuses to start a live-URL session without that acknowledgement.

## What runs

- `alpacaruns monitor` — the long-running process for 24/7 operation.
  - SIGINT/SIGTERM (or systemd stop) sets the kill switch: no NEW trades are
    started; in-flight order polling finishes before exit. Nothing is
    cancelled mid-order.
  - A panic in the monitoring loop is recovered and logged; the process keeps
    running (`[monitor] recovered from panic: ...` in the logs).
  - On boot it reconciles local books against live Alpaca positions and
    appends a `reconcile` record (with any drift) to the trade log.
  - Two live streams run alongside the monitor loop: the market-data
    stream (feed per `STREAM_FEED`, standard watchlist) ingests
    trades/quotes/bars into the logs, and the account trade_updates
    stream journals each order fill to the trade log the moment it
    arrives — P/L is live without waiting for `pl`'s backfill. Fill
    journaling deduplicates by Alpaca order ID, so fills already recorded
    by a cycle or an earlier backfill are never double-counted. A stream
    that fails to connect or dies permanently is logged and skipped; the
    monitor loop keeps running and `pl --since ...` backfill remains the
    safety net.
- `alpacaruns auto` — the deterministic no-LLM trading loop (see
  [Deterministic auto mode](#deterministic-auto-mode-no-llm) below).
  Runs 24/7 like `monitor`; SIGINT/SIGTERM stops it cleanly and the
  persisted TP/SL state file survives restarts.

## Trade log / P/L

Every fill, decision, and reconcile snapshot is appended to a JSONL file,
default `data/trades.jsonl` (override with `TRADE_LOG`). It is created on
first run and survives restarts; order fills are deduplicated by Alpaca
order ID, and `pl` backfills any fills missed while the process was down.

Inspect P/L at any time (works even while `monitor` runs):

```bash
alpacaruns pl                      # all-time
PL_SINCE=2026-08-01 alpacaruns pl  # since a date via env
alpacaruns pl --since 2026-08-01   # or via flag (RFC3339 also accepted)
```

### Manual one-shot orders

```bash
alpacaruns trade --symbol TSLA --side buy --qty 1
alpacaruns trade --symbol TSLA --side sell --qty 1 --limit 250.50 --tif day --extended-hours
alpacaruns trade --symbol AAPL --side buy --notional 500          # USD-sized instead of shares
```

`trade` bypasses the LLM entirely but goes through the exact same
deterministic risk validator as the agent path (kill switch, notional and
portfolio-percentage caps, market-hours/extended-session rules), journals
the decision to the trade log, and exits nonzero on any rejection.

Prints equity, realized P/L (FIFO), unrealized P/L (live marks), win rate,
and per-symbol breakdown.

### Options (calls and puts)

Paper-trading support for US equity options is built in. Orders use the
same `POST /v2/orders` endpoint with the OCC contract symbol
(e.g. `AAPL240119C00100000`) in the `symbol` field; the client and the
risk validator both enforce Alpaca's options rules before anything
reaches the API:

- qty is whole contracts only; **no notional sizing**
- time in force is **day or gtc only** (no ioc/fop)
- **extended-hours must be off** (options trade regular hours only)
- risk-gate exposure = premium outlay = `qty x 100 x mark price`
  (`MAX_POSITION_USD` therefore caps premium spent per idea, and the
  mark comes from the options snapshot quote mid / last trade; a price
  fetch failure rejects the order — fail closed)

CLI:

```bash
# inspect a chain with quotes, greeks and implied vol (read-only)
alpacaruns chain --symbol AAPL --exp 2026-09-18 --type call

# buy 2 contracts through the same risk validator as every other path
alpacaruns trade --occ AAPL240119C00100000 --side buy --qty 2 --tif day
# OCC format in --symbol is auto-detected, so this is identical:
alpacaruns trade --symbol AAPL240119C00100000 --side buy --qty 2 --limit 5.50
```

Both commands journal to the trade log (`cli:chain` / `cli:trade`). The
autonomous agent path trades options through the same Alpaca MCP order
tools: OCC symbols pass through the identical risk gate, so `MODE=autonomous`
with the paper URL works end-to-end for options. Option positions appear
in `/v2/positions` like equities, so `pl`, reconciliation and fill
journaling work unchanged.

## Deterministic auto mode (no LLM)

`alpacaruns auto` is the fully autonomous, LLM-free execution loop:
data ingestion, factor scoring, threshold decision, timed execution
windows, fixed-fractional sizing with TP/SL brackets — across equities,
crypto and options. Every order passes `agents.NewValidator`, so all
position caps and kill-switch rules apply identically to the agent path.
Each decision is journaled to the trade log (`strategy:auto`) for audit.

- Equities trade only inside `TRADING_WINDOWS` (US/Eastern); crypto is a
  24/7 market and uses `CRYPTO_WINDOWS`.
- Equity entries carry server-side OCO brackets (`order_class=bracket`);
  Alpaca does not support brackets on crypto, so crypto TP/SL levels are
  persisted to `data/strategy-state.json` (next to the trade log) and
  enforced locally every tick. Missing equity protective stops are
  detected via open orders and rebuilt automatically after restarts.
- Drawdown halts: daily ≥ 5%, weekly ≥ 10%, total ≥ 15% of peak equity
  engage the shared kill switch; nothing further trades until restart.

```bash
alpacaruns auto --once      # single pass (cron/systemd-timer friendly)
alpacaruns auto --dry-run   # decisions logged, no orders placed
```

## Docker

```bash
cd deploy
docker build -t alpacaruns -f Dockerfile ..
docker run -d --name alpacaruns --restart unless-stopped \
  --env-file ../.env \
  -v alpacaruns-data:/app/data \
  alpacaruns monitor
```

The final image is distroless, non-root, and contains only the binary;
state lives in the `/app/data` volume so the trade log survives container
replacement.

## systemd

```bash
sudo useradd -r -s /usr/sbin/nologin alpacaruns
sudo mkdir -p /opt/alpacaruns/data && sudo chown -R alpacaruns:alpacaruns /opt/alpacaruns
# copy your binary, .env, and unit file into place:
sudo cp bin/alpacaruns /usr/local/bin/alpacaruns
sudo cp .env /opt/alpacaruns/.env && sudo chown root:alpacaruns /opt/alpacaruns/.env && sudo chmod 640 /opt/alpacaruns/.env
sudo cp deploy/alpacaruns.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now alpacaruns
journalctl -u alpacaruns -f     # watch logs
```

`Restart=always` + `RestartSec=10` brings the monitor back after crashes or
reboots; the boot-time reconciliation re-syncs state from Alpaca, and missed
fills are backfilled into the trade log on the next `pl`/boot.

## Environment reference (additions)

| Variable | Default | Meaning |
|---|---|---|
| `TRADE_LOG` | `data/trades.jsonl` | JSONL trade log path |
| `PL_SINCE` | _(all time)_ | default window for `pl` |
| `EXTENDED_HOURS` | `false` | allow limit/day\|gtc orders during US extended sessions (04:00–20:00 ET weekdays) when market closed |
| `PRE_ORDERS` | `false` | while market closed, queue equity BUYs as resting limit/GTC brackets at the last daily close (one per symbol, deduped against open orders) |
| `MIN_CONFIDENCE` | `0.7` | validator floor for autonomous entries — must be ≤ the strategy's typical verdict confidence or every entry bounces (ensemble ceiling ≈0.60) |
| `STREAM_FEED` | `iex` | market-data websocket feed: `iex` (free) or `sip` (paid subscription) |
| `POLL_SECONDS` | `300` | seconds between monitor ticks (overridable per-run with `monitor --interval`) |
| `MODE` | `supervised` | `autonomous` = risk pass AND confidence ≥ `MIN_CONFIDENCE` proceeds without approval |
| `I_ALPACA_LIVE` | unset | must be `YES` to allow a non-paper base URL — never set this for paper operation |
| `STRATEGY_EQUITY_SYMBOLS` | `AAPL,MSFT,NVDA` | equity universe scored by the `auto` loop |
| `STRATEGY_CRYPTO_SYMBOLS` | `BTC/USD,ETH/USD` | crypto universe (BASE/QUOTE symbols) |
| `TRADING_WINDOWS` | `09:35-10:15,15:45-16:00` | equity/options execution windows, HH:MM-HH:MM comma-separated, US/Eastern; orders fire ONLY inside a window |
| `CRYPTO_WINDOWS` | `00:00-23:59` | crypto execution windows (crypto trades 24/7, independent of equity hours) |
| `POSITION_PCT` | `0.05` | fixed-fractional sizing: budget = portfolio × pct, capped by `MAX_POSITION_USD`; qty < 1 is skipped |
| `TP_PCT` | `0.08` | take-profit above entry (brackets require TP_PCT > SL_PCT > 0) |
| `SL_PCT` | `0.04` | stop-loss below entry |
| `TREND_BUY` | `0.7` | minimum trend factor score for a BUY signal |
| `MOMENTUM_BUY` | `0.6` | minimum momentum factor score for a BUY signal |
| `EXIT_COMPOSITE` | `0.4` | composite at/below this exits a held position |
| `EXIT_MOMENTUM` | `0.35` | momentum at/below this exits a held position |
| `OPTIONS_ENABLED` | `true` | deep-ITM options overlay replacing equity BUYs when data allows |
| `OPTION_DELTA_MIN` / `OPTION_DELTA_MAX` | `0.60` / `0.70` | absolute delta band for overlay contracts |
| `OPTION_MIN_DTE` / `OPTION_MAX_DTE` | `30` / `45` | days-to-expiry window for overlay contracts |
| `DAILY_DD_HALT` | `0.05` | daily realized drawdown fraction engaging the kill switch |
| `WEEKLY_DD_HALT` | `0.10` | weekly realized drawdown fraction engaging the kill switch |
| `TOTAL_DD_HALT` | `0.15` | total drawdown from peak equity engaging the kill switch |
| `ENSEMBLE_ENABLED` | `false` | switch the `auto` tick path to the Layer-2 multi-expert ensemble (see README) |
| `MIN_ENSEMBLE_CONFIDENCE` | `0.55` | minimum normalized winning vote mass for an ensemble action |
| `ENSEMBLE_BENCHMARK` | `SPY` | vol-regime reference symbol (ATR percentile + realized-vol VIX proxy) |
| `ENSEMBLE_PAIRS` | `SPY/QQQ,XLE/XLF` | comma-separated LEG_A/LEG_B ratio pairs, long-only stat-arb voice |
| `RISK_PCT_PER_TRADE` | `0.01` | portfolio fraction risked per entry at a 2×ATR stop distance |
| `ENSEMBLE_MAX_CORRELATION` | `0.85` | block buys averaging above this return-corr to held positions |
| `ENSEMBLE_LIQUIDITY_PCT` | `0.01` | entry notional cap vs 20-day average dollar volume |
| `ENSEMBLE_PERF_WINDOW` | `30` | trailing resolved signals behind each expert's hit-rate weight |

Strategy settings are parsed inside `strategy/settings.go`, reading
`os.Getenv` strictly after `config.Load` — which materializes the selected
`.env` into process environment variables — because `config/config.go` is
owned by a separate workstream and strategy knobs stay self-contained.
