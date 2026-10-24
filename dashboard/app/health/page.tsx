"use client";

// Self-adjustment view: what the bot has learned from its own closed
// trades, and what its self-heal pass last found.
//
// Both panels read the files the trading bot writes, so they show the
// numbers an order was actually placed against — not a recomputed guess.

import useSWR from "swr";
import { swrFetcher } from "@/lib/api";
import type {
  LearningResponse,
  SelfHealResponse,
} from "@/lib/api";
import { cn } from "@/lib/utils";
import {
  ErrorState,
  PageShell,
  ShimmerBox,
  StaggerItem,
  StaggerList,
} from "@/components/shimmer";
import { SwrProvider } from "@/components/swr-provider";

export default function HealthPage() {
  return (
    <SwrProvider>
      <PageShell className="space-y-6">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">
            Learning &amp; Self-Heal
          </h1>
          <p className="mt-1 text-sm text-muted">
            The bot adjusts its own entry bar, sizing and stops from
            realised results — always toward less risk, never more — and
            repairs the states it falls into on its own.
          </p>
        </div>
        <LearningPanel />
        <QuarantinePanel />
        <SelfHealPanel />
      </PageShell>
    </SwrProvider>
  );
}

function pct(x: number, digits = 1): string {
  if (!Number.isFinite(x)) return "—";
  return `${(x * 100).toFixed(digits)}%`;
}

function signed(x: number, digits = 2): string {
  if (!Number.isFinite(x)) return "—";
  const v = x.toFixed(digits);
  return x > 0 ? `+${v}` : v;
}

function StatusChip({ ok, yes, no }: { ok: boolean; yes: string; no: string }) {
  return (
    <span
      className={cn(
        "rounded-full border px-2 py-0.5 text-xs uppercase",
        ok
          ? "border-accent/40 bg-accent/15 text-accent"
          : "border-border bg-panel text-muted",
      )}
    >
      {ok ? yes : no}
    </span>
  );
}

// LearningPanel shows the snapshot the bot is applying right now.
function LearningPanel() {
  const { data, error, isLoading } = useSWR<LearningResponse>(
    "/api/learning",
    swrFetcher,
    { refreshInterval: 15000 },
  );

  if (error) {
    return (
      <section className="space-y-3">
        <h2 className="text-lg font-medium">What the bot has learned</h2>
        <ErrorState message={String(error)} />
      </section>
    );
  }
  if (isLoading || !data) {
    return (
      <section className="space-y-3">
        <h2 className="text-lg font-medium">What the bot has learned</h2>
        <ShimmerBox height={190} className="w-full" />
      </section>
    );
  }

  const a = data.adjustments;
  const classes = Object.values(a.classes ?? {});

  return (
    <section className="space-y-3">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="text-lg font-medium">What the bot has learned</h2>
        <div className="flex items-center gap-2 text-xs text-muted">
          <StatusChip ok={data.enabled} yes="learning on" no="learning off" />
          <StatusChip
            ok={a.active}
            yes="adjustments applied"
            no="below trade bar"
          />
        </div>
      </div>

      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <Stat label="Closed trades" value={String(a.samples_seen ?? 0)} />
        <Stat
          label="Needs before it speaks"
          value={`${data.bounds.min_trades}`}
          hint={`recalibrates every ${data.bounds.every_trades} more`}
        />
        <Stat
          label="Entry floor"
          value={signed(a.score_floor_add ?? 0, 3)}
          hint={
            (a.score_floor_add ?? 0) > 0
              ? `raised to ${a.suggested_floor?.toFixed(3)}`
              : "operator floor unchanged"
          }
        />
        <Stat
          label="Last computed"
          value={a.computed_at ? new Date(a.computed_at).toLocaleString() : "—"}
        />
      </div>

      {!a.active ? (
        <div className="rounded-xl border border-border bg-panel p-5 text-sm text-muted">
          Nothing is being applied. The bot records every closed trade with
          the score it acted on, and once {data.bounds.min_trades} round
          trips exist the learner starts speaking. Until then the operator’s
          thresholds stand exactly as configured.
        </div>
      ) : (
        <>
          {classes.length === 0 ? (
            <div className="rounded-xl border border-border bg-panel p-5 text-sm text-muted">
              Active, but no asset class has {data.bounds.min_samples_per_class}{" "}
              trades yet, so sizing and stops are untouched.
            </div>
          ) : (
            <div className="overflow-x-auto rounded-xl border border-border bg-panel">
              <table className="w-full text-sm">
                <thead className="text-left text-xs uppercase tracking-wide text-muted">
                  <tr>
                    <th className="px-4 py-3">Class</th>
                    <th className="px-4 py-3 text-right">Trades</th>
                    <th className="px-4 py-3 text-right">Win rate</th>
                    <th className="px-4 py-3 text-right">Expectancy</th>
                    <th className="px-4 py-3 text-right">Size</th>
                    <th className="px-4 py-3 text-right">Stop</th>
                    <th className="px-4 py-3">Evidence</th>
                  </tr>
                </thead>
                <tbody>
                  {classes.map((c) => (
                    <tr key={c.class} className="border-t border-border">
                      <td className="px-4 py-3 font-medium capitalize">
                        {c.class}
                      </td>
                      <td className="px-4 py-3 text-right tabular-nums">
                        {c.samples}
                      </td>
                      <td
                        className={cn(
                          "px-4 py-3 text-right tabular-nums",
                          c.win_rate >= 0.5 ? "text-accent" : "text-danger",
                        )}
                      >
                        {pct(c.win_rate, 0)}
                      </td>
                      <td
                        className={cn(
                          "px-4 py-3 text-right tabular-nums",
                          c.expectancy_pct >= 0 ? "text-accent" : "text-danger",
                        )}
                      >
                        {pct(c.expectancy_pct, 2)}
                      </td>
                      <td className="px-4 py-3 text-right tabular-nums">
                        {c.size_multiplier >= 1 ? (
                          "full"
                        ) : (
                          <span className="text-danger">
                            {c.size_multiplier.toFixed(2)}×
                          </span>
                        )}
                      </td>
                      <td className="px-4 py-3 text-right tabular-nums">
                        {c.stop_multiplier >= 1 ? (
                          "as set"
                        ) : (
                          <span className="text-accent">
                            {c.stop_multiplier.toFixed(2)}× tighter
                          </span>
                        )}
                      </td>
                      <td className="px-4 py-3 text-xs text-muted">
                        {c.reason}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          <div className="rounded-xl border border-border bg-panel p-4">
            <h3 className="text-xs uppercase tracking-wide text-muted">
              What each score actually bought
            </h3>
            <p className="mt-1 text-xs text-muted">
              The calibration table: entries grouped by the composite the bot
              acted on. Only trades that carried a score appear here — a hand
              fill has no score, and inventing one would fabricate evidence.
            </p>
            <ul className="mt-3 grid gap-1 text-sm sm:grid-cols-2">
              {(a.buckets ?? []).map((b) => (
                <li
                  key={`${b.from}-${b.to}`}
                  className="flex items-baseline justify-between rounded-lg bg-bg/60 px-3 py-2"
                >
                  <span className="tabular-nums text-muted">
                    {b.from.toFixed(2)}–{b.to.toFixed(2)}
                  </span>
                  <span className="tabular-nums">
                    {b.samples === 0 ? (
                      <span className="text-muted">no trades</span>
                    ) : (
                      <>
                        {b.samples} trade{b.samples === 1 ? "" : "s"},{" "}
                        <span
                          className={
                            b.expectancy_pct >= 0 ? "text-accent" : "text-danger"
                          }
                        >
                          {pct(b.expectancy_pct, 2)}
                        </span>
                      </>
                    )}
                  </span>
                </li>
              ))}
            </ul>
          </div>

          {(a.notes ?? []).length > 0 && (
            <div className="rounded-xl border border-border bg-panel p-4">
              <h3 className="text-xs uppercase tracking-wide text-muted">
                Decisions this calibration made
              </h3>
              <ul className="mt-2 space-y-1 text-sm">
                {(a.notes ?? []).map((n, i) => (
                  <li key={i} className="text-muted">
                    {n}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </>
      )}
    </section>
  );
}

function Stat({
  label,
  value,
  hint,
}: {
  label: string;
  value: string;
  hint?: string;
}) {
  return (
    <div className="rounded-xl border border-border bg-panel px-4 py-3">
      <div className="text-xs uppercase tracking-wide text-muted">{label}</div>
      <div className="mt-1 text-lg font-semibold tabular-nums">{value}</div>
      {hint && <div className="mt-0.5 text-xs text-muted">{hint}</div>}
    </div>
  );
}

// QuarantinePanel lists the symbols blocked from new entries, and says
// plainly that a block is a brake rather than a punishment.
function QuarantinePanel() {
  const { data, isLoading } = useSWR<LearningResponse>(
    "/api/learning",
    swrFetcher,
    { refreshInterval: 15000 },
  );
  if (isLoading || !data || !data.adjustments.active) return null;
  const q = data.adjustments.quarantined ?? [];
  if (q.length === 0) return null;
  return (
    <section className="space-y-3">
      <div className="flex items-baseline justify-between">
        <h2 className="text-lg font-medium">Quarantined symbols</h2>
        <span className="text-xs text-muted">
          blocked for new entries, released when recent trades say otherwise
        </span>
      </div>
      <StaggerList>
        {q.map((item) => (
          <StaggerItem key={item.symbol}>
            <div className="flex items-center justify-between gap-4 rounded-xl border border-danger/30 bg-danger/5 px-4 py-3">
              <div>
                <div className="font-medium">{item.symbol}</div>
                <div className="text-xs text-muted">{item.reason}</div>
              </div>
              <div className="text-right text-xs tabular-nums text-muted">
                {item.samples} trades
                <div>{pct(item.win_rate, 0)}</div>
              </div>
            </div>
          </StaggerItem>
        ))}
      </StaggerList>
    </section>
  );
}

const SEVERITY_TONE: Record<string, string> = {
  critical: "border-danger/40 bg-danger/10 text-danger",
  high: "border-danger/40 bg-danger/10 text-danger",
  medium: "border-warn/40 bg-warn/10 text-warn",
  low: "border-border bg-panel text-muted",
};

// SelfHealPanel shows the last pass. A pass that could not read the
// account is rendered as a finding, never as clean.
function SelfHealPanel() {
  const { data, error, isLoading } = useSWR<SelfHealResponse>(
    "/api/selfheal",
    swrFetcher,
    { refreshInterval: 15000 },
  );

  if (error) {
    return (
      <section className="space-y-3">
        <h2 className="text-lg font-medium">Self-heal</h2>
        <ErrorState message={String(error)} />
      </section>
    );
  }
  if (isLoading || !data) {
    return (
      <section className="space-y-3">
        <h2 className="text-lg font-medium">Self-heal</h2>
        <ShimmerBox height={160} className="w-full" />
      </section>
    );
  }

  const actions = data.report.actions ?? [];
  const errs = data.report.errors ?? [];
  const clean = actions.length === 0 && errs.length === 0;

  return (
    <section className="space-y-3">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="text-lg font-medium">Self-heal</h2>
        <div className="flex items-center gap-2 text-xs">
          <StatusChip ok={data.enabled} yes="enabled" no="disabled" />
          <StatusChip
            ok={data.report_only}
            yes="report only"
            no="repairs enabled"
          />
          <span className="text-muted">
            every {data.every_ticks} ticks
            {data.report.at ? ` · last ${new Date(data.report.at).toLocaleString()}` : " · not run yet"}
          </span>
        </div>
      </div>

      {!data.enabled ? (
        <div className="rounded-xl border border-border bg-panel p-5 text-sm text-muted">
          SELFHEAL_ENABLED is false, so degraded state (an orphaned
          protective order, a duplicate bracket, a stale resting entry) is
          left for an operator to find in the logs.
        </div>
      ) : clean ? (
        <div className="rounded-xl border border-accent/30 bg-accent/5 p-5 text-sm text-accent">
          Last pass found nothing: no orphaned protective orders, no
          duplicate brackets, no unreadable state.
        </div>
      ) : (
        <div className="overflow-x-auto rounded-xl border border-border bg-panel">
          <table className="w-full text-sm">
            <thead className="text-left text-xs uppercase tracking-wide text-muted">
              <tr>
                <th className="px-4 py-3">Check</th>
                <th className="px-4 py-3">Severity</th>
                <th className="px-4 py-3">Action</th>
                <th className="px-4 py-3">Finding</th>
              </tr>
            </thead>
            <tbody>
              {actions.map((a, i) => (
                <tr key={`${a.check}-${i}`} className="border-t border-border">
                  <td className="px-4 py-3 font-medium">{a.check}</td>
                  <td className="px-4 py-3">
                    <span
                      className={cn(
                        "rounded-full border px-2 py-0.5 text-xs uppercase",
                        SEVERITY_TONE[a.severity] ?? SEVERITY_TONE.low,
                      )}
                    >
                      {a.severity}
                    </span>
                  </td>
                  <td className="px-4 py-3">
                    {a.repaired ? (
                      <span className="text-accent">repaired</span>
                    ) : (
                      <span className="text-muted">found, not repaired</span>
                    )}
                  </td>
                  <td className="px-4 py-3 text-xs text-muted">{a.detail}</td>
                </tr>
              ))}
              {errs.map((e, i) => (
                <tr key={`err-${i}`} className="border-t border-border">
                  <td className="px-4 py-3 font-medium">unavailable</td>
                  <td className="px-4 py-3">
                    <span className="rounded-full border border-warn/40 bg-warn/10 px-2 py-0.5 text-xs uppercase text-warn">
                      skipped
                    </span>
                  </td>
                  <td className="px-4 py-3 text-muted">—</td>
                  <td className="px-4 py-3 text-xs text-muted">
                    check not evaluated: {e}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
