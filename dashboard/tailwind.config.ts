import type { Config } from "tailwindcss";

const config: Config = {
  content: [
    "./app/**/*.{js,ts,jsx,tsx,mdx}",
    "./components/**/*.{js,ts,jsx,tsx,mdx}",
  ],
  darkMode: "class",
  theme: {
    extend: {
      colors: {
        // Custom Alpacaruns palette.
        bg: "#0a0e17",        // slate-950 base
        panel: "#0f1422",     // elevated panel
        border: "#1f2738",
        muted: "#9ca3af",
        accent: "#10b981",    // emerald-500 — bot running
        warn: "#f59e0b",      // amber-500 — paused / warning
        danger: "#ef4444",    // red-500 — halted
        bull: "#22c55e",
        bear: "#f87171",
      },
      fontFamily: {
        mono: ["ui-monospace", "SFMono-Regular", "Menlo", "Monaco", "Consolas", "monospace"],
      },
      keyframes: {
        shimmer: {
          "0%": { backgroundPosition: "-200% 0" },
          "100%": { backgroundPosition: "200% 0" },
        },
        "pulse-dot": {
          "0%, 100%": { opacity: "1", transform: "scale(1)" },
          "50%": { opacity: "0.5", transform: "scale(1.4)" },
        },
      },
      animation: {
        shimmer: "shimmer 2s linear infinite",
        "pulse-dot": "pulse-dot 1.6s ease-in-out infinite",
      },
      backgroundImage: {
        shimmer:
          "linear-gradient(90deg, transparent, rgba(255,255,255,0.04), transparent)",
      },
    },
  },
  plugins: [],
};

export default config;