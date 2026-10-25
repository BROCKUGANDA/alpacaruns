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
4. [Position-aware TP/SL](#4-position-aware-tpsl)
5. [The agent-to-agent bridge](#5-the-agent-to-agent-bridge)
6. [Learning from its own trades](#6-learning-from-its-own-trades)
7. [Self-heal](#7-self-heal)
8. [Safety rails](#8-safety-rails)
9. [Configuration](#9-configuration)
10. [CLI](#10-cli)
11. [HTTP API](#11-http-api)
12. [Dashboard](#12-dashboard)
13. [Data flow and state files](#13-data-flow-and-state-files)
14. [Deployment](#14-deployment)
15. [Tests and gates](#15-tests-and-gates)
16. [What was fixed in this pass](#16-what-was-fixed-in-this-pass)
17. [Layout](#17-layout)
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
6. **Protects every position**, not only the ones this process opened.
   The broker's open positions are reconciled against the TP/SL book each
   tick and anchored to their real average entry.
7. **Executes** through the official Alpaca REST surface — equities get a
   resting OCO pair, crypto and options are enforced locally, and options
   are whole contracts only.
8. **Serves a dashboard** from the same binary on the same port.

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
| `learning/` | The learner: bounded score-floor, sizing, stop and quarantine adjustments computed from closed trades |
| `selfheal/` | Degraded-state detection and repair: orphaned and duplicate protective orders, stale resting entries, unreadable state, stuck halts |
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

## 4. Position-aware TP/SL

Protection is driven by **what the broker says is open**, not by what this
process remembers placing. That distinction matters: a bot that only
protects its own entries leaves every other position in the account
riding with no stop — a hand trade from the dashboard, an order from the
agent path, a position that survived a redeploy of the data directory,
or one that was simply already there when the bot started.

Every tick, before any decision is made:

1. **Read the broker's positions.** Each one is matched against the level
   book by venue symbol (`BTCUSD` and `BTC/USD` are the same position).
2. **Adopt or re-anchor.** A position with no levels gets them; a position
   whose stored entry no longer matches the broker's `avg_entry_price`,
   whose quantity moved, or whose side flipped gets rebuilt. Levels are
   anchored to the **actual average entry**, never to the price an entry
   was planned at — a market order prints where the tape is, and
   averaging in moves the basis.
3. **Place the protection where the class allows it.**

   | Class | Where TP/SL live | Why |
   |---|---|---|
   | Equity / ETF / index | A resting `order_class=oco` pair at the broker | A bracket with no entry leg; fills one, cancels the other |
   | Crypto | Locally enforced levels | Alpaca has no order class for crypto |
   | Option | Locally enforced levels | Alpaca has no order class for options either |

4. **Sweep orphans.** A level whose position the broker no longer reports
   is dropped, in that book only.

Direction is part of the model. A long's target sits above the entry and
its stop below; a short is the mirror, and closing it is a **buy**.
Long-style levels on a short put the target above the market, so the
first tick buys the position back at a loss — the level looks present and
is actively wrong.

Two more rules worth stating because they are the ones that bite:

- **A zero level is not a trigger.** A bracket that failed to compute
  leaves zeros behind; treating zero as a threshold would market-sell a
  fresh position on the next tick. Zero means "no local enforcement", and
  the adoption pass gives it a real level.
- **Resting orders are pulled before a local close.** Otherwise the leg
  that survived the fill fires later against a flat account and reopens
  the position on the wrong side.

Protection is idempotent: the deterministic `client_order_id` is derived
from `(symbol, target, stop, qty)`, so a retry after a timeout dedupes at
the broker instead of stacking a second pair, and a position already
carrying an `oco`/`bracket` is left alone. Every adoption, re-anchor,
placement and refusal is journaled to the trade log under
`strategy:protect`, so "why is this guarded at this price?" is answerable
from the audit trail.

`TPSL_PROTECT_ALL=false` narrows adoption to the configured universe, for
an operator who wants the bot to leave its own hand trades alone.

### Rest-of-hours trading

`EXTENDED_HOURS=false` by default. The pre-market (04:00–09:30 ET) and
after-hours (16:00–20:00 ET) sessions are a different market from the one
the strategy is calibrated in, and the broker lets much less of the bot's
toolkit into them. Every rule below is a **measured Alpaca refusal**
(paper API, 2026-10-25), not a house preference:

| Order shape in the extended session | Broker answer |
|---|---|
| `limit` + `day`/`gtc` + `extended_hours=true` | accepted, echoes `extended_hours: true` |
| `order_class=bracket` | **403** `bracket orders do not support extended hours trading` |
| `order_class=oco` | **403** `oco orders do not support extended hours trading` |
| `type=market` | **422** `extended hours order must be DAY or GTC limit orders` |
| `type=stop_limit` | **422** `stop_limit orders are not eligible for extended hours trading` |
| `time_in_force=ioc` | **422** `ioc orders are only accepted during market hours` |
| any option with `extended_hours` | refused; options have no extended session |

The bracket refusal is the one with consequences, so it shapes the whole
feature: **a position opened in the extended session has no broker-side
stop while it sits there.** Three things follow.

- **Entry is a marketable limit.** It crosses the ask by a 5bps buffer, is
  re-sized against the price actually paid so the approved dollar budget is
  the budget spent, and is refused if the live mark has moved more than 1%
  from the price the plan was sized at. No bracket envelope is sent, so the
  order is never a 403 waiting to happen.
- **Protection arrives with the next regular session.** The entry records
  the strategy's TP/SL in state, and the monitor's protect pass — which
  already adopts positions the bot did not open — rests the `oco` pair for
  the open. Measured: protective pairs placed while the market is closed sit
  in `accepted` and hold the position's shares (`insufficient qty available`
  on any competing exit), so the pair is in place before the bell.
- **Exits in that session cross the bid themselves.** A stop breach
  after-hours is a limit/`day` with `extended_hours=true`, because market
  and stop orders are refused. The shape is resolved *before* the resting
  protection is cancelled: an exit that cannot be priced leaves the position
  exactly as it was, floor intact, rather than stripping its protection to
  buy a close that never happens.

Between sessions (20:00–04:00, weekends, holidays) nothing acts. The bot
does not rest a market order for the next open, because the exit path
cancels protection first — a resting sell would face the opening print with
no floor. The breach re-triggers inside a session that can execute.

**Freshness is a trade condition, not a nicety.** A snapshot keeps serving
its last print after the session ends; measured on a Sunday, AAPL's quote
was Friday 20:00:03Z and 10% wide. So `EXTENDED_HOURS_MAX_QUOTE_AGE_SECONDS`
bounds the age of the print an entry or exit is priced from, a future-dated
stamp is refused (negative age would otherwise read as infinitely new), and
one-sided or crossed touches are refused. `EXTENDED_HOURS_WINDOWS` narrows
entries away from the first and last minutes of the session, and
`EXTENDED_HOURS_MAX_SPREAD_BPS` is the session's own liquidity bar —
config refuses to enable the session with no cap in either key.

**Sessions come from the broker's calendar, not the wall clock.** `/v2/calendar`
publishes `session_open`/`session_close` per trading day and omits holidays
entirely, so a market holiday at 05:00 Tuesday is *closed*, where the previous
weekday guess said *extended*. An unreadable calendar blocks the entry with
the reason instead of guessing a session, and the risk gate's clock adapter
now fails closed on an outage — the agent path used to assume "market open".

Crypto is untouched (24/7, no extended session) and the options overlay is
skipped in this session: an option order placed now can only fill by resting
until the bell, at a price and time the signal never described.

---

## 5. The agent-to-agent bridge

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
A2A_PEER_TOKENS=research:r-secret,analyst:a-secret  # optional per-agent credentials
```

### Posting a signal

```bash
curl -X POST http://localhost:8080/api/a2a/signals \
  -H "Authorization: Bearer $A2A_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"id":"sig-1","agent":"research","symbol":"AAPL","side":"buy",
       "notional":"3200","confidence":0.82,"reason":"momentum"}'
```

```json
{"accepted": true, "mode": "shadow", "reason": "accepted in shadow mode; no order will be placed"}
```

Send **notional**, not qty. The bridge has no market-data client (pricing
and sizing belong to the risk gate), so it cannot compute the exposure of a
qty-only signal — and a cap it cannot evaluate is not a cap. With
`A2A_MAX_NOTIONAL` set, a qty-only signal is refused rather than waved
through; the refusal names the reason.

The `id` is an idempotency key bound to the payload: a replayed signal is
dropped, and the same id carrying *different* fields is refused as a reuse
rather than silently swallowed. The queue is capped (`MaxPending`, 100), the
per-agent quota is a rolling hour (not a lifetime counter), and a signal is
consumed once — the bot acknowledges each one after evaluating it instead of
re-trading it every tick until it expires.

When an agent has its own credential (`A2A_PEER_TOKENS=agent:token,…`), only
that token may post as it: the shared group token cannot impersonate an
agent that has its own, including one ramped `on`.

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

## 6. Learning from its own trades

A factor score is a hypothesis. Until a trade closes, nobody knows
whether a composite of 0.62 meant anything — and the same 0.62 can be a
good entry in one asset class and a consistent loser in another. Only
realised outcomes can say which.

So the bot measures its own results and adjusts. `learning/` computes,
and `cmd/alpacaruns/learning.go` applies, four things:

| Knob | What it may do | What it may never do |
|---|---|---|
| Entry score floor | RAISE it above `FACTOR_MIN_SCORE` when low scores demonstrably lose | Lower the operator's bar |
| Position size | SHRINK a class's size toward `LEARN_MIN_SIZE_MULTIPLIER` | Grow any position |
| Stop distance | TIGHTEN a class's stop toward `LEARN_MAX_TIGHTEN_FACTOR` | Widen a stop |
| Symbol set | QUARANTINE a symbol whose win rate stays under the bar | Unblock a risk gate |

**The learner is a brake, not an accelerator.** Every knob it can touch
moves in the direction that reduces risk or increases selectivity. A
mis-calibrated estimate, a regime change, or a run of bad luck can make
the bot trade less; none of them can make it trade bigger, take looser
entries, or hold a wider stop than the operator authorised. That property
is enforced in three places, not just asserted: `Config.Validate()`
rejects a bound above 1.0 at startup (`learning may only shrink, never
grow`), `Adjustments.SizeMultiplierFor` and `StopMultiplierFor` re-clamp
on every read, and `Store.Load` re-clamps again after deserialising — so
a hand-edited `data/learning-state.json` cannot scale a position up.

### How it gets its evidence

Each fill is journalled with the composite the bot acted on
(`EntryPlan.Score` → `pnl.Record.Confidence`), so a closed round-trip
carries both the decision and its result. The stats ledger keeps a
bounded window of those trades (`data/strategy-stats.json`, 600 samples)
and the engine splits them into equal-width score buckets and per-class
groups.

Two honesty rules follow from that:

- **Unscored trades are excluded from calibration.** A fill the bot did
  not make — placed by hand, or backfilled by order id — has no score.
  Filing it at 0.00 would invent evidence that low scores lose, and the
  learner would raise its own floor to escape a number it fabricated.
- **A self-reported 1.00 confidence is not a score either.** Proposals
  that claim perfect confidence are recorded as unattributed.

### When it speaks

Not on a timer. Nothing changes until `LEARN_MIN_TRADES` round-trips
exist, and after that it recomputes every `LEARN_EVERY_TRADES` new closed
trades. A single recomputation may not raise the floor by more than 0.15
— silently stopping all trading is indistinguishable from a bug, so the
move is bounded and the note says why.

The engine runs whether or not `LEARN_ENABLED` is true: with it off, the
samples accumulate and the snapshot records what the learner *would*
conclude, so turning it on is a decision made against data.

### Where it is applied

Every entry path, and only in the risk-reducing direction: `handleBuy`
(swing), `preOrder` (off-hours queue), `intraEntry` (intraday book) and
`evaluateA2A` (peer-agent signals — an agent cannot ask for more than the
learner allows for that class). A shrink that would erase the position
refuses the trade instead of sending a zero-unit order to the broker.

### The per-symbol ledger's one-way bias

`strategy-stats.json` also computes a confidence bias per symbol. Only
the penalty side is applied: a chronic loser has its signal confidence
reduced, so the unchanged risk floor rejects it. A winner's bonus would
push a fresh signal over a bar it did not clear, so the positive branch is
computed for reporting and deliberately never applied.

### Configuration

| Key | Default | Meaning |
|---|---|---|
| `LEARN_ENABLED` | `false` | Apply the conclusions (evidence is recorded either way) |
| `LEARN_MIN_TRADES` | `12` | Closed round-trips before the first adjustment |
| `LEARN_EVERY_TRADES` | `5` | Recalibrate after N more closed trades |
| `LEARN_MIN_SIZE_MULTIPLIER` | `0.25` | Floor on how far sizing may shrink |
| `LEARN_MAX_TIGHTEN_FACTOR` | `0.75` | Floor on how far a stop may tighten |
| `LEARN_QUARANTINE_WIN_RATE` | `0.25` | Win rate at or below which a symbol is blocked |
| `LEARN_QUARANTINE_TRADES` | `6` | Trades a symbol needs before quarantine applies |
| `LEARN_MIN_SAMPLES_PER_CLASS` | `8` | Samples a class needs before size or stop moves |
| `LEARN_SCORE_BUCKETS` | `5` | Bucket count in the calibration table |

Read it at `GET /api/learning` or on the dashboard's **Learning** page,
which shows the buckets, the per-class multipliers, the quarantined
symbols and the note the learner wrote when it changed something.
Changes are journalled to `data/trades.jsonl` under source `learning`.

---

## 7. Self-heal

A long-running trading process fails in ways that are silent, and some of
those states are unsafe. `selfheal/` runs a pass that finds them and — by
default — repairs them, without an operator reading logs at 3am.

| Check | Severity | State it catches | Repair |
|---|---|---|---|
| `orphan-protective-order` | high | A resting sell order with **no position behind it** — that is a short-sale instruction, reachable by closing a position by hand and leaving the pair the bot placed | Cancel it |
| `duplicate-protection` | high | Two protective pairs on one position; both may fill and sell twice what is held | Keep the newest parent, cancel the rest |
| `stale-resting-entry` | medium | An unfilled entry limit from days ago that would fill on a spike nobody re-checked, at a price the factors would not accept | Cancel it past `SELFHEAL_ORPHAN_MAX_AGE_HOURS` |
| `state-file` | critical | `strategy-state.json` is not parseable, so intended TP/SL levels and the drawdown peak are unreadable | **Reported, not repaired** |
| `journal-tail` | low | A crash left a truncated final line in `trades.jsonl` | Reported |
| `halt-still-engaged` | medium | A drawdown halt still engaged after the breach healed | Re-base the peak, opt-in |

The state file is deliberately *not* auto-repaired: setting it aside
deletes the only record of the stop levels the bot intended to hold, so
the position-aware reconciliation pass re-derives what it can from the
broker and an operator decides about the file.

**The pass may only cancel.** It never places an order, never increases
size, never widens a stop, and never deletes a file. Cancelling an
orphaned protective order is risk-reducing in every case; inventing one
would be the bot trading, which is not this package's job. So the worst a
bug in here can do is leave the book exactly as exposed as it already was.

### A skipped check is not a clean pass

`Report.Clean()` is `len(Actions) == 0 && len(Errors) == 0`. If the
positions or open-orders read fails, every remaining check is
un-evaluable, and that is recorded as an error and journalled — not
reported as healthy. The difference between "examined and clean" and "not
examined" is the whole point of the pass.

Lifting a halt is opt-in (`SELFHEAL_LIFT_HALT`) because a halt is the
operator's brake, and the daily/weekly halts are computed from the
journal's realised fills — a bookkeeping re-base must not erase the
losses that earned the halt. Only the total-drawdown peak is re-based.

### Cadence, output and configuration

Runs on the first tick and then every `SELFHEAL_EVERY_TICKS`, because
these states do not appear between two polls and each pass reads the whole
order list. Every action is journalled under source `selfheal:<check>`
with risk `HEAL_REQUIRED` or `HEAL_NOTED`, and the last report is
persisted to `data/selfheal-report.json` for the separate API process.
The report also carries the **posture the pass ran with** (report-only,
halt-lift, orphan TTL), and the API prefers that over its own
environment — after a config change plus a bot restart the two disagree,
and the dashboard must describe what acted on the account, not what the
API container happens to think.

| Key | Default | Meaning |
|---|---|---|
| `SELFHEAL_ENABLED` | `true` | Run the pass at all |
| `SELFHEAL_EVERY_TICKS` | `6` | Tick cadence |
| `SELFHEAL_REPORT_ONLY` | `false` | Find and journal, change nothing |
| `SELFHEAL_ORPHAN_MAX_AGE_HOURS` | `24` | Age after which a resting entry is stale (`0` disables) |
| `SELFHEAL_LIFT_HALT` | `false` | Allow re-basing the drawdown peak |

Read it at `GET /api/selfheal` or on the dashboard's **Learning** page.

---

## 8. Safety rails

| Control | Mechanism | Where |
|---|---|---|
| Paper by default | `ALPACA_BASE_URL` points at the paper API; a non-paper URL is refused without `I_ALPACA_LIVE=YES` | `config/config.go` |
| Kill switch | SIGINT/SIGTERM halts new trades; drawdown halts engage the same switch | `risk/`, `strategy/monitor.go` |
| Position-aware TP/SL | Every broker position is adopted and anchored to its real `avg_entry_price`; equities get a resting `oco` pair, crypto and options are enforced locally | `strategy/positions.go` |
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

## 9. Configuration

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
| `EXTENDED_HOURS` | `false` | Trade the pre-market and after-hours sessions (see [Rest-of-hours trading](#rest-of-hours-trading)) |
| `EXTENDED_HOURS_WINDOWS` | `04:30-09:25,16:05-19:55` | When entries may open in that session (US/Eastern) |
| `EXTENDED_HOURS_MAX_SPREAD_BPS` | `0` (uses `MAX_SPREAD_BPS`) | Liquidity bar for extended-session entries |
| `EXTENDED_HOURS_MAX_QUOTE_AGE_SECONDS` | `300` | Older print than this is not a market; entries and exits refuse it |

### Agent bridge

See [the bridge section](#5-the-agent-to-agent-bridge).

### LLM providers

Priority order: `LLM_BASE_URL` (local llama.cpp — use `--jinja` so
Qwen3's own tool-call template is used) → `LLM_PROVIDER=oxlo` with
`OXLO_API_KEY` → `GEMINI_API_KEY`.

---

## 10. CLI

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

# research: replay the rule over history, per walk-forward fold. Never trades.
go run ./cmd/alpacaruns research --symbols SPY,QQQ --from 2024-01-01 --to 2025-01-01     --folds 4 --spread-bps 2 --slippage-bps 3

# mirror: build/verify the SQLite view of the journal + state. Exit 1 if they differ.
go run ./cmd/alpacaruns mirror

# sessions: what the broker's calendar says the market is doing, and what each
# symbol's entry route decides. Read-only; the extended-session quote age and
# spread are printed so a refusal is explainable before the session, not after.
go run ./cmd/alpacaruns sessions --symbols AAPL,BTC/USD
```

`auto` is the recommended entry point. `monitor` (the LLM-gated path),
`cycle` (one LLM cycle) and `query` (ad-hoc market questions) are also
available.

---

## 11. HTTP API

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
| GET | `/api/learning` | The adjustments the learner is applying, its bounds and the calibration table |
| GET | `/api/selfheal` | The last self-heal pass: actions taken, findings, skipped checks |
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

## 12. Dashboard

The Next.js app is statically exported and embedded into the binary at
`api/ui/`. Seven pages:

| Page | What it shows |
|---|---|
| Welcome | Health badge, splash, CTA |
| Live | Equity, day/total P&L, open positions, equity curve, kill-switch badges |
| Trades | Cursor-paginated trade log with path filters |
| Brain | Open positions, decision feed, factor weights |
| **Picker** | The classified universe, the ranked signals, and the agent bridge with its ramp controls |
| **Learning** | The learner's floor uplift, per-class size/stop multipliers, the score→outcome calibration table, quarantined symbols, and the last self-heal report |
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

## 13. Data flow and state files

| File | Written by | Read by |
|---|---|---|
| `data/trades.jsonl` | Every path (bot, agent, dashboard, bridge) | P/L, the dashboard, the stats ledger |
| `data/strategy-state.json` | The bot (heartbeat, TP/SL levels, peak equity) | The dashboard, the bot |
| `data/a2a-state.json` | The bridge (ramp positions, counters, pending signals) | The bot, the dashboard |
| `data/strategy-stats.json` | The stats ledger (per-symbol win/loss + the bounded trade window) | The bot (confidence bias), the learner |
| `data/learning-state.json` | The learner (the adjustments it computed) | The bot (entry gate), the dashboard |
| `data/selfheal-report.json` | The self-heal pass (last report) | The dashboard |
| `data/paused` | The dashboard's pause control | The bot, the API |

The bot and the dashboard are **two separate processes** sharing these
files. That is why the ramp position, the pause flag and the TP/SL
levels all live on disk rather than in memory.

---

## 14. Deployment

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

## 15. Tests and gates

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
- The learner's tests prove it cannot grow anything: a multiplier above
  1.0 is rejected at startup, re-clamped on read, and re-clamped again
  after a hand-edited state file is loaded — and NaN is refused, because
  a `<= 0 || > 1` rejection test lets NaN through.
- The sample-window tests prove unscored trades never reach the
  calibration table, and that rebuilding the ledger from the journal is
  idempotent (the old code added each refresh on top of the last,
  permanently inflating every win rate).
- The self-heal tests prove a pass that could not read the account is
  reported as skipped rather than clean, that report-only mode cancels
  nothing, that the pass runs on its tick cadence rather than every tick,
  and that every action reaches the journal.

---

## 16. What was fixed in this pass

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
| TP/SL scope | Protection existed only for positions this process opened, and levels were anchored to the plan-time price. Every broker position is now adopted and re-anchored to its real average entry, shorts are inverted, options are enforced locally, and equities get a resting `oco` pair. |
| Pre-orders | `preOrder` was never wired into the tick; now a closed market queues a resting bracket instead of dropping the signal. |
| Dashboard auth | The dashboard attached the bearer token only to `/api/control/*`, so every config save and manual order 401'd on a token-protected deployment. |
| Config race | `POST /api/config` mutated the live config while other handlers read it. |
| Market clock | The dashboard's manual-order path had no market-hours check at all. |
| `X-Forwarded-Host` | The docstring promised it, the code never read it: behind a proxy every control POST was 403. |
| `path=auto` | The dashboard's "auto" tab matched nothing and was permanently empty. |
| Asset catalogue | `/v2/assets` ignores `limit`/`offset`; the paging loop re-fetched the full 14k-asset catalogue 50 times per call. |
| Entry scoring | Every swing fill was journalled with the **gate confidence**, which the deterministic loop pins at 1.0 — so no score→outcome pair ever existed and a learner reading that journal would have "calibrated" on a constant. The plan now carries the decision composite (`EntryPlan.Score`). |
| Option fills | The options overlay journalled the decision but not the fill, so option trades were invisible to realised P/L, the stats ledger and the learner. |
| Confidence bias | The ledger's winner *discount* was added to the signal's own confidence, which lets a past win push a fresh signal over the risk floor it did not clear. Only the penalty side is applied now. |
| Multiplier clamp | Rejection-style guards (`x <= 0 || x > 1`) let NaN through as a valid multiplier, and a NaN size factor silently zeroes a position. Every clamp is now an affirmative test, and a corrupt value means "no opinion" (1.0). |
| Dead threshold | `statsLedger.confidenceRequired` computed a *lowered* threshold for losing symbols under a docstring claiming the opposite, and had no callers. Removed rather than fixed. |

### Second pass — verified against the source, then fixed

An external architecture review of this project was written from the README
alone. Every claim below was re-checked against the code first; the ones
that survived are listed with what was actually wrong. Claims that did not
survive (for example "the risk gate is not shared", "MCP tools can place
orders ungated") are not listed, because they were already true.

| Finding | Verified defect | Fix |
|---|---|---|
| Fills can be lost | A fill was journalled only if it was present in the POST response; `auto` runs no stream and never backfills. **Measured on the live paper account: two filled entry orders, zero fill rows in the journal**, so drawdown halts, the ledger and the learner all computed on an empty book | Durable intent before every submission, and a per-tick reconciliation that journals the delta against cumulative broker fill quantity (partial-fill aware, idempotent) |
| Exits gated by entry policy | `handleSell` refused to exit outside the **entry** trading window, and skipped any position whose qty was not positive — so a short could never be closed | Exit authority (`ExecuteReduce`) with a `Reducing` proposal the gate *proves* against live holdings, exempt from the kill switch, confidence floor and caps |
| Ungated mutation paths | `handleSell`, `closePosition`, `intraExit` called the broker directly; `closePosition` read the tick-frozen position cache | All three now go through the exit authority: fresh position read, protection cancelled first, one gate, deterministic id (the old id embedded `UnixNano`, so a retry was *not* deduped — measured: a reused id returns HTTP 422) |
| Halt stopped protection | The loop exited when the kill switch engaged; with `Restart=always` the unit then cycled every 10s, and nothing enforced TP/SL between exits | A halt suppresses new entries only; monitoring, exits and reconciliation continue |
| Additive cap was dead | `NewValidatorWithScorer` never set `Positions`, so the combined-exposure check was skipped on exactly the unattended path | Wired to a short-TTL live book, invalidated on every mutation; an unreadable book now refuses instead of reading zero |
| Two buys, one tick | Per-symbol cap read a frozen tick-start snapshot | Live book invalidated after each submission |
| Manual order fail-open | `validateProposal` returned `Approved: true` when no config was loaded | Refuses, and names why |
| Same-origin could be spoofed | Under `TRUSTED_PROXY`, the CSRF host check preferred client-supplied `X-Forwarded-Host` | Compares against `r.Host`, or the configured CORS origin behind a trusted proxy — never a caller header |
| Paper/live guard was a string | Only the URL was checked: **paper URL + live keys passed every check** | `VerifyVenue` reads the account the credentials resolve to, requires a paper account id, and halts on mismatch (bot) / 503s (dashboard) |
| A2A identity, quota, queue | Any token holder could post as any agent id; the "hourly" quota was a lifetime counter; the pending queue was unbounded; qty-only signals bypassed the notional cap; `Drain` re-offered every signal so an ON agent's signal executed on every tick until expiry | Per-peer tokens (`A2A_PEER_TOKENS`), rolling-hour window, `MaxPending`, refusal of unmeasurable exposure, payload-fingerprint dedupe, and an ack so each signal is consumed once (the broker intent is keyed to the signal id, so a redelivered retry dedupes) |
| Three writers on the state file | The API's config save rewrote the bot's document through the **same** temp path, so a dashboard save could revert a TP/SL level or the peak; `starting_equity` was dropped by every typed save, leaving adaptive sizing permanently inert | One writer per file: the API writes `config-overrides.json`, the bot owns `strategy-state.json` (pid-scoped scratch); `StartingEquity` is a typed field and the bot reads the overrides |
| Stale bars | No freshness check: a halted symbol's three-week-old close still scored and could be bought | Per-timeframe freshness gate, fail-closed |
| Unadjusted splits | Bars were fetched `raw`, so a split read as a 90% collapse in trend/momentum and inflated ATR stops | `adjustment=split` (verified accepted by the API; `all` also rewrites history for dividends) |
| Silent factor | The loop passes `news=nil`, so sentiment contributed its full weight at a neutral 0.5 and diluted every composite | A factor with no source is excluded from the weighted mean instead of voting |
| Cancels assumed | A 2xx from DELETE was treated as cancelled — **measured: 204 returned while the order still listed as accepted** | Cancellation is confirmed by re-reading the order; unconfirmed is an error, and self-heal no longer reports it "repaired" |
| Sizing truncation | `int(budget/price)` dropped a unit whenever the division landed a hair under an integer | Epsilon-aware floor |

| Drawdown used cash flow | Daily/weekly halts summed fill cash flow, so a BUY read as a same-day loss and a busy entry day could halt the bot without one losing trade | Realised P/L per window from closed round-trips, sharing the FIFO with `pnl.Compute`; unrealised risk stays in the marked-equity total halt |
| Unknown submission outcome | A timeout after the broker accepted an order was treated as failure, inviting a retry that opens a second position | Every submission is resolved by its journalled `client_order_id` before it can be retried; not found means a real error |
| Non-tradable symbols | The picker recorded `tradable` but the trading universe ignored it, so delisted/halted/renamed tickers were scored and rejected once per poll | The loop scores `TradableSymbols()` and logs what it excluded |
| Unpinned MCP server | `uvx alpaca-mcp-server` floated to whatever build PyPI served; the guard classifies tool names, so an unannounced upgrade changes the surface it was written against | Pinned to `alpaca-mcp-server==2.4.0` (override with `ALPACA_MCP_SPEC`), and the spec is logged at launch |

### Screening, costs and per-class gates

**Market cap is screened with real numbers, not estimates.** `alpacaruns caps
--refresh` builds `data/market-caps.json` from SEC EDGAR's reported shares
outstanding times the broker's latest close, stamped with its observation time
and source. With `MIN_MARKET_CAP_USD` set, a single-name equity whose cap is
unknown, older than `MARKET_CAP_MAX_AGE_DAYS`, or below the floor is **not
scored** — fail closed — and the log names every symbol it dropped and why.
ETFs, index trackers and crypto are exempt by class: a fund has AUM rather than
a market cap, and a pair has no share count. Live table at the time of
writing: AAPL $5.07T, MSFT $3.89T, NVDA $5.08T (EDGAR, 2026-10-25).

**Costs and margin are live, not just modelled in the replay.**
`MAX_SPREAD_BPS` refuses an entry whose quoted spread is wider than the limit
(fails closed when the quote is unreadable), and `MAX_GROSS_EXPOSURE_PCT` caps
the book's total market value against equity — per-name caps alone let N names
each under the cap add up to a leveraged book. 1.0 means no leverage.

**Class floors are separate because the score distributions are.**
`FACTOR_MIN_SCORE_EQUITY`, `_ETF`, `_INDEX`, `_CRYPTO`, `_OPTION` override the
global floor per class. One floor calibrated on equities is not a neutral
default for crypto; applied unchanged it silently means "never trade that
class". Classes without an override fall through to the global floor.

Measured per class (2024-01 → 2025-10, 4 walk-forward folds, 2bps spread +
3bps slippage per side, next-open execution, stop wins ambiguous bars):

| Class | Trades | Win rate | Expectancy/trade | Net (on $10k stakes) | Max DD |
|---|---|---|---|---|---|
| Equities (AAPL, MSFT, NVDA) | 72 | 41.7% | +1.03% | +$7,407 | 57.8% |
| ETFs (SPY, QQQ, IWM) | 34 | 44.1% | +1.21% | +$4,124 | 20.9% |
| Crypto (BTC/USD, ETH/USD) | 117 | 40.2% | +0.78% | +$9,143 | 45.5% |

All three are positive in this window and none of them is a promise: the drawdowns
are large, the window is one regime, and the same rule has now been evaluated
repeatedly on the same data — which is what the registry's repetition flag
exists to say out loud.

### The research harness, and what it is for

`research/` replays the **same** scoring code the bot runs
(`factors.ScoreBars`) over historical bars and reports what the rule would
have produced, per chronological walk-forward fold. Three properties are
enforced in code and pinned by tests, because each is how a backtest lies:

- a signal computed from bar N executes at bar **N+1's open** (a test buys
  through a 20% opening gap and asserts the fill is the gap, not the signal
  bar's close);
- when a bar's range covers both the stop and the target, the **stop** is
  booked (OHLC does not say which came first);
- every trade pays the configured spread, slippage and commission.

Every run is appended to `data/research-runs.jsonl` with its parameter hash,
and a repeated parameter set is flagged — the tenth tweak of one parameter
set is not ten independent experiments. The report says what it does not
cover: no intraday path, no borrow, no partial fills, no market impact. It is
a screen, not a validation.

### The SQLite stage

`store/` mirrors fills, intents, decisions, levels and the state scalars into
`data/alpacaruns.db`. The JSON/JSONL files remain the **write** path — one
writer each, atomic rename — and `Verify()` replays both sides and reports
every disagreement before anything reads from the mirror. `alpacaruns mirror`
runs it once (exit 1 on divergence); the bot ingests per tick and verifies
every tenth tick, journaling a `mirror:verify` finding if the two ever differ.
A cut-over that moved the live monitor onto a schema change in one commit
would have put position protection on the critical path of a migration; this
does not.

### Identity, renames, and what a model is allowed to do

**Operators can authenticate with an identity provider.** With
`OIDC_ISSUER`, `OIDC_AUDIENCE` and `OIDC_JWKS_URL` set, the control endpoints
accept an RS256 JWT and record its `email` as the actor. The verifier allows
exactly one algorithm ("none" and HMAC are refused by name — the alg-confusion
attack in one check), verifies the signature against the provider's published
keys (cached, refreshed once on an unknown `kid`), and enforces issuer,
audience and expiry. `DASHBOARD_TOKENS` remains for bootstrap; a token that
looks like a JWT never falls through to a static comparison. What this does
NOT do is authorisation: holding a valid token is the whole check, and mapping
provider groups to roles is an operator decision.

**Renames are broker fact, not a guess.** The asset catalogue carries a stable
`asset_id` that survives a ticker change, so the reference-data snapshot can
say "asset X was `FB` and is now `META`" — and the bot moves the stored TP/SL
levels to the new ticker before anything else runs, instead of silently
leaving the stop attached to a symbol the broker no longer knows. A true
disappearance (delisting, merger out) is still only reported: mapping it to a
successor needs an external source, and inventing the link would move
protection to the wrong instrument.

**A model cannot self-certify a trade.** The MCP guard caps a proposal's
claimed confidence at the deterministic factor composite — the model may be
more cautious than the evidence, never more confident — and refuses an order
outright when no deterministic score is available to corroborate it. Prompt
injection is still possible; a persuaded model can pick the wrong instrument
among eligible ones. It cannot manufacture eligibility, exceed the caps, or
place anything the gate did not approve.

### Rest-of-hours, measured rather than assumed

Extended-session trading existed as a config key and a client-side check, but
the shipped path could not reach it: `handleBuy` asked one boolean — open or
closed — and everything not-open went to a skip or a pre-order. It is now a
route with four answers (live / extended / pre-order / blocked-with-a-reason),
resolved from `/v2/calendar` so holidays and half days are what the exchange
says they are.

Three defects surfaced while building it, each now pinned by a test:

- **The gate never saw the session it was approving.** The dashboard and CLI
  paths passed `extended_hours` to the broker but not to the proposal they
  validated, so the approval described a different order than the one sent.
  The agent path went further and *stamped* `extended_hours=true` onto any
  limit/day order — including one carrying a bracket, which the broker refuses
  with a 403 on every attempt.
- **A future-dated print was read as maximally fresh.** Age was
  `time.Since(stamp)` compared to a limit, so a stamp ahead of the host clock
  went negative and passed. Now refused past 15s of skew.
- **The exit path cancelled protection before it knew it could exit.** An
  extended exit needs a live quote to price itself, and the old order of
  operations would clear the position's floor and then fail to place anything.
  The shape is resolved first now; an unpriceable exit leaves the pair intact.

### What is still open

- **The gap between an extended fill and its protective pair.** The pair is
  placed by the next monitor pass and only guards the regular session; a
  breach outside a session cannot be executed by anyone, so an after-hours
  position that gaps overnight is met at the open by the stop, at whatever
  price the open prints.
- **Mergers and delistings by succession.** A merge that retires one asset id
  in favour of another cannot be derived from the catalogue; it is reported
  and left to an operator.
- **Authorisation, not just authentication.** Roles and per-action scopes are
  not modelled; a valid operator token can use every control endpoint.
- **Intraday evidence.** The harness runs on daily bars. Everything it cannot
  see — intraday path, borrow, partial fills, market impact — is stated in the
  report rather than modelled.
- **Parameter search.** The registry flags repeated parameter sets but nothing
  stops an operator from searching; a deflated-Sharpe style correction is not
  implemented.

---

## 17. Layout

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
learning/           bounded self-calibration from closed trades (engine + store)
model/llamacpp/     llama.cpp / OpenAI-compatible model adapter
options/            options chain, greeks, snapshots
picker/             universal signal picker (registry, ingestion, ranking)
pnl/                JSONL journal + FIFO P/L
risk/               deterministic pre-trade validator
selfheal/           degraded-state detection and repair (+ report store)
stream/             market-data + trade-update streams
strategy/           deterministic engine (decide, execute, monitor, state, windows)
strategy/ensemble/  optional multi-expert layer
tools/              Alpaca REST client
```

---

## License

MIT — see [`LICENSE`](LICENSE).
