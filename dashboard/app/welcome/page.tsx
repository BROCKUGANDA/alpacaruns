// Splash / landing page for the Alpacaruns demo dashboard.
// First impression: who this bot is, what it does, why it's not a static
// mockup, and one big "Enter Live Dashboard" button. Polls /api/health
// once on mount to show whether the upstream Go server is reachable.

"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { swrFetcher } from "@/lib/api";
import type { HealthResponse } from "@/lib/api";

type Conn = "checking" | "live" | "stale" | "offline";

export default function WelcomePage() {
  const [conn, setConn] = useState<Conn>("checking");

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (!r.ok) {
          if (!cancelled) setConn("offline");
          return;
        }
        const j = (await r.json()) as HealthResponse;
        if (cancelled) return;
        setConn(j.bot_alive ? "live" : "stale");
      } catch {
        if (!cancelled) setConn("offline");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const connLabel =
    conn === "live"
      ? "API online"
      : conn === "stale"
        ? "API reachable · bot idle"
        : conn === "offline"
          ? "API offline"
          : "Checking…";
  const connTone =
    conn === "live"
      ? "bg-accent/15 text-accent border-accent/40"
      : conn === "stale"
        ? "bg-warn/15 text-warn border-warn/40"
        : conn === "offline"
          ? "bg-danger/15 text-danger border-danger/40"
          : "bg-panel text-muted border-border";

  return (
    <main className="relative mx-auto flex min-h-[calc(100vh-160px)] max-w-6xl flex-col items-center justify-center px-4 sm:px-6 lg:px-8">
      <section className="grid w-full grid-cols-1 items-center gap-10 lg:grid-cols-[3fr_2fr]">
        {/* Left: hero copy */}
        <div className="space-y-7">
          <div className="inline-flex items-center gap-3">
            <span
              className={
                "inline-flex items-center gap-2 rounded-full border px-3 py-1 text-xs uppercase tracking-wide " +
                connTone
              }
              aria-label={`api status: ${connLabel}`}
            >
              <span
                className={
                  "inline-block h-2 w-2 rounded-full " +
                  (conn === "live"
                    ? "bg-accent"
                    : conn === "stale"
                      ? "bg-warn"
                      : conn === "offline"
                        ? "bg-danger"
                        : "bg-muted")
                }
              />
              {connLabel}
            </span>
            <span className="text-xs uppercase tracking-wide text-muted">
              Demo dashboard · v0.9.6
            </span>
          </div>

          <h1 className="text-balance text-4xl font-semibold leading-tight tracking-tight sm:text-5xl lg:text-6xl">
            An autonomous{" "}
            <span className="bg-gradient-to-r from-emerald-400 to-sky-400 bg-clip-text text-transparent">
              multi-agent
            </span>{" "}
            paper-trading bot.
            <br />
            <span className="text-muted">Live. Auditable. Code-enforced.</span>
          </h1>

          <p className="max-w-xl text-pretty text-base text-muted sm:text-lg">
            Alpacaruns runs two cooperating inference paths &mdash; a Google-ADK
            LLM agent graph and a deterministic mixture-of-experts ensemble
            &mdash; and funnels every order through the same Go-coded risk
            gate. This dashboard reads the live JSONL journal, the Alpaca
            account endpoint, and the decision log in real time.
          </p>

          <div className="flex flex-col gap-3 sm:flex-row">
            <Link
              href="/live"
              className="inline-flex items-center justify-center gap-2 rounded-lg border border-accent/50 bg-accent/15 px-5 py-3 text-sm font-semibold text-accent shadow-[0_0_24px_-6px_rgba(16,185,129,0.4)] transition hover:bg-accent/25"
            >
              Enter Live Dashboard
              <span aria-hidden>→</span>
            </Link>
            <a
              href="https://github.com/BROCKUGANDA/alpacaruns"
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center justify-center gap-2 rounded-lg border border-border bg-panel px-5 py-3 text-sm font-semibold text-zinc-200 transition hover:border-accent/40 hover:text-zinc-50"
            >
              View Source on GitHub
            </a>
          </div>

          <dl className="grid grid-cols-2 gap-x-6 gap-y-4 border-t border-border pt-6 text-sm sm:grid-cols-4">
            <div>
              <dt className="text-xs uppercase tracking-wide text-muted">
                Stack
              </dt>
              <dd className="mt-1 font-mono">Go 1.26 · React 18 · Tailwind v3</dd>
            </div>
            <div>
              <dt className="text-xs uppercase tracking-wide text-muted">
                Modes
              </dt>
              <dd className="mt-1 font-mono">LLM · MoE · Auto · Manual</dd>
            </div>
            <div>
              <dt className="text-xs uppercase tracking-wide text-muted">
                Venue
              </dt>
              <dd className="mt-1 font-mono">Alpaca paper</dd>
            </div>
            <div>
              <dt className="text-xs uppercase tracking-wide text-muted">
                Source of truth
              </dt>
              <dd className="mt-1 font-mono">data/trades.jsonl</dd>
            </div>
          </dl>
        </div>

        {/* Right: visual card with the logo and stat strip */}
        <aside className="relative">
          <div className="absolute -inset-6 -z-10 rounded-3xl bg-gradient-to-br from-emerald-500/15 via-sky-500/10 to-transparent blur-2xl" />
          <div className="rounded-2xl border border-border bg-panel/70 p-6 backdrop-blur">
            <div className="flex items-center gap-4">
              <img
                src="/logo.svg"
                alt="Alpacaruns"
                width={56}
                height={56}
                className="h-14 w-14"
              />
              <div>
                <div className="text-sm uppercase tracking-wide text-muted">
                  Alpacaruns
                </div>
                <div className="text-lg font-semibold">Demo Console</div>
              </div>
            </div>

            <div className="mt-6 grid grid-cols-2 gap-3 text-sm">
              <a
                href="/live"
                className="rounded-lg border border-border bg-bg/60 p-4 transition hover:border-accent/50"
              >
                <div className="text-xs uppercase tracking-wide text-muted">
                  01 · Live
                </div>
                <div className="mt-1 font-semibold">Status &amp; P&amp;L</div>
                <div className="mt-1 text-xs text-muted">
                  Equity curve, drawdown halts, open positions
                </div>
              </a>
              <a
                href="/trades"
                className="rounded-lg border border-border bg-bg/60 p-4 transition hover:border-accent/50"
              >
                <div className="text-xs uppercase tracking-wide text-muted">
                  02 · Trades
                </div>
                <div className="mt-1 font-semibold">Explainability</div>
                <div className="mt-1 text-xs text-muted">
                  Path, confidence, factor scores
                </div>
              </a>
              <a
                href="/brain"
                className="rounded-lg border border-border bg-bg/60 p-4 transition hover:border-accent/50"
              >
                <div className="text-xs uppercase tracking-wide text-muted">
                  03 · Brain
                </div>
                <div className="mt-1 font-semibold">Agent views</div>
                <div className="mt-1 text-xs text-muted">
                  Positions, signals, decision feed
                </div>
              </a>
              <a
                href="/controls"
                className="rounded-lg border border-border bg-bg/60 p-4 transition hover:border-accent/50"
              >
                <div className="text-xs uppercase tracking-wide text-muted">
                  04 · Controls
                </div>
                <div className="mt-1 font-semibold">Safe actions</div>
                <div className="mt-1 text-xs text-muted">
                  Pause / resume / step
                </div>
              </a>
            </div>

            <div className="mt-6 border-t border-border pt-4 text-muted">
              <p className="text-xs leading-relaxed">
                This UI is the spec from the hackathon brief &mdash; live status,
                trade log with explainability, an &quot;agent brain&quot; view, and
                read-only-by-default controls. The data on the next pages is the
                real JSONL journal of the production bot running 24/7 on the
                UpCloud VM.
              </p>
            </div>
          </div>
        </aside>
      </section>
    </main>
  );
}