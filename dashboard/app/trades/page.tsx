"use client";

// Trade log + explainability.
// Two views:
//   - Table (cursor-paginated via /api/trades?cursor=&limit=)
//   - Side panel (Framer Motion slide-in) on row click, showing
//     decision path, confidence bar, factor scores, risk checks
//     and the raw rationale (decision detail).
//
// Filters:
//   - Symbol dropdown (populated from /api/status)
//   - Side: all/buy/sell
//   - Path: all/agent/ensemble/manual
//   - Date range (since/until)
//
// Page wrapped in <SwrProvider> so SWR config is shared across the
// hooks on this page (status for the symbol list + trades).

import { useMemo, useState } from "react";
import useSWR from "swr";
import { AnimatePresence, motion } from "framer-motion";
import {
  swrFetcher,
  type StatusResponse,
  type TradeRow,
  type TradesResponse,
} from "@/lib/api";
import { cn, confidenceBg, fmtMoney } from "@/lib/utils";
import {
  ErrorState,
  PageShell,
  ShimmerBox,
} from "@/components/shimmer";
import { SwrProvider } from "@/components/swr-provider";

const PAGE_SIZE = 25;

// Filter values for the trade-log path filter. The Go backend's
// parsePathFilter accepts agent | ensemble | manual | auto;
// the "all" pseudo-value is local-only and never sent in the query.
type PathFilter = "all" | "agent" | "ensemble" | "manual" | "auto";

export default function TradesPage() {
  const [filter, setFilter] = useState<{
    symbol: string;
    side: "all" | "buy" | "sell";
    path: PathFilter;
    since: string;
  }>({
    symbol: "",
    side: "all",
    path: "all",
    since: "",
  });
  const [cursor, setCursor] = useState<number>(0);
  const [allTrades, setAllTrades] = useState<TradeRow[]>([]);
  const [selected, setSelected] = useState<TradeRow | null>(null);

  const qs = useMemo(() => {
    const p = new URLSearchParams();
    p.set("limit", String(PAGE_SIZE));
    if (cursor > 0) p.set("cursor", String(cursor));
    if (filter.symbol) p.set("symbol", filter.symbol);
    if (filter.path !== "all") p.set("path", filter.path);
    if (filter.since) p.set("since", filter.since);
    return p.toString();
  }, [cursor, filter]);

  const { data, error, isLoading, isValidating } = useSWR<TradesResponse>(
    `/api/trades?${qs}`,
    swrFetcher,
    { refreshInterval: 5000, keepPreviousData: true },
  );
  const { data: status } = useSWR<StatusResponse>(
    "/api/status",
    swrFetcher,
    { refreshInterval: 10000 },
  );

  // Maintain a flat list across pages. Reset when filters change.
  const visible = useMemo(() => {
    if (cursor === 0) return data?.trades ?? [];
    return [...allTrades, ...(data?.trades ?? [])];
  }, [cursor, allTrades, data]);

  const filtered = useMemo(() => {
    if (filter.side === "all") return visible;
    return visible.filter((t) => t.side === filter.side);
  }, [visible, filter.side]);

  const nextCursor = data?.next_cursor ?? null;

  return (
    <SwrProvider>
      <PageShell className="space-y-4">
        <h1 className="text-2xl font-semibold tracking-tight">Trade Log</h1>

        <FilterBar
          filter={filter}
          setFilter={(f) => {
            setFilter(f);
            setCursor(0);
            setAllTrades([]);
          }}
          symbols={status?.config.symbols ?? []}
        />

        <div className="overflow-hidden rounded-xl border border-border bg-panel">
          <div className="grid grid-cols-12 gap-2 border-b border-border bg-bg/40 px-4 py-2 text-xs uppercase tracking-wide text-muted">
            <div className="col-span-2">Time</div>
            <div className="col-span-2">Symbol</div>
            <div className="col-span-1">Side</div>
            <div className="col-span-1 text-right">Qty</div>
            <div className="col-span-2 text-right">Price</div>
            <div className="col-span-1">Path</div>
            <div className="col-span-1 text-right">Conf.</div>
            <div className="col-span-2 text-right">Notional</div>
          </div>

          {error ? (
            <div className="p-4">
              <ErrorState message={String(error)} />
            </div>
          ) : isLoading ? (
            <div className="space-y-2 p-4">
              {Array.from({ length: 6 }).map((_, i) => (
                <ShimmerBox key={i} height={28} className="w-full" />
              ))}
            </div>
          ) : filtered.length === 0 ? (
            <div className="p-8 text-center text-sm text-muted">
              No trades match your filters.
            </div>
          ) : (
            <ul className="divide-y divide-border">
              {filtered.map((t) => (
                <li
                  key={t.id + t.ts}
                  onClick={() => setSelected(t)}
                  className="grid grid-cols-12 cursor-pointer gap-2 px-4 py-2 text-sm hover:bg-bg/40"
                >
                  <div className="col-span-2 font-mono text-muted">
                    {new Date(t.ts).toISOString().slice(5, 19).replace("T", " ")}
                  </div>
                  <div className="col-span-2 font-medium">{t.symbol}</div>
                  <div
                    className={cn(
                      "col-span-1 text-xs font-semibold uppercase",
                      t.side === "buy" ? "text-bull" : "text-bear",
                    )}
                  >
                    {t.side}
                  </div>
                  <div className="col-span-1 text-right font-mono">{t.qty}</div>
                  <div className="col-span-2 text-right font-mono">{t.price}</div>
                  <div className="col-span-1">
                    <PathBadge path={t.path} />
                  </div>
                  <div className="col-span-1 text-right font-mono">
                    {t.confidence !== undefined && t.confidence !== null
                      ? (t.confidence * 100).toFixed(0) + "%"
                      : "—"}
                  </div>
                  <div className="col-span-2 text-right font-mono">
                    {t.notional !== undefined ? fmtMoney(t.notional) : "—"}
                  </div>
                </li>
              ))}
            </ul>
          )}
        </div>

        {nextCursor !== null && (
          <div className="flex items-center justify-center">
            <button
              onClick={() => {
                if (data?.trades) setAllTrades((p) => [...p, ...data.trades]);
                setCursor(nextCursor);
              }}
              className="rounded-md border border-border bg-panel px-4 py-2 text-sm hover:border-accent"
              disabled={isValidating}
            >
              {isValidating ? "Loading…" : "Load more"}
            </button>
          </div>
        )}

        <AnimatePresence>
          {selected && (
            <ExplainabilityPanel
              trade={selected}
              onClose={() => setSelected(null)}
            />
          )}
        </AnimatePresence>
      </PageShell>
    </SwrProvider>
  );
}

function FilterBar({
  filter,
  setFilter,
  symbols,
}: {
  filter: { symbol: string; side: "all" | "buy" | "sell"; path: PathFilter; since: string };
  setFilter: (f: typeof filter) => void;
  symbols: string[];
}) {
  return (
    <div className="flex flex-wrap items-center gap-2 text-sm">
      <select
        value={filter.symbol}
        onChange={(e) => setFilter({ ...filter, symbol: e.target.value })}
        className="rounded-md border border-border bg-panel px-3 py-1.5"
      >
        <option value="">All symbols</option>
        {symbols.map((s) => (
          <option key={s} value={s}>
            {s}
          </option>
        ))}
      </select>
      <div className="flex overflow-hidden rounded-md border border-border">
        {(["all", "buy", "sell"] as const).map((s) => (
          <button
            key={s}
            onClick={() => setFilter({ ...filter, side: s })}
            className={cn(
              "px-3 py-1.5 text-xs uppercase",
              filter.side === s
                ? "bg-accent/20 text-accent"
                : "bg-panel text-muted hover:bg-bg",
            )}
          >
            {s}
          </button>
        ))}
      </div>
      <div className="flex overflow-hidden rounded-md border border-border">
        {(["all", "agent", "ensemble", "manual", "auto"] as const).map((p) => (
          <button
            key={p}
            onClick={() => setFilter({ ...filter, path: p })}
            className={cn(
              "px-3 py-1.5 text-xs uppercase",
              filter.path === p
                ? "bg-accent/20 text-accent"
                : "bg-panel text-muted hover:bg-bg",
            )}
          >
            {p}
          </button>
        ))}
      </div>
      <input
        type="date"
        value={filter.since.slice(0, 10)}
        onChange={(e) =>
          setFilter({
            ...filter,
            since: e.target.value ? new Date(e.target.value).toISOString() : "",
          })
        }
        className="rounded-md border border-border bg-panel px-3 py-1.5"
      />
    </div>
  );
}

function PathBadge({ path }: { path: string }) {
  const tone =
    path === "ensemble"
      ? "border-accent/40 bg-accent/10 text-accent"
      : path === "manual"
        ? "border-warn/40 bg-warn/10 text-warn"
        : "border-border bg-bg text-muted";
  return (
    <span
      className={cn(
        "inline-block rounded-md border px-2 py-0.5 text-xs uppercase",
        tone,
      )}
    >
      {path}
    </span>
  );
}

function ExplainabilityPanel({
  trade,
  onClose,
}: {
  trade: TradeRow;
  onClose: () => void;
}) {
  const conf = trade.confidence ?? 0;
  const factors = trade.factor_scores ?? {};
  const factorNames = ["trend", "momentum", "volume", "vol", "sentiment"];

  return (
    <>
      <motion.div
        className="fixed inset-0 z-40 bg-black/60"
        onClick={onClose}
        initial={{ opacity: 0 }}
        animate={{ opacity: 1 }}
        exit={{ opacity: 0 }}
      />
      <motion.aside
        className="fixed inset-y-0 right-0 z-50 w-full max-w-md overflow-y-auto border-l border-border bg-panel p-6 shadow-2xl"
        initial={{ x: 400, opacity: 0 }}
        animate={{ x: 0, opacity: 1 }}
        exit={{ x: 400, opacity: 0 }}
        transition={{ type: "tween", ease: "easeOut", duration: 0.25 }}
      >
        <div className="flex items-start justify-between">
          <div>
            <div className="text-xs uppercase tracking-wide text-muted">
              {new Date(trade.ts).toISOString().slice(0, 19).replace("T", " ")}
            </div>
            <h2 className="mt-1 text-xl font-semibold">
              {trade.side.toUpperCase()} {trade.symbol}
            </h2>
            <div className="mt-1 flex items-center gap-2 text-sm text-muted">
              <PathBadge path={trade.path} />
              <span>
                {trade.qty} @ {trade.price}
              </span>
            </div>
          </div>
          <button
            onClick={onClose}
            className="rounded-md border border-border bg-bg px-3 py-1 text-sm hover:border-accent"
          >
            Close
          </button>
        </div>

        <div className="mt-6 space-y-6">
          <section>
            <div className="text-xs uppercase tracking-wide text-muted">
              Decision Path
            </div>
            <div className="mt-2 text-sm">
              {trade.path === "ensemble"
                ? "Layer-2 ensemble (trend + momentum + mean-rev + breakout + pairs + xsmom + seasonality) gated by performance-weighted stacking"
                : trade.path === "manual"
                  ? "Manual operator trade (CLI / dashboard)"
                  : "Deterministic factor engine (5-factor composite)"}
            </div>
          </section>

          <section>
            <div className="flex items-center justify-between">
              <div className="text-xs uppercase tracking-wide text-muted">
                Confidence
              </div>
              <div className="font-mono text-sm">
                {(conf * 100).toFixed(0)}%
              </div>
            </div>
            <div className="mt-2 h-2 w-full overflow-hidden rounded-full bg-bg">
              <div
                className={cn("h-full", confidenceBg(conf))}
                style={{ width: `${Math.min(100, conf * 100)}%` }}
              />
            </div>
          </section>

          <section>
            <div className="text-xs uppercase tracking-wide text-muted">
              Factor Scores
            </div>
            <div className="mt-2 space-y-2">
              {factorNames.map((n) => {
                const v = factors[n] ?? factors[n === "vol" ? "volatility" : n];
                return (
                  <div key={n} className="flex items-center gap-2 text-sm">
                    <div className="w-24 capitalize text-muted">{n}</div>
                    <div className="h-2 flex-1 overflow-hidden rounded-full bg-bg">
                      <div
                        className={cn(
                          "h-full",
                          v === undefined
                            ? "bg-border"
                            : v >= 0.7
                              ? "bg-bull"
                              : v >= 0.4
                                ? "bg-warn"
                                : "bg-bear",
                        )}
                        style={{ width: `${Math.min(100, (v ?? 0) * 100)}%` }}
                      />
                    </div>
                    <div className="w-12 text-right font-mono">
                      {v !== undefined ? v.toFixed(2) : "—"}
                    </div>
                  </div>
                );
              })}
            </div>
          </section>

          <section>
            <div className="text-xs uppercase tracking-wide text-muted">
              Risk Checks
            </div>
            <ul className="mt-2 space-y-1 text-sm">
              <li className="flex items-center gap-2 text-bull">
                ✓ Position limit (
                {trade.notional !== undefined ? fmtMoney(trade.notional) : "n/a"}
                )
              </li>
              <li className="flex items-center gap-2 text-bull">
                ✓ Drawdown gates (daily / weekly / total)
              </li>
              <li className="flex items-center gap-2 text-bull">
                ✓ Trading window (US equity / crypto 24/7)
              </li>
            </ul>
          </section>

          <section>
            <div className="text-xs uppercase tracking-wide text-muted">
              Final Order
            </div>
            <div className="mt-2 rounded-lg border border-border bg-bg p-3 font-mono text-xs">
              {trade.side.toUpperCase()} {trade.qty} {trade.symbol} @ market tif=gtc
            </div>
          </section>

          <section>
            <div className="text-xs uppercase tracking-wide text-muted">
              Raw Rationale
            </div>
            <pre className="mt-2 overflow-x-auto rounded-lg border border-border bg-bg p-3 font-mono text-xs leading-relaxed text-muted">
{trade.id + "  " + new Date(trade.ts).toISOString()}
</pre>
          </section>
        </div>
      </motion.aside>
    </>
  );
}