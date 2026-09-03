"use client";

// Soft radial gradient background — adds depth without competing
// with the content. Pure CSS, no client state.

export function AppBackground() {
  return (
    <div
      aria-hidden
      className="pointer-events-none fixed inset-0 z-0"
      style={{
        background:
          "radial-gradient(1200px 600px at 50% -100px, rgba(16,185,129,0.10), transparent 60%), radial-gradient(900px 600px at 100% 30%, rgba(14,165,233,0.08), transparent 60%)",
      }}
    />
  );
}