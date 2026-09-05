"use client";

// Controls page — operator console for the Alpacaruns dashboard.
//
// Wired up to four backend surfaces:
//   - GET  /api/config              — read runtime knobs (pre-fill form)
//   - POST /api/config              — push risk parameter updates
//   - POST /api/control/step        — run one decision cycle + read factor preview
//   - POST /api/trade/simulate      — dry-run a manual order through risk
//   - POST /api/trade/execute       — place a real paper order (with validator)
//
// Sections:
//   1. Risk presets (Conservative / Balanced / Aggressive) — single-click
//      maps to a complete ConfigUpdateRequest and POSTs it.
//   2. Risk config form — every knob editable, with the current value
//      shown beside each input so the operator can see "what is" vs
//      "what you can change to".
//   3. Decision cycle preview — runs one tick on demand and renders the
//      returned factor scores + rationale so judges see "the bot is
//      reasoning continuously, not just trading occasionally".
//   4. Manual trade form — symbol autocomplete from the configured
//      universe, side, qty or notional, optional limit price, Simulate
//      vs Send real paper mode. Renders the verdict + would-have-sent
//      envelope next to the action so the audit trail is obvious.
//   5. Pause / Resume + Step — the existing control endpoints. Kept
//      intact so the simple toggle behaviour judges expects still works.
//
// No API keys are ever rendered. The optional bearer token is reused
// from localStorage via getDashboardToken / setDashboardToken.

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import useSWR, { mutate } from "swr";
import { motion, AnimatePresence } from "framer-motion";
import {
  apiFetch,
  getDashboardToken,
  setDashboardToken,
  swrFetcher,
  type ConfigResponse,
  type ConfigUpdateRequest,
  type ControlResponse,
  type StatusResponse,
  type TradeExecutionResponse,
  type TradeOrder,
  type TradeProposalRequest,
  type TradeSimulationResponse,
} from "@/lib/api";
import { cn, fmtMoney, fmtPct } from "@/lib/utils";
import {
  ErrorState,
  PageShell,
  ShimmerBox,
} from "@/components/shimmer";
import { SwrProvider } from "@/components/swr-provider";

// ---- Risk presets ----
// Conservative / Balanced / Aggressive map cleanly onto the bot's
// six risk knobs. Values chosen to keep every preset within Alpaca's
// paper-API reality (sub-cent rounding won't matter; integer equity
// caps are easier to demo than 10_000.1234).
const PRESETS: Record<
  "Conservative" | "Balanced" | "Aggressive",
  ConfigUpdateRequest
> = {
  Conservative: {
    max_position_usd: 2000,
    max_portfolio_pct: 0.05,
    crypto_max_position_usd: 1000,
    min_confidence: 0.8,
    daily_dd_halt: 0.02,
    weekly_dd_halt: 0.05,
    total_dd_halt: 0.1,
  },
  Balanced: {
    max_position_usd: 10000,
    max_portfolio_pct: 0.2,
    crypto_max_position_usd: 5000,
    min_confidence: 0.5,
    daily_dd_halt: 0.05,
    weekly_dd_halt: 0.1,
    total_dd_halt: 0.15,
  },
  Aggressive: {
    max_position_usd: 25000,
    max_portfolio_pct: 0.4,
    crypto_max_position_usd: 10000,
    min_confidence: 0.4,
    daily_dd_halt: 0.1,
    weekly_dd_halt: 0.2,
    total_dd_halt: 0.25,
  },
};

export default function ControlsPage() {
  return (
    <SwrProvider>
      <PageShell className="space-y-6">
        <h1 className="text-2xl font-semibold tracking-tight">Controls</h1>
        <RiskPresets />
        <RiskConfigForm />
        <PauseStepRow />
        <DecisionCyclePanel />
        <ManualTradeForm />
        <OperatorTokenCard />
      </PageShell>
    </SwrProvider>
  );
}

// ============================================================================
// 1. Risk presets
// ============================================================================

function RiskPresets() {
  const [pending, setPending] = useState<string | null>(null);
  const [toast, setToast] = useState<{ kind: "ok" | "err"; text: string } | null>(
    null,
  );

  async function apply(name: keyof typeof PRESETS) {
    setPending(name);
    try {
      await apiFetch<ConfigResponse>("/api/config", {
        method: "POST",
        body: JSON.stringify(PRESETS[name]),
      });
      // Refresh every view that reflects config so the new values
      // show up everywhere immediately.
      mutate("/api/config");
      mutate("/api/status");
      setToast({ kind: "ok", text: `${name} preset applied` });
    } catch (err) {
      setToast({
        kind: "err",
        text: err instanceof Error ? err.message : `${name} preset failed`,
      });
    } finally {
      setPending(null);
      setTimeout(() => setToast(null), 4000);
    }
  }

  return (
    <div className="rounded-xl border border-border bg-panel p-5">
      <div className="mb-1 text-xs uppercase tracking-wide text-muted">
        Risk Presets
      </div>
      <p className="mb-4 text-xs text-muted">
        One-click profiles that overwrite every risk knob. Each preset
        maps to a complete <code>ConfigUpdateRequest</code> posted to{" "}
        <code>/api/config</code>.
      </p>
      <div className="grid gap-3 sm:grid-cols-3">
        {(Object.keys(PRESETS) as (keyof typeof PRESETS)[]).map((name) => (
          <button
            key={name}
            disabled={pending !== null}
            onClick={() => apply(name)}
            className={cn(
              "rounded-xl border px-5 py-4 text-left disabled:opacity-50",
              name === "Conservative" &&
                "border-accent/40 bg-accent/10 hover:bg-accent/20",
              name === "Balanced" &&
                "border-border bg-bg hover:border-accent/40",
              name === "Aggressive" &&
                "border-warn/40 bg-warn/10 hover:bg-warn/20",
            )}
          >
            <div className="text-sm font-semibold">{name}</div>
            <div className="mt-1 font-mono text-[11px] text-muted">
              max {fmtMoney(PRESETS[name].max_position_usd!)} · conf ≥{" "}
              {fmtPct(PRESETS[name].min_confidence!, 0)} · DD halt{" "}
              {fmtPct(PRESETS[name].daily_dd_halt!, 0)}
            </div>
          </button>
        ))}
      </div>
      <ToastView toast={toast} />
    </div>
  );
}

// ============================================================================
// 2. Risk config form
// ============================================================================

type FormState = {
  max_position_usd: string;
  max_portfolio_pct: string;
  crypto_max_position_usd: string;
  min_confidence: string;
  daily_dd_halt: string;
  weekly_dd_halt: string;
  total_dd_halt: string;
};

function toForm(c: ConfigResponse): FormState {
  return {
    max_position_usd: String(c.max_position_usd),
    max_portfolio_pct: String(c.max_portfolio_pct),
    crypto_max_position_usd: String(c.crypto_max_position_usd),
    min_confidence: String(c.min_confidence),
    daily_dd_halt: String(c.daily_dd_halt),
    weekly_dd_halt: String(c.weekly_dd_halt),
    total_dd_halt: String(c.total_dd_halt),
  };
}

function parseForm(f: FormState): {
  patch: ConfigUpdateRequest;
  errors: string[];
} {
  const errors: string[] = [];
  const patch: ConfigUpdateRequest = {};

  function num(
    k: keyof ConfigUpdateRequest,
    raw: string,
    label: string,
    min: number,
    max: number,
  ) {
    const v = parseFloat(raw);
    if (!Number.isFinite(v)) {
      errors.push(`${label} must be a number`);
      return;
    }
    if (v < min || v > max) {
      errors.push(`${label} must be between ${min} and ${max}`);
      return;
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (patch as any)[k] = v;
  }

  num("max_position_usd", f.max_position_usd, "Max position (USD)", 1, 1e9);
  num("max_portfolio_pct", f.max_portfolio_pct, "Max portfolio %", 0, 1);
  num(
    "crypto_max_position_usd",
    f.crypto_max_position_usd,
    "Crypto max position (USD)",
    0,
    1e9,
  );
  num("min_confidence", f.min_confidence, "Min confidence", 0, 1);
  num("daily_dd_halt", f.daily_dd_halt, "Daily DD halt", 0, 1);
  num("weekly_dd_halt", f.weekly_dd_halt, "Weekly DD halt", 0, 1);
  num("total_dd_halt", f.total_dd_halt, "Total DD halt", 0, 1);

  return { patch, errors };
}

function RiskConfigForm() {
  const { data, error, isLoading } = useSWR<ConfigResponse>(
    "/api/config",
    swrFetcher,
    { refreshInterval: 10000 },
  );
  const { data: status } = useSWR<StatusResponse>("/api/status", swrFetcher, {
    refreshInterval: 10000,
  });

  // Local form state — kept in a single object so a Save only POSTs
  // the delta (or, more precisely, the full current state — every
  // field is sent to make a "Save" act as "commit what I see").
  const [form, setForm] = useState<FormState | null>(null);
  const [saving, setSaving] = useState(false);
  const [toast, setToast] = useState<{ kind: "ok" | "err"; text: string } | null>(
    null,
  );

  // Seed / re-seed the form whenever the server returns a new config
  // snapshot. Skipped if the user already started editing — we don't
  // want to clobber their input on every SWR refresh.
  useEffect(() => {
    if (data && !form) setForm(toForm(data));
  }, [data, form]);

  // First load: show shimmer, don't try to render the form yet.
  if (error)
    return (
      <ErrorState message={String(error.message ?? error)} />
    );
  if (isLoading || !data || !form)
    return <ShimmerBox height={260} className="w-full" />;

  function update<K extends keyof FormState>(k: K, v: string) {
    setForm((prev) => (prev ? { ...prev, [k]: v } : prev));
  }

  async function save() {
    if (!form) return;
    const { patch, errors } = parseForm(form);
    if (errors.length) {
      setToast({ kind: "err", text: errors.join(" · ") });
      setTimeout(() => setToast(null), 5000);
      return;
    }
    setSaving(true);
    try {
      await apiFetch<ConfigResponse>("/api/config", {
        method: "POST",
        body: JSON.stringify(patch),
      });
      mutate("/api/config");
      mutate("/api/status");
      setToast({ kind: "ok", text: "Config saved" });
    } catch (err) {
      setToast({
        kind: "err",
        text: err instanceof Error ? err.message : "Save failed",
      });
    } finally {
      setSaving(false);
      setTimeout(() => setToast(null), 4000);
    }
  }

  // preview runs the same payload through /api/config?dry_run=true
  // and surfaces the would-be snapshot in a side panel. The form
  // values are NOT committed — the live config is untouched. This
  // gives operators a "demo" view of what a preset or hand-tweaked
  // form would do before they commit.
  const [previewing, setPreviewing] = useState(false);
  const [preview, setPreview] = useState<ConfigResponse | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);

  async function runPreview() {
    if (!form) return;
    const { patch, errors } = parseForm(form);
    if (errors.length) {
      setPreviewError(errors.join(" · "));
      setPreview(null);
      return;
    }
    setPreviewing(true);
    setPreviewError(null);
    try {
      const r = await apiFetch<ConfigResponse>(
        "/api/config?dry_run=true",
        { method: "POST", body: JSON.stringify(patch) },
      );
      setPreview(r);
    } catch (err) {
      setPreviewError(
        err instanceof Error ? err.message : "Preview failed",
      );
      setPreview(null);
    } finally {
      setPreviewing(false);
    }
  }

  async function reset() {
    if (data) setForm(toForm(data));
  }

  // Current values come from /api/status for the read-only fields
    // and /api/config for the bot-only knobs (status.config doesn't
    // carry min_confidence; the dedicated config endpoint does).
    const current = status?.config;
    const minConf = data?.min_confidence;
    return (
      <div className="rounded-xl border border-border bg-panel p-5">
        <div className="mb-1 flex items-center justify-between">
          <div className="text-xs uppercase tracking-wide text-muted">
            Risk Configuration (editable)
          </div>
          <button
            onClick={reset}
            disabled={saving}
            className="text-[10px] uppercase tracking-wide text-muted hover:text-accent disabled:opacity-50"
          >
            Reset
          </button>
        </div>
        <p className="mb-4 text-xs text-muted">
          Each input shows the current value beside it. Save commits the
          whole form to <code>/api/config</code> in one shot.
        </p>

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <NumField
            label="Max position (USD)"
            hint={current ? fmtMoney(current.max_position_usd) : "—"}
            value={form.max_position_usd}
            onChange={(v) => update("max_position_usd", v)}
          />
          <NumField
            label="Max portfolio %"
            hint={current ? fmtPct(current.max_portfolio_pct, 1) : "—"}
            value={form.max_portfolio_pct}
            step={0.01}
            onChange={(v) => update("max_portfolio_pct", v)}
          />
          <NumField
            label="Crypto max position (USD)"
            hint={current ? fmtMoney(current.crypto_max_position_usd) : "—"}
            value={form.crypto_max_position_usd}
            onChange={(v) => update("crypto_max_position_usd", v)}
          />
          <NumField
            label="Min confidence"
            hint={
              minConf !== undefined && minConf !== null && Number.isFinite(minConf)
                ? fmtPct(minConf, 0)
                : "—"
            }
            value={form.min_confidence}
            step={0.01}
            onChange={(v) => update("min_confidence", v)}
          />
          <NumField
            label="Daily DD halt"
            hint={current ? fmtPct(current.daily_dd_halt, 1) : "—"}
            value={form.daily_dd_halt}
            step={0.005}
            onChange={(v) => update("daily_dd_halt", v)}
          />
          <NumField
            label="Weekly DD halt"
            hint={current ? fmtPct(current.weekly_dd_halt, 1) : "—"}
            value={form.weekly_dd_halt}
            step={0.005}
            onChange={(v) => update("weekly_dd_halt", v)}
          />
          <NumField
            label="Total DD halt"
            hint={current ? fmtPct(current.total_dd_halt, 1) : "—"}
            value={form.total_dd_halt}
            step={0.005}
            onChange={(v) => update("total_dd_halt", v)}
          />
        </div>

      <div className="mt-4 flex items-center justify-end gap-3">
        <button
          disabled={saving || previewing}
          onClick={runPreview}
          className="rounded-lg border border-border bg-bg px-4 py-2 text-sm font-semibold text-zinc-200 hover:border-accent/40 disabled:opacity-50"
        >
          {previewing ? "Previewing…" : "Preview changes"}
        </button>
        <button
          disabled={saving}
          onClick={save}
          className="rounded-lg border border-accent/40 bg-accent/10 px-4 py-2 text-sm font-semibold text-accent hover:bg-accent/20 disabled:opacity-50"
        >
          {saving ? "Saving…" : "Save config"}
        </button>
      </div>

      {preview && (
        <div className="mt-3 rounded-lg border border-warn/40 bg-warn/10 p-3 text-xs">
          <div className="mb-1 flex items-center justify-between">
            <div className="font-semibold text-warn">
              Preview (not committed)
            </div>
            <button
              onClick={() => setPreview(null)}
              className="text-[10px] uppercase tracking-wide text-muted hover:text-accent"
            >
              Dismiss
            </button>
          </div>
          <p className="mb-2 text-muted">
            Live config is unchanged. The values below are what Save
            would commit.
          </p>
          <table className="w-full font-mono text-[11px]">
            <tbody>
              {(Object.keys(form ?? {}) as (keyof FormState)[]).map((k) => {
                const cur = data?.[k as keyof ConfigResponse] as number | undefined;
                const next = preview[k as keyof ConfigResponse] as number | undefined;
                if (cur === next) return null;
                return (
                  <tr key={k} className="border-b border-warn/20 last:border-0">
                    <td className="py-0.5 pr-3 text-muted">{k}</td>
                    <td className="py-0.5 pr-3 text-right text-muted line-through">
                      {k.includes("pct") || k.includes("halt")
                        ? fmtPct(Number(cur), 2)
                        : fmtMoney(Number(cur))}
                    </td>
                    <td className="py-0.5 text-right text-warn">
                      {k.includes("pct") || k.includes("halt")
                        ? fmtPct(Number(next), 2)
                        : fmtMoney(Number(next))}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {previewError && (
        <div className="mt-3 rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-xs text-danger">
          {previewError}
        </div>
      )}

      <ToastView toast={toast} />
    </div>
  );
}

function NumField({
  label,
  hint,
  value,
  step,
  onChange,
}: {
  label: string;
  hint?: string;
  value: string;
  step?: number;
  onChange: (v: string) => void;
}) {
  return (
    <label className="block">
      <div className="mb-1 flex items-baseline justify-between text-xs">
        <span className="text-muted">{label}</span>
        {hint && (
          <span className="font-mono text-[11px] text-muted">
            current: {hint}
          </span>
        )}
      </div>
      <input
        type="number"
        inputMode="decimal"
        step={step}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="w-full rounded-lg border border-border bg-bg px-3 py-2 font-mono text-sm text-zinc-100 focus:border-accent/60 focus:outline-none"
      />
    </label>
  );
}

// ============================================================================
// 3. Pause / Resume / Step (kept intact; the simplest controls)
// ============================================================================

function PauseStepRow() {
  const [pending, setPending] = useState<string | null>(null);
  const [toast, setToast] = useState<{ kind: "ok" | "err"; text: string } | null>(
    null,
  );

  async function act(path: string, label: string) {
    setPending(label);
    try {
      await apiFetch<ControlResponse>(path, { method: "POST", body: "{}" });
      mutate("/api/status");
      setToast({ kind: "ok", text: `${label} succeeded` });
    } catch (err) {
      setToast({
        kind: "err",
        text: err instanceof Error ? err.message : `${label} failed`,
      });
    } finally {
      setPending(null);
      setTimeout(() => setToast(null), 4000);
    }
  }

  return (
    <div className="grid gap-3 sm:grid-cols-3">
      <button
        disabled={pending !== null}
        onClick={() => act("/api/control/pause", "Pause new trades")}
        className="rounded-xl border border-warn/40 bg-warn/10 px-5 py-4 text-left hover:bg-warn/20 disabled:opacity-50"
      >
        <div className="text-sm font-semibold text-warn">Pause new trades</div>
        <div className="mt-1 text-xs text-muted">
          Skip new entries on every tick. Positions stay open.
        </div>
      </button>
      <button
        disabled={pending !== null}
        onClick={() => act("/api/control/resume", "Resume trading")}
        className="rounded-xl border border-accent/40 bg-accent/10 px-5 py-4 text-left hover:bg-accent/20 disabled:opacity-50"
      >
        <div className="text-sm font-semibold text-accent">Resume trading</div>
        <div className="mt-1 text-xs text-muted">
          Clear the pause flag; entries re-enabled.
        </div>
      </button>
      <button
        disabled={pending !== null}
        onClick={() => act("/api/control/step", "Run one decision cycle")}
        className="rounded-xl border border-border bg-panel px-5 py-4 text-left hover:border-accent disabled:opacity-50"
      >
        <div className="text-sm font-semibold">Run one decision cycle</div>
        <div className="mt-1 text-xs text-muted">
          Request a manual tick; bot runs the cycle on next interval.
        </div>
      </button>
      <ToastView toast={toast} />
    </div>
  );
}

// ============================================================================
// 4. Decision cycle preview
// ============================================================================

function DecisionCyclePanel() {
  const [pending, setPending] = useState(false);
  const [outcome, setOutcome] = useState<ControlResponse | null>(null);
  const [toast, setToast] = useState<{ kind: "ok" | "err"; text: string } | null>(
    null,
  );

  async function runStep() {
    setPending(true);
    try {
      const r = await apiFetch<ControlResponse>("/api/control/step", {
        method: "POST",
        body: "{}",
      });
      setOutcome(r);
      setToast({ kind: "ok", text: r.result || "Cycle complete" });
    } catch (err) {
      setToast({
        kind: "err",
        text: err instanceof Error ? err.message : "Cycle failed",
      });
    } finally {
      setPending(false);
      setTimeout(() => setToast(null), 4000);
    }
  }

  const d = outcome?.decision;
  return (
    <div className="rounded-xl border border-border bg-panel p-5">
      <div className="mb-1 flex items-center justify-between">
        <div className="text-xs uppercase tracking-wide text-muted">
          Last Decision Cycle Preview
        </div>
        <button
          disabled={pending}
          onClick={runStep}
          className="rounded-lg border border-accent/40 bg-accent/10 px-3 py-1 text-xs font-semibold text-accent hover:bg-accent/20 disabled:opacity-50"
        >
          {pending ? "Running…" : "Run cycle now"}
        </button>
      </div>
      <p className="mb-4 text-xs text-muted">
        Triggers <code>/api/control/step</code> and renders the factor
        scores + rationale the bot returned, so judges see the
        reasoning even when no trade was placed.
      </p>

      {!d && (
        <div className="rounded-lg border border-dashed border-border bg-bg/40 p-4 text-center text-xs text-muted">
          No cycle yet — click "Run cycle now" to populate.
        </div>
      )}
      {d && (
        <div className="space-y-3">
          <div className="grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
            <div>
              <div className="text-muted">Symbol</div>
              <div className="font-mono">{d.symbol || "—"}</div>
            </div>
            <div>
              <div className="text-muted">Source</div>
              <div className="font-mono">{d.source || "—"}</div>
            </div>
            <div>
              <div className="text-muted">Risk verdict</div>
              <div className="font-mono">
                {d.risk || "INFO"}
              </div>
            </div>
            <div>
              <div className="text-muted">Confidence</div>
              <div className="font-mono">
                {d.confidence !== undefined && d.confidence !== null
                  ? fmtPct(d.confidence, 2)
                  : "—"}
              </div>
            </div>
          </div>
          {d.factor_scores && Object.keys(d.factor_scores).length > 0 && (
            <div className="space-y-1.5">
              <div className="text-[11px] uppercase tracking-wide text-muted">
                Factor scores
              </div>
              {Object.entries(d.factor_scores).map(([k, v]) => (
                <div
                  key={k}
                  className="grid grid-cols-12 items-center gap-2 text-xs"
                >
                  <div className="col-span-3 capitalize text-muted">{k}</div>
                  <div className="col-span-7 h-2 overflow-hidden rounded-full bg-bg">
                    <div
                      className={cn(
                        "h-full",
                        v >= 0.5 ? "bg-accent" : v >= 0 ? "bg-warn" : "bg-danger",
                      )}
                      style={{
                        width: `${Math.min(100, Math.abs(v) * 100)}%`,
                      }}
                    />
                  </div>
                  <div className="col-span-2 text-right font-mono">
                    {v.toFixed(2)}
                  </div>
                </div>
              ))}
            </div>
          )}
          {d.detail && (
            <div className="rounded-md border border-border bg-bg p-3 text-xs text-zinc-300">
              {d.detail}
            </div>
          )}
        </div>
      )}
      <ToastView toast={toast} />
    </div>
  );
}

// ============================================================================
// 5. Manual trade form
// ============================================================================

function ManualTradeForm() {
  const { data: status } = useSWR<StatusResponse>("/api/status", swrFetcher, {
    refreshInterval: 10000,
  });
  const symbols = useMemo<string[]>(
    () => status?.config.symbols ?? [],
    [status],
  );

  const [symbol, setSymbol] = useState<string>("");
  const [side, setSide] = useState<"buy" | "sell">("buy");
  const [mode, setMode] = useState<"qty" | "notional">("qty");
  const [qty, setQty] = useState<string>("");
  const [notional, setNotional] = useState<string>("");
  const [limitPrice, setLimitPrice] = useState<string>("");
  const [orderType, setOrderType] = useState<"market" | "limit">("market");
  const [tif, setTif] = useState<"day" | "gtc">("day");
  const [confidence, setConfidence] = useState<string>("");

  const [pending, setPending] = useState<"simulate" | "execute" | null>(null);
  const [simResult, setSimResult] = useState<TradeSimulationResponse | null>(
    null,
  );
  const [execResult, setExecResult] = useState<TradeExecutionResponse | null>(
    null,
  );
  const [error, setError] = useState<string | null>(null);

  function buildProposal(): TradeProposalRequest | null {
    const sym = symbol.trim().toUpperCase();
    if (!sym) {
      setError("Symbol required");
      return null;
    }
    if (mode === "qty" && !qty.trim()) {
      setError("Qty required (or switch to Notional)");
      return null;
    }
    if (mode === "notional" && !notional.trim()) {
      setError("Notional required (or switch to Qty)");
      return null;
    }
    if (orderType === "limit" && !limitPrice.trim()) {
      setError("Limit price required for limit orders");
      return null;
    }
    const confN = confidence.trim() === "" ? undefined : parseFloat(confidence);
    if (confN !== undefined && !Number.isFinite(confN)) {
      setError("Confidence must be a number");
      return null;
    }
    setError(null);
    const req: TradeProposalRequest = {
      symbol: sym,
      side,
      order_type: orderType,
      time_in_force: tif,
      confidence: confN,
    };
    if (mode === "qty") req.qty = qty.trim();
    else req.notional = notional.trim();
    if (orderType === "limit") req.limit_price = limitPrice.trim();
    return req;
  }

  async function run(kind: "simulate" | "execute") {
    const req = buildProposal();
    if (!req) return;
    setPending(kind);
    try {
      if (kind === "simulate") {
        const r = await apiFetch<TradeSimulationResponse>(
          "/api/trade/simulate",
          { method: "POST", body: JSON.stringify(req) },
        );
        setSimResult(r);
        setExecResult(null);
      } else {
        const r = await apiFetch<TradeExecutionResponse>(
          "/api/trade/execute",
          { method: "POST", body: JSON.stringify(req) },
        );
        setExecResult(r);
        setSimResult(null);
        mutate("/api/decisions");
        mutate("/api/positions");
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : `${kind} failed`);
    } finally {
      setPending(null);
    }
  }

  return (
    <div className="rounded-xl border border-border bg-panel p-5">
      <div className="mb-1 text-xs uppercase tracking-wide text-muted">
        Manual Trade
      </div>
      <p className="mb-4 text-xs text-muted">
        Build a proposal by hand. <strong>Simulate only</strong> runs
        it through the bot's risk validator and returns the
        would-have-sent envelope. <strong>Send real paper order</strong>{" "}
        places the order via Alpaca (still paper) when approved.
      </p>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-6">
        <SymbolField
          className="sm:col-span-2"
          value={symbol}
          onChange={setSymbol}
          options={symbols}
        />
        <SegField
          className="sm:col-span-1"
          label="Side"
          value={side}
          onChange={(v) => setSide(v)}
          options={[
            { value: "buy", label: "Buy" },
            { value: "sell", label: "Sell" },
          ]}
        />
        <SegField
          className="sm:col-span-1"
          label="Sizing"
          value={mode}
          onChange={(v) => setMode(v)}
          options={[
            { value: "qty", label: "Qty" },
            { value: "notional", label: "Notional" },
          ]}
        />
        <SegField
          className="sm:col-span-1"
          label="Type"
          value={orderType}
          onChange={(v) => setOrderType(v)}
          options={[
            { value: "market", label: "Market" },
            { value: "limit", label: "Limit" },
          ]}
        />
        <SegField
          className="sm:col-span-1"
          label="TIF"
          value={tif}
          onChange={(v) => setTif(v)}
          options={[
            { value: "day", label: "Day" },
            { value: "gtc", label: "GTC" },
          ]}
        />
      </div>

      <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-4">
        <label className="block sm:col-span-1">
          <div className="mb-1 text-xs text-muted">
            {mode === "qty" ? "Quantity (shares)" : "Notional (USD)"}
          </div>
          <input
            type="number"
            inputMode="decimal"
            value={mode === "qty" ? qty : notional}
            onChange={(e) =>
              mode === "qty" ? setQty(e.target.value) : setNotional(e.target.value)
            }
            placeholder={mode === "qty" ? "e.g. 5" : "e.g. 500"}
            className="w-full rounded-lg border border-border bg-bg px-3 py-2 font-mono text-sm text-zinc-100 focus:border-accent/60 focus:outline-none"
          />
        </label>
        <label className="block sm:col-span-1">
          <div className="mb-1 text-xs text-muted">Limit price</div>
          <input
            type="number"
            inputMode="decimal"
            value={limitPrice}
            onChange={(e) => setLimitPrice(e.target.value)}
            disabled={orderType !== "limit"}
            placeholder={orderType === "limit" ? "e.g. 195.50" : "n/a"}
            className="w-full rounded-lg border border-border bg-bg px-3 py-2 font-mono text-sm text-zinc-100 disabled:opacity-40 focus:border-accent/60 focus:outline-none"
          />
        </label>
        <label className="block sm:col-span-1">
          <div className="mb-1 text-xs text-muted">Confidence (0–1)</div>
          <input
            type="number"
            inputMode="decimal"
            step={0.01}
            value={confidence}
            onChange={(e) => setConfidence(e.target.value)}
            placeholder="e.g. 0.6"
            className="w-full rounded-lg border border-border bg-bg px-3 py-2 font-mono text-sm text-zinc-100 focus:border-accent/60 focus:outline-none"
          />
        </label>
      </div>

      {error && (
        <div className="mt-3 rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-xs text-danger">
          {error}
        </div>
      )}

      <div className="mt-4 grid gap-3 sm:grid-cols-2">
        <button
          disabled={pending !== null}
          onClick={() => run("simulate")}
          className="rounded-xl border border-border bg-bg px-5 py-4 text-left hover:border-accent disabled:opacity-50"
        >
          <div className="text-sm font-semibold">Simulate only</div>
          <div className="mt-1 text-xs text-muted">
            Run risk validator; no order sent.
          </div>
        </button>
        <button
          disabled={pending !== null}
          onClick={() => run("execute")}
          className="rounded-xl border border-accent/40 bg-accent/10 px-5 py-4 text-left hover:bg-accent/20 disabled:opacity-50"
        >
          <div className="text-sm font-semibold text-accent">
            Send real paper order
          </div>
          <div className="mt-1 text-xs text-muted">
            Posts to Alpaca (paper); journals the decision.
          </div>
        </button>
      </div>

      {(simResult || execResult) && (
        <div className="mt-4">
          <VerdictPanel
            sim={simResult}
            exec={execResult}
            onClear={() => {
              setSimResult(null);
              setExecResult(null);
            }}
          />
        </div>
      )}
    </div>
  );
}

function VerdictPanel({
  sim,
  exec,
  onClear,
}: {
  sim: TradeSimulationResponse | null;
  exec: TradeExecutionResponse | null;
  onClear: () => void;
}) {
  const approved = sim?.approved ?? exec?.approved ?? false;
  const reasons = sim?.reasons ?? exec?.reasons ?? [];
  const order: TradeOrder | null =
    sim?.would_have_sent ?? exec?.order ?? null;
  const notional = sim?.notional ?? exec?.notional ?? 0;
  const mode = exec?.mode ?? "simulated";
  // Show "View in Trade Log" only when an order actually went to
  // the broker (mode=live). Simulated decisions are journaled but the
  // operator probably wants to see the verdict and move on.
  const showTradesLink = exec?.mode === "live";

  return (
    <div
      className={cn(
        "rounded-xl border p-4",
        approved
          ? "border-accent/40 bg-accent/10"
          : "border-danger/40 bg-danger/10",
      )}
    >
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2">
          <span
            className={cn(
              "rounded-md border px-2 py-0.5 text-[10px] uppercase tracking-wide",
              approved
                ? "border-accent/60 bg-accent/20 text-accent"
                : "border-danger/60 bg-danger/20 text-danger",
            )}
          >
            Risk check: {approved ? "PASSED" : "FAILED"}
          </span>
          {exec && (
            <span className="rounded-md border border-border bg-bg px-2 py-0.5 text-[10px] uppercase tracking-wide text-muted">
              Mode: {mode}
            </span>
          )}
          <span className="font-mono text-xs text-muted">
            notional ≈ {fmtMoney(notional)}
          </span>
        </div>
        <div className="flex items-center gap-3">
          {showTradesLink && (
            <Link
              href="/trades"
              className="text-[10px] uppercase tracking-wide text-accent hover:text-zinc-100"
            >
              View in Trade Log →
            </Link>
          )}
          <button
            onClick={onClear}
            className="text-[10px] uppercase tracking-wide text-muted hover:text-accent"
          >
            Dismiss
          </button>
        </div>
      </div>
      {reasons.length > 0 && (
        <ul className="mt-2 list-disc space-y-0.5 pl-5 text-xs text-zinc-300">
          {reasons.map((r, i) => (
            <li key={i}>{r}</li>
          ))}
        </ul>
      )}
      {order && (
        <div className="mt-3">
          <div className="mb-1 text-[11px] uppercase tracking-wide text-muted">
            {exec ? "Order sent" : "Would have sent"}
          </div>
          <pre className="overflow-x-auto rounded-md border border-border bg-bg p-3 text-[11px] leading-relaxed text-zinc-300">
            {JSON.stringify(order, null, 2)}
          </pre>
        </div>
      )}
    </div>
  );
}

function SymbolField({
  className,
  value,
  onChange,
  options,
}: {
  className?: string;
  value: string;
  onChange: (v: string) => void;
  options: string[];
}) {
  const datalistId = "controls-symbol-list";
  return (
    <label className={cn("block", className)}>
      <div className="mb-1 text-xs text-muted">Symbol</div>
      <input
        list={datalistId}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="e.g. AAPL"
        autoCapitalize="characters"
        spellCheck={false}
        className="w-full rounded-lg border border-border bg-bg px-3 py-2 font-mono text-sm uppercase text-zinc-100 focus:border-accent/60 focus:outline-none"
      />
      <datalist id={datalistId}>
        {options.map((s) => (
          <option key={s} value={s} />
        ))}
      </datalist>
    </label>
  );
}

function SegField<T extends string>({
  className,
  label,
  value,
  onChange,
  options,
}: {
  className?: string;
  label: string;
  value: T;
  onChange: (v: T) => void;
  options: { value: T; label: string }[];
}) {
  return (
    <div className={cn("block", className)}>
      <div className="mb-1 text-xs text-muted">{label}</div>
      <div className="grid grid-flow-col gap-1 rounded-lg border border-border bg-bg p-1">
        {options.map((o) => (
          <button
            key={o.value}
            onClick={() => onChange(o.value)}
            className={cn(
              "rounded-md px-2 py-1 text-xs",
              value === o.value
                ? "bg-accent/15 text-accent"
                : "text-muted hover:text-zinc-100",
            )}
          >
            {o.label}
          </button>
        ))}
      </div>
    </div>
  );
}

// ============================================================================
// Operator token card (unchanged from original page)
// ============================================================================

function OperatorTokenCard() {
  const [token, setToken] = useState<string>(() =>
    typeof window !== "undefined" ? getDashboardToken() ?? "" : "",
  );

  return (
    <div className="rounded-xl border border-border bg-panel p-5">
      <div className="mb-1 text-xs uppercase tracking-wide text-muted">
        Operator token (optional)
      </div>
      <p className="mb-3 text-xs text-muted">
        Needed only when the API requires a bearer token
        (<code>DASHBOARD_TOKEN</code> on the server) or when this
        dashboard is hosted on a different origin. Stored in this
        browser only — never sent except as an{" "}
        <code>Authorization</code> header on control actions.
      </p>
      <input
        type="password"
        autoComplete="off"
        spellCheck={false}
        placeholder="Bearer token for pause / resume / step / trade"
        value={token}
        onChange={(e) => {
          setToken(e.target.value);
          setDashboardToken(e.target.value);
        }}
        className="w-full rounded-lg border border-border bg-bg px-3 py-2 font-mono text-sm text-zinc-100 placeholder:text-muted/60 focus:border-accent/60 focus:outline-none"
      />
    </div>
  );
}

function ToastView({ toast }: { toast: { kind: "ok" | "err"; text: string } | null }) {
  return (
    <AnimatePresence>
      {toast && (
        <motion.div
          initial={{ opacity: 0, y: 12 }}
          animate={{ opacity: 1, y: 0 }}
          exit={{ opacity: 0, y: 12 }}
          transition={{ duration: 0.2 }}
          className={cn(
            "fixed bottom-6 left-1/2 z-50 -translate-x-1/2 rounded-lg border px-4 py-3 text-sm shadow-lg",
            toast.kind === "ok"
              ? "border-accent/40 bg-accent/15 text-accent"
              : "border-danger/40 bg-danger/15 text-danger",
          )}
        >
          {toast.text}
        </motion.div>
      )}
    </AnimatePresence>
  );
}