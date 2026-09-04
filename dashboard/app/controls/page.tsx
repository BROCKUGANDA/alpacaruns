"use client";

// Controls page: read-only config table + 3 actions (pause, resume,
// step). All actions POST to /api/control/* and surface the result
// in a toast. No API keys are ever rendered.

import { useState } from "react";
import useSWR, { mutate } from "swr";
import { motion, AnimatePresence } from "framer-motion";
import {
  apiFetch,
  getDashboardToken,
  setDashboardToken,
  swrFetcher,
  type ControlResponse,
  type StatusResponse,
} from "@/lib/api";
import { fmtMoney, fmtPct } from "@/lib/utils";
import {
  ErrorState,
  PageShell,
  ShimmerBox,
} from "@/components/shimmer";
import { SwrProvider } from "@/components/swr-provider";

export default function ControlsPage() {
  const { data, error, isLoading } = useSWR<StatusResponse>(
    "/api/status",
    swrFetcher,
    { refreshInterval: 5000 },
  );

  return (
    <SwrProvider>
      <PageShell className="space-y-6">
        <h1 className="text-2xl font-semibold tracking-tight">Controls</h1>

        {error ? (
          <ErrorState message={String(error)} />
        ) : isLoading || !data ? (
          <ShimmerBox height={200} className="w-full" />
        ) : (
          <ConfigTable cfg={data.config} paused={data.paused} />
        )}

        <ActionButtons />
      </PageShell>
    </SwrProvider>
  );
}

function ConfigTable({
  cfg,
  paused,
}: {
  cfg: StatusResponse["config"];
  paused: boolean;
}) {
  const rows = [
    { label: "Bot state", value: paused ? "Paused" : "Running" },
    { label: "LLM provider", value: cfg.llm_provider || "—" },
    { label: "Max position (USD)", value: fmtMoney(cfg.max_position_usd) },
    {
      label: "Crypto max position (USD)",
      value: fmtMoney(cfg.crypto_max_position_usd),
    },
    { label: "Max portfolio %", value: fmtPct(cfg.max_portfolio_pct, 1) },
    { label: "Daily DD halt", value: fmtPct(cfg.daily_dd_halt, 1) },
    { label: "Weekly DD halt", value: fmtPct(cfg.weekly_dd_halt, 1) },
    { label: "Total DD halt", value: fmtPct(cfg.total_dd_halt, 1) },
    { label: "Options overlay", value: cfg.options_enabled ? "enabled" : "disabled" },
    { label: "Ensemble layer", value: cfg.ensemble_enabled ? "enabled" : "disabled" },
    { label: "Symbol universe", value: cfg.symbols.join(", ") },
  ];
  return (
    <div className="rounded-xl border border-border bg-panel p-5">
      <div className="mb-3 text-xs uppercase tracking-wide text-muted">
        Runtime Configuration (read-only)
      </div>
      <table className="w-full text-sm">
        <tbody className="divide-y divide-border">
          {rows.map((r) => (
            <tr key={r.label}>
              <td className="py-2 text-muted">{r.label}</td>
              <td className="py-2 text-right font-mono">{r.value}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function ActionButtons() {
  const [pending, setPending] = useState<string | null>(null);
  const [token, setToken] = useState<string>(() =>
    typeof window !== "undefined" ? getDashboardToken() ?? "" : "",
  );
  const [toast, setToast] = useState<{
    kind: "ok" | "err";
    text: string;
  } | null>(null);

  async function act(path: string, label: string) {
    setPending(label);
    try {
      const result = await apiFetch<ControlResponse>(path, {
        method: "POST",
        body: "{}",
      });
      // Re-fetch status so the toggle UI updates immediately.
      mutate("/api/status");
      setToast({ kind: "ok", text: result.result || `${label} succeeded` });
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
    <>
      <div className="rounded-xl border border-border bg-panel p-5">
        <div className="mb-1 text-xs uppercase tracking-wide text-muted">
          Operator token (optional)
        </div>
        <p className="mb-3 text-xs text-muted">
          Needed only when the API requires a bearer token (DASHBOARD_TOKEN
          on the server) or when this dashboard is hosted on a different
          origin. Stored in this browser only — never sent except as an
          Authorization header on control actions.
        </p>
        <input
          type="password"
          autoComplete="off"
          spellCheck={false}
          placeholder="Bearer token for pause / resume / step"
          value={token}
          onChange={(e) => {
            setToken(e.target.value);
            setDashboardToken(e.target.value);
          }}
          className="w-full rounded-lg border border-border bg-bg px-3 py-2 font-mono text-sm text-zinc-100 placeholder:text-muted/60 focus:border-accent/60 focus:outline-none"
        />
      </div>
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
      </div>

      <AnimatePresence>
        {toast && (
          <motion.div
            initial={{ opacity: 0, y: 12 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: 12 }}
            transition={{ duration: 0.2 }}
            className={
              "fixed bottom-6 left-1/2 z-50 -translate-x-1/2 rounded-lg border px-4 py-3 text-sm shadow-lg " +
              (toast.kind === "ok"
                ? "border-accent/40 bg-accent/15 text-accent"
                : "border-danger/40 bg-danger/15 text-danger")
            }
          >
            {toast.text}
          </motion.div>
        )}
      </AnimatePresence>
    </>
  );
}