# Alpacaruns Production Hardening Audit — 2026-08-26

Scope: `auto` deterministic loop (swing + new intraday track), executor, position
monitor, risk validator, state/journal durability. All gates re-run live this
session; every FIXED item below was verified green after the change.

## Verdict

**CONDITIONALLY SHIP-READY for paper trading.** Core money paths are sound:
single-threaded event loop (no data races on state), atomic state writes,
fail-closed validator, kill switch shared across all order paths, drawdown
halts computed from journal+live equity. Two real money-path defects were
found and fixed this pass (below). Live-money trading remains gated behind
`I_ALPACA_LIVE=YES` by design.

## Gates run live (this session)

| Gate | Result |
|---|---|
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `go test ./... -count=1` | PASS (11 packages) |
| Linux cross-build + sha256-pinned deploy | PASS (`2e3e9db9…`) |
| Live service restart + first-tick behavior | PASS (dedup logs correct) |

## Findings

### FIXED (verified)

1. **Double-close risk via non-idempotent client_order_id** —
   `strategy/monitor.go` embedded `time.Now().Unix()` in TP/SL close and
   SL-rebuild order IDs. A retry after a network timeout produced a *fresh*
   ID; Alpaca idempotency never engaged; a duplicate market-sell on a paper
   account opens an accidental short. Both IDs are now deterministic per
   (symbol, reason/level, qty) so retries dedupe broker-side.
2. **Sub-penny bracket prices rejected by Alpaca** (found live, Aug 26):
   `ComputeBrackets` emitted 4-decimal prices → HTTP 422
   `sub-penny increment`. Now penny-rounded via `round2`. This bug would
   have blocked every equity entry once signals cleared the gater.
3. **`tools.Client.GetBars` returned zero bars** when callers omitted
   `start` (Alpaca returns an empty page). Injected default 8-month
   lookback at the client layer — fixes every consumer, including the
   validator's factor gate which previously failed closed on all closed-
   market orders.
4. **Bar decoder could not parse Alpaca v2 timestamps** — RFC3339 strings
   into an int64 field hard-failed JSON decode. Custom `UnmarshalJSON` now
   accepts RFC3339 or numeric epochs plus float volumes (v1beta3 crypto).

### MITIGATED (verified present, no change needed)

- **Kill-switch coverage**: every order path (entries, exits, TP/SL closes,
  pre-orders, options) validates through the single shared kill switch.
- **State durability**: `strategy-state.json` written temp+rename under
  mutex; versioned; corrupt-file errors surface instead of silently
  resetting.
- **Drawdown halts**: daily/weekly/total vs peak equity engage the kill
  switch before any entry decision each tick.
- **Fail-closed posture**: unknown clock states, missing snapshots, scorer
  outages all reject orders rather than trade blind.
- **Secrets**: `.env` is root:alpacaruns 0640 outside any repo tree;
  unit file runs as dedicated user with `ProtectSystem=strict`.

### STILL RED / OPEN

1. **OPEN — no alerting channel**: halts and crashes land only in journald.
   Until a webhook/email notifier is wired, a crash-looping bot is silent.
   Recommended: systemd `OnFailure=` hook to a messaging webhook (small task).
2. **OPEN — journal grows unbounded** (`data/trades.jsonl`). Rotation or
   compaction needed for multi-month uptime; disk currently not a constraint.
3. **OPEN — ensemble forced-HOLDs log nothing** (known from root-cause
   session). One-line fix candidate in `runner.go`; deferred to keep this
   pass scoped to money-path defects.
4. **MITIGATION GAP (accepted)** — intraday EOD flatten depends on the bot
   being alive near close; a dead process cannot flatten. Standard caveat
   for local enforcement; server-side brackets cover equities regardless.

## Capabilities added this pass (verified)

- **Intraday track** (`INTRADAY_TRACK=off|shadow|live`, default off):
  independent engine over `INTRADAY_EQUITY_SYMBOLS`, 15-minute bars
  (`factors.Options.Timeframe`), own windows/poll/sizing knobs, optional
  end-of-day flatten. Shadow mode = full signal pipeline, dry-run logging,
  zero orders.
- **ATR brackets** (`BRACKET_MODE=atr`, `ATR_TP_MULT=3.0`,
  `ATR_SL_MULT=1.5`): volatility-scaled swing stops with automatic pct
  fallback when bars are unavailable. Pure functions unit-tested; ATR14
  helper shared.
- **Swing track** made explicit (`SWING_ENABLED=true` default) — legacy
  daily-bar path unchanged when all new knobs are left at defaults.

## Deploy state

- Binary `2e3e9db9…` live on otemaach, service active, pre-order dedup
  verified post-restart.
- Prod env additions: `INTRADAY_TRACK=shadow`,
  `INTRADAY_EQUITY_SYMBOLS=SPY,QQQ,AAPL,MSFT,NVDA,TSLA`.
- Rollback: `/opt/alpacaruns/alpacaruns.bak-*`, `.env.bak-*` retained.
