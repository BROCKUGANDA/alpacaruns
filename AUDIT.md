# Alpacaruns Production-Readiness Audit

Date: 2026-08-24 · Scope: full repo (`agents/`, `config/`, `tools/`, `cmd/alpacaruns/`, `model/llamacpp/`) · Audit only — no source files modified.

---

## 1. Index / Blast-radius findings (GitNexus)

Commands actually run (real output summarized below):

```
gitnexus analyze .            -> FAILED first: "Not a git repository"
gitnexus analyze . --skip-git -> OK: Repository indexed successfully (9.5s)
                                 288 nodes | 587 edges | 8 clusters | 25 flows
gitnexus list                 -> Alpacaruns registered among 11 indexed repos
gitnexus impact <symbol> -r Alpacaruns   (Build, do, placeOrder, Load, alpacaToolset, runOnce)
gitnexus query "trading cycle execution risk" -r Alpacaruns
gitnexus check --cycles -r Alpacaruns    -> "No circular imports found."
gitnexus detect-changes -r Alpacaruns    -> "No changes detected."
```

Caveat stated up front: **this directory has no `.git` folder**, so GitNexus ran with `--skip-git`. Commit tracking and incremental updates are disabled and `detect-changes` output ("No changes detected") is meaningless here — there is no diff baseline. Initialize git to make change-mapping useful.

### Blast-radius table (all `epistemic: exact`, upstream direction)

| Symbol | Location | Impacted | GitNexus risk | Notes |
|---|---|---|---|---|
| `Client.do` | `tools/alpaca.go:38` | 15 symbols, 9 direct, 5 processes | **CRITICAL** | Single HTTP chokepoint for every Alpaca call |
| `Build` | `agents/agents.go:58` | 1 (main) | LOW | Whole graph hangs off one builder |
| `Load` | `config/config.go:40` | 1 (main) | LOW | |
| `alpacaToolset` | `agents/mcp.go:17` | 2 | LOW | Both MCP connections route through it |
| `runOnce` | `cmd/alpacaruns/main.go:80` | 1 (main) | LOW | |
| `Set.placeOrder` | `tools/handlers.go:55` | 0 | LOW | **Dead code** — see below |

### Coupling conclusions

1. **`Client.do` (`tools/alpaca.go:38`) is the highest-risk, most-coupled symbol** — rated CRITICAL by GitNexus. Every account, position, bars, quotes, snapshot, news, order-place/status/cancel call funnels through it. Any change to its error semantics propagates to all 9 wrappers and 5 execution processes. It currently has no retry/backoff/rate-limit handling (see §2.1), which makes this chokepoint also the system's most fragile point.
2. **The graph is a shallow fan**: `main → Load → Build → {model, 2× MCP toolsets, agents}`. Low depth means low structural complexity, but it also means there are **zero redundant safety layers between the LLM and order placement** (see §2.2).
3. **`tools/` package (adapters/handlers/client) is dead production code.** `grep -rn "alpacaruns/tools"` finds **no imports outside `tools/` itself**. The README calls it "legacy … kept for reference/tests", and indeed `Set.placeOrder` shows impactedCount 0. All live trading traffic goes through the MCP bridge in `agents/mcp.go`. Consequence: the well-tested typed REST client does not protect production; production correctness depends entirely on Alpaca's MCP server subprocess.

---

## 2. Blockers (must-fix before autonomous operation)

### B1. Risk gates are prompt text, not code — no programmatic enforcement
- `RiskManagementExpert` (`agents/agents.go:152–159`): `MAX_POSITION_USD`, `MAX_PORTFOLIO_PCT`, `MIN_CONFIDENCE` exist **only inside an LLM instruction string** (`fmt.Sprintf` at `agents/agents.go:156`). Nothing parses `risk_assessment`; nothing recomputes notional vs equity.
- `ExecutionExpert` (`agents/agents.go:160–167`) receives the raw exec MCP toolset and places whatever its model decides. There is **no Go code between "LLM says buy" and an HTTP/MCP order**.
- A hallucinated number, a malformed JSON idea, or a prompt-injected ticker bypasses every cap. For an autonomous 24/7 agent this is disqualifying. Fix: deterministic pre-trade validator function wrapping every order call (check notional ≤ cap, pct of portfolio, confidence ≥ threshold, kill-switch state) that rejects before the MCP tool fires.

### B2. `KillSwitch.Halted()` is never consulted
- `KillSwitch` (`agents/agents.go:44–56`) is set by the `kill_switch` tool (`agents/agents.go:100–107`) and by Ctrl+C (`cmd/alpacaruns/main.go:60–65`), but **`Halted()` has zero callers** (verified by grep). The comment "every gate refuses to proceed" describes gates that don't exist. The kill switch halts nothing except future LLM discretion; an in-flight TradingCycle keeps running to completion after Halt.

### B3. No state persistence — crash amnesia
- `runOnce` creates `session.InMemoryService()` fresh on every invocation (`cmd/alpacaruns/main.go:81`). Sessions, `market_data`, `trade_ideas`, `risk_assessment`, `executions` output-keys all evaporate on exit.
- No database, file, or journal anywhere (`os.WriteFile` appears only in tests). Open positions/orders are *re-discoverable* from Alpaca's server via `get_positions`/`get_order_status`, but there is **no startup reconciliation step**: nothing compares broker state against what the agent believes it did, nothing resumes half-filled GTC orders placed by a previous run (`OrderRequest.ClientOrderID`, `tools/alpaca.go:223`, is defined but never populated — restarts can duplicate orders with no idempotency key).

### B4. Index-out-of-range panic in the local-LLM path
- `Model.GenerateContent` (`model/llamacpp/llamacpp.go:262`) evaluates `or.Choices[0].FinishReason` in the log call **before** the `len(or.Choices) == 0` guard at line 267. An empty-choices response from llama-server panics the goroutine. Combined with B6 (no panic recovery) this kills the whole process mid-cycle.

### B5. No market-hours / clock handling
- `MonitorLoop` description says "during market hours" (`agents/agents.go:208`) but nothing checks Alpaca's `/v2/clock` (the typed client never calls it either). The loop runs 24/7 against closed markets, burning LLM tokens and acting on stale data.
- `PollSeconds` (`config/config.go:27,53`) is loaded and then **never used by any Go code** — tick pacing is implicitly delegated to the LoopAgent/LLM, i.e., unbounded and unmeasured.

### B6. No panic recovery, fragile long-running loop
- `buildErrPanic` deliberately panics (`agents/agents.go:233–236`); acceptable at build time, but there is no `recover()` anywhere in the runner event loop (`cmd/alpacaruns/main.go:95–109`) or monitor path. One panicking tool callback or LLM adapter ends the "24/7" process.
- Event errors are logged and `continue`d (`main.go:98–100`) — good for resilience, but a persistently failing LLM means an infinite silent error loop with no circuit breaker.

### B7. Config can be silently wrong; no live-mode guard
- `getInt`/`getFloat` (`config/config.go:99–113`) swallow parse errors via `Sscanf` and fall back to defaults: `MAX_POSITION_USD=10O00` (letter O) silently becomes 10000; `MIN_CONFIDENCE=0,7` becomes 0.7. No range validation either — `MAX_POSITION_USD=-5`, `MAX_PORTFOLIO_PCT=5` (500 %!), or `MIN_CONFIDENCE=0` all load without complaint.
- `.env` never overrides pre-existing environment variables (`config/config.go:85`: `if _, exists := os.LookupEnv(k); !exists`). A stale exported `ALPACA_BASE_URL` pointing at the live API silently beats the `.env` file's paper URL.
- Paper-by-default itself holds (default at `config/config.go:49`; `MODE` defaults to supervised at line 52; invalid modes rejected at 63–65; test `config_test.go:45–48` covers bad mode). But **nothing refuses `MODE=autonomous` when `ALPACA_BASE_URL` is the live endpoint** — the two-flag mistake the README warns about ("change must be deliberate in two places") is enforced by convention only.
- Keys are injected into the `uvx` child process environment (`agents/mcp.go:19–23`) — visible to any process/user on the host that can inspect child environments.

---

## 3. Warnings

### W1. No retry/backoff/rate-limit handling on Alpaca HTTP calls
- `Client.do` (`tools/alpaca.go:38–78`): single attempt, fixed 15 s timeout (`NewClient`, line 33), no retry on 429/5xx/network errors, no `Retry-After` handling. Error bodies are surfaced as opaque strings (lines 69–73) — adequate for logging, useless for classification.
- Mitigating: this client is dead production code (see §1.3). The **live path is the MCP stdio bridge** (`agents/mcp.go`), whose retry behavior is whatever `uvx alpaca-mcp-server` does internally — unaudited from this side.

### W2. MCP subprocess lifecycle is fragile for 24/7
- `alpacaToolset` (`agents/mcp.go:17–40`) spawns `uvx alpaca-mcp-server` twice (data + exec). On failure it `Process.Kill()`s without `Wait()` (leaves a zombie until GC reaps). There is **no health monitoring and no respawn**: if `uvx` dies mid-`monitor`, every subsequent tool call fails until a human restarts the process. `uvx` also cold-starts slowly and needs Python/uv on PATH — a deployment dependency with no startup preflight beyond the spawn itself.

### W3. Ctrl+C handler leaks cleanup and exits unconditionally
- The monitor signal goroutine calls `os.Exit(0)` (`cmd/alpacaruns/main.go:64`), which skips `defer g.Close()` (line 42) and `defer stop()` (line 36) — MCP child processes are killed by `Close()`'s closers, so they may survive the parent as orphans. Exit status 0 on kill-switch also misleads supervisors into thinking a clean cycle finished.

### W4. Legacy tool handlers ignore caller cancellation
- `tools/adapters.go` / `tools/handlers.go` pass `context.Background()` everywhere (e.g. `handlers.go:22, 29, 36, 40, 76`), discarding `agent.ToolContext`. Dead code today, but a landmine if `tools.Set` is ever promoted back into the live path.

### W5. Tool-call argument decoding errors swallowed
- `llamacpp.go:278`: `_ = json.Unmarshal(...)` — malformed tool-call JSON from Qwen3 becomes a `FunctionCall` with nil args, which downstream ADK will reject confusingly far from the root cause. Given the README itself flags malformed tool calls as the top local-model failure mode, this should be surfaced loudly.

### W6. Test coverage does not touch the risk path
- Tests exist only for `config` (`config_test.go`: paper-defaults, bad mode) and the legacy REST client (`tools/alpaca_test.go`: order place/cancel, bars). **Zero tests** for `agents.Build` wiring, kill-switch behavior, or any risk logic — because there is no risk logic in code to test (B1).

### W7. Secrets on disk in plaintext
- `.env` and `.env.local` hold real paper API keys and a Gemini key in the working tree; directory is not a git repo so no history leak, but any backup/sync of `Desktop\Alpacaruns` exfiltrates them.

---

## 4. Missing features (needed for the P/L-over-time mandate and 24/7 autonomy)

1. **P/L tracking — entirely absent.** Verified by search across the repo: no code computes realized or unrealized P&L. The closest raw materials exist — `Position.AvgEntryPrice/CurrentPrice/MarketValue` (`tools/alpaca.go:92–99`) and `Account.Equity/PortfolioValue` (line 83–87) — but nothing consumes them arithmetically, and the MCP-tool path never even parses positions into structs. There is no trade-history store, no daily/total return series, no benchmark comparison. **The user will be judged on P/L over time; today the system cannot even answer "how much have I made?"**
2. **Trade journal / audit log.** Orders, fills, rejections, risk decisions are streamed to stdout and lost. Need append-only persisted records keyed by `client_order_id`.
3. **Startup reconciliation.** On boot: fetch open orders + positions from Alpaca, compare to journal, alert on drift, adopt-or-cancel orphaned GTC orders.
4. **Idempotent order placement.** Populate `ClientOrderID` deterministically per (cycle, idea) so crashes can't double-order.
5. **Market calendar / clock gating.** Check `is_open` before cycles; schedule around opens/closes/half-days.
6. **Deterministic risk engine** (see B1) plus portfolio-level drawdown halt persisted across restarts (a restart currently resets all memory of a bad day).
7. **MCP supervisor.** Watchdog that respawns `uvx alpaca-mcp-server` with exponential backoff and fails safe (block order tools, keep data tools) while down.
8. **Metrics & alerting.** At minimum: cycle latency, tool error rates, LLM refusal/malformed-call counts, push notification on kill-switch/HALT_TRADING.
9. **Graceful degradation for the LLM.** Retry-with-backoff around `GenerateContent`; quarantine malformed outputs; cap consecutive failures before auto kill-switch.

---

## Verdict

Structurally clean (no import cycles, shallow coupling, one CRITICAL chokepoint in `Client.do`), honestly documented, and genuinely paper-safe by default. But it is a **scaffold, not an autonomous trader**: every safety property advertised in the README (position caps, confidence gate, kill switch) is enforced by LLM prompt compliance rather than code (B1/B2), state dies with the process (B3), the primary local-LLM path can panic on a malformed response (B4), and P/L — the metric of record — is not computed anywhere (§4.1). Do not enable `MODE=autonomous` until B1–B5 are fixed; supervised mode with a human reading the event stream is the current realistic operating envelope.

*Every claim above cites symbols and lines read directly from the working tree on 2026-08-24; GitNexus figures come from the actual commands listed in §1 (index: 288 nodes / 587 edges / 8 clusters / 25 flows).*
