import type { Metadata } from "next";
import "./globals.css";
import { SiteHeader } from "@/components/site-header";
import { AppBackground } from "@/components/app-background";
import { FooterApiUrl } from "@/components/footer-api-url";

export const metadata: Metadata = {
  title: "Alpacaruns — Live Paper Trading Bot",
  description:
    "Live status, trades and explainability for the Alpacaruns autonomous paper-trading bot.",
};

// Root layout for the showcase. Server component; the per-page
// containers add SWR providers + Framer Motion.
export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en" className="dark">
      <body className="min-h-screen bg-bg text-zinc-100 antialiased">
        <AppBackground />
        <div className="relative z-10">
          <SiteHeader />
          <main className="mx-auto max-w-7xl px-4 pb-24 pt-6 sm:px-6 lg:px-8">
            {children}
          </main>
          <footer className="mx-auto max-w-7xl px-4 pb-8 pt-4 text-center text-xs text-muted sm:px-6 lg:px-8">
            Alpacaruns showcase · API: <FooterApiUrl />
          </footer>
        </div>
      </body>
    </html>
  );
}