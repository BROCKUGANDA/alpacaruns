"use client";

// PageShell — wraps a page's contents in a fade-up + stagger entrance
// animation. Every page in the showcase uses it; centralizing keeps
// the motion consistent.

import { motion } from "framer-motion";
import { ReactNode } from "react";

export function PageShell({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  return (
    <motion.div
      initial={{ opacity: 0, y: 12 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.4, ease: "easeOut" }}
      className={className}
    >
      {children}
    </motion.div>
  );
}

// StaggerList — animates direct children in sequence. Useful for
// stat-tile rows and table rows.
export function StaggerList({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  return (
    <motion.div
      initial="hidden"
      animate="show"
      variants={{
        hidden: {},
        show: { transition: { staggerChildren: 0.06 } },
      }}
      className={className}
    >
      {children}
    </motion.div>
  );
}

const childVariants = {
  hidden: { opacity: 0, y: 8 },
  show: { opacity: 1, y: 0, transition: { duration: 0.35 } },
};

export function StaggerItem({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  return (
    <motion.div variants={childVariants} className={className}>
      {children}
    </motion.div>
  );
}

// ShimmerBox — a loading placeholder. Width/height props are
// optional; the consumer can supply a className for more complex
// layouts.
export function ShimmerBox({
  className,
  height = 16,
}: {
  className?: string;
  height?: number;
}) {
  return (
    <div
      className={
        "relative overflow-hidden rounded-md bg-panel shimmer-bar " +
        (className ?? "")
      }
      style={{ height }}
    />
  );
}

// ErrorState — generic retry widget. Shown when an SWR fetch throws.
export function ErrorState({
  message,
  onRetry,
}: {
  message: string;
  onRetry?: () => void;
}) {
  return (
    <div className="rounded-lg border border-danger/40 bg-danger/10 p-4 text-sm">
      <div className="font-medium text-danger">Couldn't load data</div>
      <div className="mt-1 text-muted">{message}</div>
      {onRetry && (
        <button
          onClick={onRetry}
          className="mt-3 rounded-md border border-border bg-panel px-3 py-1 text-xs hover:border-accent"
        >
          Retry
        </button>
      )}
    </div>
  );
}