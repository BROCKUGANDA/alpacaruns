"use client";

// Live status & P&L view. Polls:
//   - /api/status every 5s for state + kill switches
//   - /api/account every 5s for equity / cash / day P/L
//   - /api/pnl every 30s for the equity curve (slower to keep the
//     line drawing cheap)
//   - /api/positions every 5s for the open positions count
//
// Renders:
//   - 4 stat tiles (current equity, day P/L, total P/L, open positions)
//   - Equity curve (Recharts area) with 24h/7d/all toggle
//   - Kill-switch badges (daily / weekly / total)
//   - Live badge with pulsing dot
//   - Auto-refresh via SWR.
//
// Wrapped in <SwrProvider> so SWR config (refresh interval, dedup,
// keepPreviousData) is shared across all hooks on this page.

import { useMemo, useState } from "react";
import useSWR from "swr";
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  swrFetcher,
  type AccountResponse,
  type PnLResponse,
  type PositionsResponse,
  type StatusResponse,
} from "@/lib/api";
import { fmtMoney, fmtPct, tone } from "@/lib/utils";
import {
  ErrorState,
  PageShell,
  ShimmerBox,
  StaggerItem,
  StaggerList,
} from "@/components/shimmer";
import { SwrProvider } from "@/components/swr-provider";

type Range = "24h" | "7d" | "all";

export default function HomePage() {
  return (
    <SwrProvider>
      <PageShell className="space-y-6">
        <LiveBadge />
        <HeaderRow />
        <StatTiles />
        <EquityCurve />
      </PageShell>
    </SwrProvider>
  );
}

// LiveBadge — pulsing green dot + "Live" label + last-poll time.
function LiveBadge() {
  const { data, error } = useSWR<StatusResponse>(
    "/api/status",
    swrFetcher,
    { refreshInterval: 5000 },
  );
  const alive = !error && data?.bot !== "halted";
  return (
    <div className="flex items-center gap-3">
      <span className="inline-flex items-center gap-2 rounded-full border border-accent/30 bg-accent/10 px-3 py-1 text-xs">
        <span className={alive ? "live-dot" : "h-2 w-2 rounded-full bg-danger"} />
        <span className={alive ? "text-accent" : "text-danger"}>
          {alive ? "Live" : "Down"}
        </span>
      </span>
      <span className="text-xs text-muted">
        Last poll:{" "}
        {data?.last_tick
          ? new Date(data.last_tick).toISOString().replace("T", " ").slice(0, 19)
          : "—"}
      </span>
      <span className="text-xs text-muted">Tick #{data?.tick_number ?? "—"}</span>
    </div>
  );
}

// HeaderRow — page title + kill-switch badges.
function HeaderRow() {
  const { data, error, isLoading } = useSWR<StatusResponse>(
    "/api/status",
    swrFetcher,
    { refreshInterval: 5000 },
  );

  if (error) return <ErrorState message={String(error)} />;
  if (isLoading || !data)
    return (
      <div className="flex items-center justify-between">
        <ShimmerBox height={32} className="w-64" />
        <ShimmerBox height={28} className="w-72" />
      </div>
    );

  return (
    <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">
          Live Status &amp; P&amp;L
        </h1>
        <p className="text-sm text-muted">
          Autonomous paper-trading bot · {data.config.symbols.length} symbols ·{" "}
          {data.config.llm_provider}
        </p>
      </div>
      <KillSwitchBadges ks={data.kill_switch} />
    </div>
  );
}

function KillSwitchBadges({ ks }: { ks: StatusResponse["kill_switch"] }) {
  return (
    <div className="flex items-center gap-2 text-xs">
      {(["daily", "weekly", "total"] as const).map((k) => (
        <span
          key={k}
          className={
            "rounded-md border px-2 py-1 uppercase tracking-wide " +
            (ks[k]
              ? "border-danger/40 bg-danger/10 text-danger"
              : "border-border bg-panel text-muted")
          }
        >
          {k} {ks[k] ? "halted" : "ok"}
        </span>
      ))}
    </div>
  );
}

function StatTiles() {
  const { data: status } = useSWR<StatusResponse>(
    "/api/status",
    swrFetcher,
    { refreshInterval: 5000 },
  );
  const { data: acct } = useSWR<AccountResponse>(
    "/api/account",
    swrFetcher,
    { refreshInterval: 5000 },
  );
  const { data: pos } = useSWR<PositionsResponse>(
    "/api/positions",
    swrFetcher,
    { refreshInterval: 5000 },
  );
  const { data: pnl } = useSWR<PnLResponse>(
    "/api/pnl",
    swrFetcher,
    { refreshInterval: 30000 },
  );

  const equity = Number(acct?.equity ?? 0);
  const dayPnL = Number(acct?.day_pnl ?? 0);
  const totalPnL = pnl?.summary?.total_pnl ?? 0;
  const openCount = pos?.count ?? 0;
  const start = pnl?.summary?.starting_equity ?? 100000;
  const equityDelta = equity > 0 ? equity - start : 0;

  const tiles = [
    {
      label: "Current Equity",
      value: fmtMoney(equity),
      sub: (
        <span className={tone(equityDelta)}>
          {equityDelta >= 0 ? "+" : ""}
          {fmtMoney(equityDelta)} vs $100k
        </span>
      ),
    },
    {
      label: "Day P&L",
      value: fmtMoney(dayPnL),
      sub: (
        <span className={tone(dayPnL)}>
          {fmtPct(equity > 0 ? dayPnL / equity : 0)}
        </span>
      ),
    },
    {
      label: "Total P&L",
      value: fmtMoney(totalPnL),
      sub: (
        <span className={tone(totalPnL)}>
          {fmtPct(start > 0 ? totalPnL / start : 0)}
        </span>
      ),
    },
    {
      label: "Open Positions",
      value: String(openCount),
      sub: (
        <span className="text-muted">
          {status?.config.symbols.length ?? 0} symbols scored
        </span>
      ),
    },
  ];

  return (
    <StaggerList className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
      {tiles.map((t) => (
        <StaggerItem
          key={t.label}
          className="rounded-xl border border-border bg-panel p-5"
        >
          <div className="text-xs uppercase tracking-wide text-muted">
            {t.label}
          </div>
          <div className="mt-2 text-2xl font-semibold">{t.value}</div>
          <div className="mt-1 text-sm">{t.sub}</div>
        </StaggerItem>
      ))}
    </StaggerList>
  );
}

function EquityCurve() {
  const [range, setRange] = useState<Range>("7d");
  const { data, error, isLoading } = useSWR<PnLResponse>(
    "/api/pnl",
    swrFetcher,
    { refreshInterval: 30000 },
  );

  const filtered = useMemo(() => {
    if (!data?.snapshots) return [];
    const now = Date.now();
    const cutoff =
      range === "24h"
        ? now - 24 * 3600 * 1000
        : range === "7d"
          ? now - 7 * 24 * 3600 * 1000
          : 0;
    return data.snapshots
      .filter((s) => new Date(s.t).getTime() >= cutoff)
      .map((s) => ({
        t: new Date(s.t).toISOString().slice(5, 16).replace("T", " "),
        equity: s.equity,
      }));
  }, [data, range]);

  return (
    <div className="rounded-xl border border-border bg-panel p-5">
      <div className="mb-4 flex items-center justify-between">
        <div>
          <div className="text-xs uppercase tracking-wide text-muted">
            Equity Curve
          </div>
          <div className="mt-1 text-sm">
            Sharpe{" "}
            <span className="font-mono">
              {data ? data.summary.sharpe.toFixed(2) : "—"}
            </span>
            {"  ·  "}
            Max DD{" "}
            <span className="font-mono">
              {data ? fmtPct(data.summary.max_drawdown) : "—"}
            </span>
            {"  ·  "}
            Win rate{" "}
            <span className="font-mono">
              {data ? fmtPct(data.summary.win_rate, 1) : "—"}
            </span>
          </div>
        </div>
        <div className="flex items-center gap-1 text-xs">
          {(["24h", "7d", "all"] as Range[]).map((r) => (
            <button
              key={r}
              onClick={() => setRange(r)}
              className={
                "rounded-md border px-2 py-1 " +
                (range === r
                  ? "border-accent/50 bg-accent/10 text-accent"
                  : "border-border bg-bg text-muted hover:border-accent/40")
              }
            >
              {r}
            </button>
          ))}
        </div>
      </div>

      <div className="h-72">
        {error ? (
          <ErrorState message={String(error)} />
        ) : isLoading || !data ? (
          <div className="flex h-full items-center justify-center">
            <ShimmerBox height={200} className="w-full" />
          </div>
        ) : filtered.length === 0 ? (
          <div className="flex h-full items-center justify-center text-sm text-muted">
            No trades yet — the bot will start filling the curve on its next tick.
          </div>
        ) : (
          <ResponsiveContainer width="100%" height="100%">
            <AreaChart data={filtered}>
              <defs>
                <linearGradient id="equityFill" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stopColor="#10b981" stopOpacity={0.4} />
                  <stop offset="100%" stopColor="#10b981" stopOpacity={0} />
                </linearGradient>
              </defs>
              <CartesianGrid stroke="#1f2738" strokeDasharray="3 3" />
              <XAxis dataKey="t" stroke="#9ca3af" fontSize={11} />
              <YAxis
                stroke="#9ca3af"
                fontSize={11}
                tickFormatter={(v: number) =>
                  v >= 1000 ? `$${(v / 1000).toFixed(0)}k` : `$${v.toFixed(0)}`
                }
                width={64}
              />
              <Tooltip
                contentStyle={{
                  background: "#0f1422",
                  border: "1px solid #1f2738",
                  borderRadius: 8,
                  fontSize: 12,
                }}
                formatter={(v: number) => [fmtMoney(v), "Equity"]}
              />
              <Area
                type="monotone"
                dataKey="equity"
                stroke="#10b981"
                fill="url(#equityFill)"
                strokeWidth={2}
              />
            </AreaChart>
          </ResponsiveContainer>
        )}
      </div>
    </div>
  );
}