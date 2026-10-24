"use client";

// Signal picker + agent bridge view.
//
// The picker panel lists the classified universe (equities, ETFs,
// index trackers, options, crypto) and the ranked signals the factor
// engine produced for it. The agents panel shows each peer agent's
// position on the on/off ramp and lets the operator move it.
//
// Read paths poll every 10s; the ramp buttons are one-shot writes.

import useSWR from "swr";
import {
  apiFetch,
  swrFetcher,
  type A2AAgentState,
  type A2AAgentsResponse,
  type InstrumentsResponse,
  type SignalsResponse,
} from "@/lib/api";
import { cn, fmtMoney, tone } from "@/lib/utils";
import {
  ErrorState,
  PageShell,
  ShimmerBox,
  StaggerItem,
  StaggerList,
} from "@/components/shimmer";
import { SwrProvider } from "@/components/swr-provider";

export default function PickerPage() {
  return (
    <SwrProvider>
      <PageShell className="space-y-6">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Signal Picker</h1>
          <p className="mt-1 text-sm text-muted">
            One universe across equities, ETFs, index trackers, options and
            crypto — classified, scored and ranked by the same factor engine
            that gates every order.
          </p>
        </div>
        <SignalsPanel />
        <UniversePanel />
        <AgentsPanel />
      </PageShell>
    </SwrProvider>
  );
}

const CLASS_LABEL: Record<string, string> = {
  equity: "Equity",
  etf: "ETF",
  index: "Index",
  option: "Option",
  crypto: "Crypto",
};

function signalBadge(sig: string) {
  const cls =
    sig === "buy"
      ? "bg-accent/15 text-accent border-accent/40"
      : sig === "sell"
        ? "bg-danger/20 text-danger border-danger/40"
        : "bg-panel text-muted border-border";
  return (
    <span className={cn("rounded-full border px-2 py-0.5 text-xs uppercase", cls)}>
      {sig}
    </span>
  );
}

// SignalsPanel ranks every instrument by its factor composite.
function SignalsPanel() {
  const { data, error, isLoading } = useSWR<SignalsResponse>(
    "/api/signals",
    swrFetcher,
    { refreshInterval: 10000 },
  );

  if (error) {
    return (
      <section className="space-y-3">
        <h2 className="text-lg font-medium">Ranked signals</h2>
        <ErrorState message={String(error)} />
      </section>
    );
  }
  if (isLoading || !data) {
    return (
      <section className="space-y-3">
        <h2 className="text-lg font-medium">Ranked signals</h2>
        <ShimmerBox height={160} className="w-full" />
      </section>
    );
  }

  return (
    <section className="space-y-3">
      <div className="flex items-baseline justify-between">
        <h2 className="text-lg font-medium">Ranked signals</h2>
        <span className="text-xs text-muted">
          {data.scored}/{data.scanned} scored
        </span>
      </div>
      {data.signals.length === 0 ? (
        <div className="rounded-xl border border-border bg-panel p-6 text-center text-sm text-muted">
          No instruments configured. Add symbols to STRATEGY_EQUITY_SYMBOLS,
          STRATEGY_ETF_SYMBOLS, STRATEGY_INDEX_SYMBOLS or
          STRATEGY_CRYPTO_SYMBOLS.
        </div>
      ) : (
        <div className="overflow-x-auto rounded-xl border border-border bg-panel">
          <table className="w-full text-sm">
            <thead className="text-left text-xs uppercase tracking-wide text-muted">
              <tr>
                <th className="px-4 py-3">Symbol</th>
                <th className="px-4 py-3">Class</th>
                <th className="px-4 py-3 text-right">Composite</th>
                <th className="px-4 py-3 text-right">Price</th>
                <th className="px-4 py-3">Signal</th>
                <th className="px-4 py-3">Why</th>
              </tr>
            </thead>
            <tbody>
              {data.signals.map((c) => (
                <tr key={c.instrument.symbol} className="border-t border-border">
                  <td className="px-4 py-3 font-medium">{c.instrument.symbol}</td>
                  <td className="px-4 py-3 text-muted">
                    {CLASS_LABEL[c.instrument.class] ?? c.instrument.class}
                  </td>
                  <td
                    className={cn(
                      "px-4 py-3 text-right tabular-nums",
                      tone(c.composite - 0.5),
                    )}
                  >
                    {c.error ? "—" : c.composite.toFixed(2)}
                  </td>
                  <td className="px-4 py-3 text-right tabular-nums">
                    {c.price > 0 ? fmtMoney(c.price) : "—"}
                  </td>
                  <td className="px-4 py-3">{signalBadge(c.error ? "hold" : c.signal)}</td>
                  <td className="px-4 py-3 text-xs text-muted">
                    {c.error ?? c.reason}
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

// UniversePanel lists every classified instrument with its venue.
function UniversePanel() {
  const { data, error, isLoading } = useSWR<InstrumentsResponse>(
    "/api/instruments",
    swrFetcher,
    { refreshInterval: 30000 },
  );

  if (error) {
    return (
      <section className="space-y-3">
        <h2 className="text-lg font-medium">Universe</h2>
        <ErrorState message={String(error)} />
      </section>
    );
  }
  if (isLoading || !data) {
    return (
      <section className="space-y-3">
        <h2 className="text-lg font-medium">Universe</h2>
        <ShimmerBox height={120} className="w-full" />
      </section>
    );
  }

  const counts = Object.entries(data.classes);
  return (
    <section className="space-y-3">
      <h2 className="text-lg font-medium">Universe</h2>
      <div className="flex flex-wrap gap-2">
        {counts.map(([cls, n]) => (
          <span
            key={cls}
            className="rounded-full border border-border bg-panel px-3 py-1 text-xs text-muted"
          >
            {CLASS_LABEL[cls] ?? cls}: <span className="text-fg">{n}</span>
          </span>
        ))}
        {counts.length === 0 && (
          <span className="text-sm text-muted">No instruments configured.</span>
        )}
      </div>
      <StaggerList>
        {data.instruments.map((i) => (
          <StaggerItem key={i.symbol}>
            <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-panel px-4 py-3">
              <div>
                <div className="font-medium">{i.symbol}</div>
                <div className="text-xs text-muted">
                  {CLASS_LABEL[i.class] ?? i.class}
                  {i.exchange ? ` · ${i.exchange}` : ""}
                  {i.window ? ` · ${i.window} window` : ""}
                  {i.delta ? ` · Δ ${i.delta.toFixed(2)}` : ""}
                </div>
              </div>
              <div className="text-xs text-muted">
                {i.tradable ? "tradable" : "not tradable"}
                {i.fractional ? " · fractional" : ""}
                {i.shortable ? " · shortable" : ""}
              </div>
            </div>
          </StaggerItem>
        ))}
      </StaggerList>
    </section>
  );
}

// RAMP_MODES are the positions an agent can be moved to.
const RAMP_MODES = ["off", "shadow", "on"] as const;

function modeBadge(mode: string) {
  const cls =
    mode === "on"
      ? "bg-danger/20 text-danger border-danger/40"
      : mode === "shadow"
        ? "bg-warn/20 text-warn border-warn/40"
        : "bg-panel text-muted border-border";
  return (
    <span className={cn("rounded-full border px-2 py-0.5 text-xs uppercase", cls)}>
      {mode}
    </span>
  );
}

// AgentsPanel shows the agent-to-agent bridge: who may post, where each
// agent sits on the ramp, and the buttons that move it.
function AgentsPanel() {
  const { data, error, isLoading, mutate } = useSWR<A2AAgentsResponse>(
    "/api/a2a/agents",
    swrFetcher,
    { refreshInterval: 10000 },
  );

  if (error) {
    return (
      <section className="space-y-3">
        <h2 className="text-lg font-medium">Agent bridge</h2>
        <ErrorState message={String(error)} />
      </section>
    );
  }
  if (isLoading || !data) {
    return (
      <section className="space-y-3">
        <h2 className="text-lg font-medium">Agent bridge</h2>
        <ShimmerBox height={100} className="w-full" />
      </section>
    );
  }

  const ramp = async (agent: string, mode: string) => {
    await apiFetch(`/api/a2a/agents/${encodeURIComponent(agent)}/ramp`, {
      method: "POST",
      body: JSON.stringify({ mode }),
    });
    await mutate();
  };

  return (
    <section className="space-y-3">
      <div className="flex items-baseline justify-between">
        <h2 className="text-lg font-medium">Agent bridge</h2>
        <span className="text-xs text-muted">
          off → shadow → on. Ramping on needs earned shadow passes.
        </span>
      </div>
      {data.agents.length === 0 ? (
        <div className="rounded-xl border border-border bg-panel p-6 text-center text-sm text-muted">
          No peer agents have posted yet. Enable A2A_ENABLED and allow-list an
          agent id in A2A_AGENTS.
        </div>
      ) : (
        <StaggerList>
          {data.agents.map((a: A2AAgentState) => (
            <StaggerItem key={a.agent}>
              <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-panel px-4 py-3">
                <div>
                  <div className="flex items-center gap-2 font-medium">
                    {a.agent} {modeBadge(a.mode)}
                  </div>
                  <div className="mt-1 text-xs text-muted">
                    accepted {a.accepted} · refused {a.refused} · traded{" "}
                    {a.traded} · shadow {a.shadow_passes} passed /{" "}
                    {a.shadow_fails} failed
                  </div>
                  {a.last_reason && (
                    <div className="mt-1 text-xs text-muted">{a.last_reason}</div>
                  )}
                </div>
                <div className="flex gap-2">
                  {RAMP_MODES.map((m) => (
                    <button
                      key={m}
                      type="button"
                      onClick={() => ramp(a.agent, m)}
                      disabled={a.mode === m}
                      className={cn(
                        "rounded-lg border px-3 py-1 text-xs transition",
                        a.mode === m
                          ? "border-accent/40 bg-accent/15 text-accent"
                          : "border-border bg-panel text-muted hover:border-accent/40 hover:text-accent",
                      )}
                    >
                      {m}
                    </button>
                  ))}
                </div>
              </div>
            </StaggerItem>
          ))}
        </StaggerList>
      )}
    </section>
  );
}
