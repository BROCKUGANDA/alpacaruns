// Utility helpers shared across components.

import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

// cn — className composition with Tailwind-merge for conflicting
// utility classes. Used by every component that takes a className prop.
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

const USD = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});

// fmtMoney — render a number as USD with thousands separators and
// two decimal places. Centralized so every stat tile / table cell
// formats identically. Non-trivial formatting: Intl + sign handling
// — keep as a function.
export function fmtMoney(n: number) {
  if (!Number.isFinite(n)) return "—";
  if (n < 0) return "-" + USD.format(Math.abs(n));
  return USD.format(n);
}

// fmtPct — render a fractional value (0.0523) as "+5.23%". Used in
// every P/L cell; centralizing keeps the sign convention consistent.
export function fmtPct(n: number, decimals = 2) {
  if (!Number.isFinite(n)) return "—";
  return (n >= 0 ? "+" : "") + (n * 100).toFixed(decimals) + "%";
}

// tone — color class for a +/- signed number. Domain mapping used
// by both stat tiles and table cells.
export function tone(n: number) {
  if (n > 0) return "text-bull";
  if (n < 0) return "text-bear";
  return "text-muted";
}

// confidenceBg — gradient bar background based on confidence 0-1.
// Single source of truth for the green/amber/red rule used by both
// the trade-log row badge and the explainability side panel.
export function confidenceBg(c: number) {
  if (c >= 0.8) return "bg-bull";
  if (c >= 0.5) return "bg-warn";
  return "bg-bear";
}