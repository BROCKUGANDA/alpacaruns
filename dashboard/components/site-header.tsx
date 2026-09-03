"use client";

// Site-wide header: logo + brand + nav links + live badge.
// Pulls bot state from /api/status every 5s and renders the
// status pill inline next to the brand.

import Link from "next/link";
import { usePathname } from "next/navigation";
import useSWR from "swr";
import { swrFetcher, type StatusResponse } from "@/lib/api";
import { cn } from "@/lib/utils";

const NAV = [
  { href: "/", label: "Live" },
  { href: "/trades", label: "Trades" },
  { href: "/brain", label: "Brain" },
  { href: "/controls", label: "Controls" },
];

export function SiteHeader() {
  const path = usePathname();
  const { data, error, isLoading } = useSWR<StatusResponse>(
    "/api/status",
    swrFetcher,
    { refreshInterval: 5000 },
  );

  const state = data?.bot ?? "running";
  const pillColor =
    state === "halted"
      ? "bg-danger/20 text-danger border-danger/40"
      : state === "paused"
        ? "bg-warn/20 text-warn border-warn/40"
        : state === "error"
          ? "bg-danger/20 text-danger border-danger/40"
          : "bg-accent/15 text-accent border-accent/40";

  return (
    <header className="sticky top-0 z-30 border-b border-border bg-bg/80 backdrop-blur">
      <div className="mx-auto flex max-w-7xl items-center gap-4 px-4 py-3 sm:px-6 lg:px-8">
        <Link href="/" className="flex items-center gap-3">
          <img
            src="/logo.svg"
            alt=""
            width={32}
            height={32}
            className="h-8 w-8"
          />
          <span className="text-lg font-semibold tracking-tight">
            Alpacaruns
          </span>
        </Link>

        <span
          className={cn(
            "inline-flex items-center gap-2 rounded-full border px-3 py-1 text-xs uppercase tracking-wide",
            pillColor,
          )}
          aria-label={`bot state: ${state}`}
        >
          <span
            className={cn(
              "inline-block h-2 w-2 rounded-full",
              state === "halted" || state === "error"
                ? "bg-danger"
                : state === "paused"
                  ? "bg-warn"
                  : "bg-accent",
            )}
          />
          {isLoading ? "…" : error ? "offline" : state}
        </span>

        <nav className="ml-auto flex items-center gap-1 text-sm">
          {NAV.map((n) => {
            const active = path === n.href;
            return (
              <Link
                key={n.href}
                href={n.href}
                className={cn(
                  "rounded-md px-3 py-1.5 transition-colors",
                  active
                    ? "bg-panel text-zinc-100"
                    : "text-muted hover:bg-panel hover:text-zinc-100",
                )}
              >
                {n.label}
              </Link>
            );
          })}
        </nav>
      </div>
    </header>
  );
}