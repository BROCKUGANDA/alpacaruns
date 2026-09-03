"use client";

// SWR provider — global configuration. The dashboard refreshes
// most read endpoints every 5s; the controls page triggers
// mutations explicitly.

import { SWRConfig } from "swr";

export function SwrProvider({ children }: { children: React.ReactNode }) {
  return (
    <SWRConfig
      value={{
        // 5s default refresh — the dashboard uses SWR's built-in
        // deduping so two components polling the same endpoint
        // don't double-fetch.
        refreshInterval: 5000,
        // Don't revalidate on focus — the user could be reading a
        // table row and a focus-driven refresh would jump them.
        revalidateOnFocus: false,
        // Reconnect-driven refetch is fine; a network blip should
        // pull fresh data once the tab is online again.
        revalidateOnReconnect: true,
        // Keep previous data visible while a new fetch is in
        // flight so the UI never flashes to a loading state on
        // every poll.
        keepPreviousData: true,
        onError: (err) => {
          if (process.env.NODE_ENV !== "production") {
            // eslint-disable-next-line no-console
            console.warn("[swr]", err);
          }
        },
      }}
    >
      {children}
    </SWRConfig>
  );
}