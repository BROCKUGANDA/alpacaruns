"use client";

// Agent brain view. Shows:
//   - Open positions (qty, entry, current, P&L, change-today, hold time)
//   - Stance weights (synthetic blend from FACTOR_WEIGHTS)
//   - Recent decisions (last 20 from /api/decisions, newest first)
//
// All data is read-only; no controls here. Polls /api/positions and
// /api/decisions every 5s so the page feels live.

import { useMemo } from "react";
import useSWR from "swr";
import {
  swrFetcher,
  type DecisionsResponse,
  type PositionsResponse,
  type StatusResponse,
} from "@/lib/api";
import { cn, fmtMoney, fmtPct, tone } from "@/lib/utils";
import {
  ErrorState,
  PageShell,
  ShimmerBox,
  StaggerItem,
  StaggerList,
} from "@/components/shimmer";
import { SwrProvider } from "@/components/swr-provider";

export default function BrainPage() {
  return (
    <SwrProvider>
      <PageShell className="space-y-6">
        <h1 className="text-2xl font-semibold tracking-tight">Agent Brain</h1>
        <PositionsPanel />
        <FactorPanel />
        <DecisionsFeed />
      </PageShell>
    </SwrProvider>
  );
}

// holdTimeLabel renders a "Since" timestamp as "3d 4h", "5 h 12 m",
// or "—" when zero. Used to show how long a position has been held.
function holdTimeLabel(since: string | undefined): string {
  if (!since) return "—";
  const t = new Date(since).getTime();
  if (!Number.isFinite(t) || t <= 0) return "—";
  const ms = Date.now() - t;
  if (ms < 60_000) return "< 1 m";
  const m = Math.floor(ms / 60_000);
  if (m < 60) return `${m} m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} h ${m % 60} m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24} h`;
}

// Since the brain panel is the primary "where is my money" view,
// the row layout is wide: symbol | qty@entry | mark | change | P&L |
// hold-time | side. Operators want every number on one line.
function PositionsPanel() {
  const { data, error, isLoading } = useSWR<PositionsResponse>(
    "/api/positions",
    swrFetcher,
    { refreshInterval: 5000 },
  );

  if (error) return <ErrorState message={String(error)} />;
  if (isLoading || !data)
    return <ShimmerBox height={120} className="w-full" />;

  if (data.positions.length === 0) {
    return (
      <div className="rounded-xl border border-border bg-panel p-6 text-center text-sm text-muted">
        No open positions. The bot is watching for entries.
      </div>
    );
  }

  return (
    <div className="rounded-xl border border-border bg-panel p-5">
      <div className="mb-4 flex items-baseline justify-between">
        <div className="text-xs uppercase tracking-wide text-muted">
          Open Positions
        </div>
        <div className="text-xs text-muted">
          {data.count} position{data.count === 1 ? "" : "s"} · live marks
        </div>
      </div>
      <StaggerList className="space-y-2">
        {data.positions.map((p) => {
          const pl = Number(p.unrealized_pl);
          const plPct = Number(p.unrealized_pl_pct);
          const chg = Number(p.change_today ?? 0);
          return (
            <StaggerItem
              key={p.symbol}
              className="grid grid-cols-12 items-center gap-2 rounded-lg border border-border bg-bg p-3"
            >
              <div className="col-span-2 flex items-baseline gap-2">
                <span className="font-semibold">{p.symbol}</span>
                <span
                  className={cn(
                    "rounded border px-1.5 py-0.5 text-[10px] uppercase",
                    p.side === "long"
                      ? "border-bull/40 text-bull"
                      : "border-bear/40 text-bear",
                  )}
                >
                  {p.side}
                </span>
              </div>
              <div className="col-span-2 text-right font-mono text-xs text-muted">
                <div>{p.qty} @ {p.avg_entry_price}</div>
                <div className="text-[10px]">entry · {fmtMoney(Number(p.avg_entry_price) * Number(p.qty))}</div>
              </div>
              <div className="col-span-2 text-right font-mono text-xs">
                <div>{p.current_price}</div>
                <div className={cn("text-[10px]", tone(chg))}>
                  today {fmtPct(chg, 2)}
                </div>
              </div>
              <div className="col-span-2 text-right font-mono text-xs">
                {fmtMoney(Number(p.market_value))}
              </div>
              <div
                className={cn(
                  "col-span-2 text-right font-mono text-xs",
                  tone(pl),
                )}
              >
                <div>{fmtMoney(pl)}</div>
                <div className="text-[10px]">{fmtPct(plPct, 2)}</div>
              </div>
              <div className="col-span-2 text-right font-mono text-[10px] text-muted">
                {holdTimeLabel(p.since)}
              </div>
            </StaggerItem>
          );
        })}
      </StaggerList>
    </div>
  );
}

function FactorPanel() {
  const { data, error, isLoading } = useSWR<StatusResponse>(
    "/api/status",
    swrFetcher,
    { refreshInterval: 30000 },
  );

  if (error) return null;
  if (isLoading || !data) return <ShimmerBox height={120} className="w-full" />;

  // The dashboard doesn't know the actual factor breakdown for the
  // current stance — that's only in the most recent decision's
  // factor_scores. Surface a reasonable synthetic blend based on
  // the configured FACTOR_WEIGHTS so the UI isn't empty before any
  // decisions exist.
  const weights = [
    { name: "trend", weight: 0.3 },
    { name: "momentum", weight: 0.25 },
    { name: "volume", weight: 0.2 },
    { name: "volatility", weight: 0.15 },
    { name: "sentiment", weight: 0.1 },
  ];

  return (
    <div className="rounded-xl border border-border bg-panel p-5">
      <div className="mb-4 text-xs uppercase tracking-wide text-muted">
        Stance Weights (FACTOR_WEIGHTS)
      </div>
      <div className="space-y-2">
        {weights.map((f) => (
          <div key={f.name} className="flex items-center gap-2 text-sm">
            <div className="w-28 capitalize text-muted">{f.name}</div>
            <div className="h-3 flex-1 overflow-hidden rounded-full bg-bg">
              <div
                className="h-full bg-accent"
                style={{ width: `${f.weight * 100}%` }}
              />
            </div>
            <div className="w-12 text-right font-mono text-xs">
              {(f.weight * 100).toFixed(0)}%
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

// Side chip — small badge that color-codes buy vs sell so a long
// feed stays scannable.
function SideChip({ side }: { side: string | undefined }) {
  const s = (side ?? "").toLowerCase()
  if (s !== "buy" && s !== "sell") {
    return (
      <span className="rounded border border-border bg-bg px-1.5 py-0.5 text-[10px] uppercase text-muted">
        —
      </span>
    )
  }
  return (
    <span
      className={cn(
        "rounded border px-1.5 py-0.5 text-[10px] uppercase",
        s === "buy"
          ? "border-bull/40 bg-bull/10 text-bull"
          : "border-bear/40 bg-bear/10 text-bear",
      )}
    >
      {s}
    </span>
  )
}

function DecisionsFeed() {
  const { data, error, isLoading } = useSWR<DecisionsResponse>(
    "/api/decisions?limit=20",
    swrFetcher,
    { refreshInterval: 5000 },
  );

  if (error) return <ErrorState message={String(error)} />;
  if (isLoading || !data)
    return (
      <div className="space-y-2">
        {Array.from({ length: 5 }).map((_, i) => (
          <ShimmerBox key={i} height={28} />
        ))}
      </div>
    );

  if (data.decisions.length === 0) {
    return (
      <div className="rounded-xl border border-border bg-panel p-6 text-center text-sm text-muted">
        No decisions yet. The first tick will populate this feed.
      </div>
    );
  }

  return (
    <div className="rounded-xl border border-border bg-panel p-5">
      <div className="mb-4 flex items-baseline justify-between">
        <div className="text-xs uppercase tracking-wide text-muted">
          Recent Decisions
        </div>
        <div className="text-xs text-muted">newest first · 5 s refresh</div>
      </div>
      <div className="mb-2 grid grid-cols-12 gap-2 border-b border-border pb-1 text-[10px] uppercase tracking-wide text-muted">
        <div className="col-span-2">Time</div>
        <div className="col-span-1">Side</div>
        <div className="col-span-2">Symbol</div>
        <div className="col-span-1 text-right">Conf.</div>
        <div className="col-span-1 text-center">Verdict</div>
        <div className="col-span-2">Source</div>
        <div className="col-span-3">Detail</div>
      </div>
      <ul className="divide-y divide-border">
        {data.decisions.map((d, i) => {
          const ts = d.ts ? new Date(d.ts) : null;
          const time = ts && Number.isFinite(ts.getTime())
            ? ts.toISOString().slice(5, 19).replace("T", " ")
            : "—";
          return (
            <li
              key={d.ts + d.symbol + d.source + i}
              className="grid grid-cols-12 items-center gap-2 py-1.5 text-sm"
            >
              <div className="col-span-2 font-mono text-xs text-muted">{time}</div>
              <div className="col-span-1">
                <SideChip side={d.side} />
              </div>
              <div className="col-span-2 font-medium">
                {d.symbol || <span className="text-muted">—</span>}
              </div>
              <div className="col-span-1 text-right font-mono text-xs">
                {d.confidence !== undefined && d.confidence !== null
                  ? (d.confidence * 100).toFixed(0) + "%"
                  : "—"}
              </div>
              <div className="col-span-1 text-center">
                <span
                  className={cn(
                    "rounded border px-1.5 py-0.5 text-[10px] uppercase",
                    d.risk === "APPROVED"
                      ? "border-bull/40 bg-bull/10 text-bull"
                      : d.risk === "REJECTED" || d.risk === "HALT_TRADING"
                        ? "border-danger/40 bg-danger/10 text-danger"
                        : "border-border bg-bg text-muted",
                  )}
                >
                  {d.risk || "INFO"}
                </span>
              </div>
              <div className="col-span-2 truncate text-xs text-muted">{d.source}</div>
              <div className="col-span-3 truncate text-xs">{d.detail || ""}</div>
            </li>
          )
        })}
      </ul>
    </div>
  );
}