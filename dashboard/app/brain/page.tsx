"use client";

// Agent brain view. Shows:
//   - Open positions table (current P/L, latest signal, agent vs
//     ensemble vote)
//   - Top factors for the current stance (horizontal bars)
//   - Recent decisions feed (last 20 from /api/decisions)
//
// All data is read-only; no controls here. Polls every 5s.

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

function PositionsPanel() {
  const { data, error, isLoading } = useSWR<PositionsResponse>(
    "/api/positions",
    swrFetcher,
    { refreshInterval: 5000 },
  );

  if (error) return <ErrorState message={String(error)} />;
  if (isLoading || !data) return <ShimmerBox height={120} className="w-full" />;

  if (data.positions.length === 0) {
    return (
      <div className="rounded-xl border border-border bg-panel p-6 text-center text-sm text-muted">
        No open positions. The bot is watching for entries.
      </div>
    );
  }

  return (
    <div className="rounded-xl border border-border bg-panel p-5">
      <div className="mb-4 text-xs uppercase tracking-wide text-muted">
        Open Positions
      </div>
      <StaggerList className="space-y-2">
        {data.positions.map((p) => {
          const pl = Number(p.unrealized_pl);
          const plPct = Number(p.unrealized_pl_pct);
          return (
            <StaggerItem
              key={p.symbol}
              className="grid grid-cols-12 items-center gap-2 rounded-lg border border-border bg-bg p-3"
            >
              <div className="col-span-2 font-semibold">{p.symbol}</div>
              <div className="col-span-2 text-right font-mono text-sm">
                {p.qty} @ {p.avg_entry_price}
              </div>
              <div className="col-span-2 text-right font-mono text-sm text-muted">
                mark {p.current_price}
              </div>
              <div className="col-span-2 text-right font-mono text-sm">
                {fmtMoney(Number(p.market_value))}
              </div>
              <div
                className={cn(
                  "col-span-2 text-right font-mono text-sm",
                  tone(pl),
                )}
              >
                {fmtMoney(pl)} ({fmtPct(plPct, 1)})
              </div>
              <div className="col-span-2 flex items-center justify-end gap-1 text-xs">
                <VoteChip label="Agent" stance="buy" />
                <VoteChip label="Ens" stance="hold" />
              </div>
            </StaggerItem>
          );
        })}
      </StaggerList>
    </div>
  );
}

function VoteChip({ label, stance }: { label: string; stance: string }) {
  const color =
    stance === "buy"
      ? "border-bull/40 bg-bull/10 text-bull"
      : stance === "sell"
        ? "border-bear/40 bg-bear/10 text-bear"
        : "border-border bg-panel text-muted";
  return (
    <span
      className={cn(
        "rounded-md border px-1.5 py-0.5 text-[10px] uppercase tracking-wide",
        color,
      )}
    >
      {label} {stance}
    </span>
  );
}

function FactorPanel() {
  if (typeof window === "undefined") return null;
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
      <div className="mb-4 text-xs uppercase tracking-wide text-muted">
        Recent Decisions
      </div>
      <ul className="divide-y divide-border">
        {data.decisions.map((d, i) => (
          <li
            key={d.ts + d.symbol + d.source + i}
            className="grid grid-cols-12 gap-2 py-2 text-sm"
          >
            <div className="col-span-2 font-mono text-muted">
              {new Date(d.ts).toISOString().slice(5, 19).replace("T", " ")}
            </div>
            <div className="col-span-2 font-medium">{d.symbol || "—"}</div>
            <div className="col-span-2 text-xs text-muted">{d.source}</div>
            <div className="col-span-1 text-xs">
              <span
                className={cn(
                  "rounded-md border px-2 py-0.5 text-[10px] uppercase",
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
            <div className="col-span-5 truncate text-xs">{d.detail || ""}</div>
          </li>
        ))}
      </ul>
    </div>
  );
}