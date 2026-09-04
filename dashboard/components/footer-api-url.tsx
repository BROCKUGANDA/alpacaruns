"use client";

// FooterApiUrl — renders the API origin the dashboard is talking to.
// Client-only because window.location isn't available on the server.
// Same-origin HTTPS deploys (Cloudflare Tunnel) show the page origin;
// cross-origin deploys (NEXT_PUBLIC_API_URL set at build time) show
// the configured API URL.
import { useEffect, useState } from "react";

export function FooterApiUrl() {
  const [apiUrl, setApiUrl] = useState<string>("…");
  useEffect(() => {
    const fromEnv = process.env.NEXT_PUBLIC_API_URL;
    setApiUrl(
      fromEnv && fromEnv.trim() !== ""
        ? fromEnv
        : typeof window !== "undefined"
          ? window.location.origin
          : "",
    );
  }, []);
  return <code className="font-mono">{apiUrl}</code>;
}