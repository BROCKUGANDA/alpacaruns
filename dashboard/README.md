# Alpacaruns Demo Dashboard

The multi-page demo app for the Alpacaruns autonomous paper-trading
bot. Built with **Next.js 14 (App Router) + TypeScript + Tailwind +
Framer Motion + SWR + Recharts**, statically exported and embedded
into the Go binary (`alpacaruns serve`) at `api/ui/`. Single
binary, single port, no Node process to run in production.

## Pages

- `/welcome` — splash page (entry point; `/` redirects here)
- `/live` — live status, P&L, equity curve, kill-switch badges
- `/trades` — trade log + side-panel explainability
- `/brain` — agent brain: open positions, factor panel, decision feed
- `/controls` — read-only config + safe actions (pause / resume / step)
- 404 themed: "Looks like this trade didn't fill."

The header shows the live bot state pulled from `/api/status` and
updates every 5s via SWR.

## How it's wired

```
+--------------------------+
|  alpacaruns serve :8080  |
|  Go HTTP API             |
+--------------------------+
   |    ^             ^
   | GET|             | GET /api/*  (the dashboard fetches JSON)
   v    |             |
browser <----- embedded Next.js static export at api/ui/
                  (1.5 MB, all 5 pages + _next chunks)
```

The Next.js build is **statically exported** with `next build && next
export` (or the new App Router equivalent). The resulting `out/`
directory is copied into `api/ui/` and embedded into the Go binary
via `//go:embed all:ui` in `api/ui.go`. The API's `/api/*` paths
take precedence over the static handler.

## Develop (live reload against the running Go API)

```bash
# In one terminal: run the API.
go run ./cmd/alpacaruns serve --port 8080 --cors-origin "*"

# In another: run the Next.js dev server.
cd dashboard
npm install
NEXT_PUBLIC_API_URL=http://localhost:8080 npm run dev
# open http://localhost:3000
```

The dev server proxies `/api/*` calls through `NEXT_PUBLIC_API_URL`
(so the Go API must allow that origin via `--cors-origin`).

## Build the embedded static export

```bash
cd dashboard
npm install
NEXT_PUBLIC_API_URL=https://your-api.example.com npm run build
# Next.js with output: 'export' produces ./out by default. Copy it
# into the api/ui/ embed path:
mkdir -p ../api/ui
rm -rf ../api/ui/*
cp -r out/* ../api/ui/
```

Then rebuild the Go binary:

```bash
go build -o alpacaruns.exe ./cmd/alpacaruns
./alpacaruns serve --port 8080
```

`http://localhost:8080/welcome` now serves the embedded dashboard.

## Environment

- `NEXT_PUBLIC_API_URL` — base URL of the Go API. **Build-time only**
  (it's a `NEXT_PUBLIC_*` var). Default: `http://localhost:8080`.

## Docker (standalone dashboard only)

```bash
docker build -t alpacaruns-dashboard .
docker run --rm -p 3000:3000 \
  -e NEXT_PUBLIC_API_URL=https://alpacaruns-api.example.com \
  alpacaruns-dashboard
```

The Dockerfile is a multi-stage Node 20 build → distroless Node 20
runtime, non-root, with a healthcheck. Most production deployments
should skip the container and use the embedded `api/ui/` route in
the Go binary instead.

## Security

No secrets in the client bundle. The only env var visible to the
browser is `NEXT_PUBLIC_API_URL`. API keys never leave the Go
process. The pause/resume/step actions POST to `/api/control/*`
which never exposes raw order placement.

## Tech notes

- Recharts for the equity curve (area chart, 24h / 7d / all toggle)
- Framer Motion for the page entrance + side-panel slide
- SWR with 5s default refresh, dedupe, and `keepPreviousData` so
  polled views never flash to loading
- Design tokens (colors, fonts, keyframes) live in
  `tailwind.config.ts` and `app/globals.css`; the splash is a dark
  emerald-on-slate theme that matches the brand logo
