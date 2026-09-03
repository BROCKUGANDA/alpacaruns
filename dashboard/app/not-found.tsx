"use client";

// 404 page — themed to match the showcase. "Looks like this trade
// didn't fill" matches the spec verbatim.

import Link from "next/link";
import { motion } from "framer-motion";

export default function NotFound() {
  return (
    <motion.div
      initial={{ opacity: 0, y: 12 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.3 }}
      className="flex flex-col items-center justify-center py-24 text-center"
    >
      <div className="text-7xl font-bold tracking-tight text-accent">404</div>
      <div className="mt-4 text-xl font-semibold">
        Looks like this trade didn&rsquo;t fill.
      </div>
      <p className="mt-2 max-w-md text-sm text-muted">
        The page you&rsquo;re looking for either moved, was renamed, or
        never existed. Try one of the live views from the header.
      </p>
      <Link
        href="/"
        className="mt-6 rounded-md border border-accent/40 bg-accent/10 px-4 py-2 text-sm text-accent hover:bg-accent/20"
      >
        Back to live dashboard
      </Link>
    </motion.div>
  );
}