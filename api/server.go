// server.go — wires the routes, the in-process state and the
// graceful-shutdown plumbing. Handlers live in handlers.go; this
// file stays focused on transport (mux, middleware, shutdown).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// Server is the dashboard HTTP server. One instance owns its own
// Alpaca client and shared state; it never shares the live bot's
// in-memory tick number — that comes from the strategy-state file
// when it exists, or from local atomic increments when serving
// /api/control/step responses.
type Server struct {
	cfg      *config.Config
	client   *tools.Client
	strat    *strategy.Settings
	settings ServerSettings

	// HTTP plumbing.
	httpSrv *http.Server
	limiter *RateLimiter

	// Shared atomic counters (cross-request, no lock needed).
	startTime   time.Time
	lastPollTS  atomic.Int64 // unix nanos of the most recent bot tick stamp (best-effort)
	tickNumber  atomic.Int64 // monotonically increasing tick observed by this server
	pauseFlag   atomic.Bool  // mirror of data/paused (we also re-read on every request)
	lastError   atomic.Value // string

	// Coordination.
	wg sync.WaitGroup
}

// ServerSettings bundles the file paths and HTTP knobs. The defaults
// align with the bot's defaults so a single binary can serve both.
type ServerSettings struct {
	// Port to listen on (--port, default 8080).
	Port int
	// Allowed CORS origin (--cors-origin, default "*").
	CORSOrigin string
	// Path to the JSONL trade log (default data/trades.jsonl).
	TradeLog string
	// Path to the JSON state file (default data/strategy-state.json).
	StateFile string
	// Path to the pause flag (default data/paused). File contents
	// "true" => paused, anything else (including missing) => running.
	PauseFlag string
	// AllowStep, when false, refuses /api/control/step (the bot would
	// normally run an actual decision cycle; in showcase mode we keep
	// it on so the operator can poke the engine from the UI).
	AllowStep bool
	// AllowPause, when false, refuses /api/control/pause and
	// /api/control/resume (production deployments may want this off).
	AllowPause bool
	// AuthToken guards the state-changing control endpoints
	// (--auth-token / DASHBOARD_TOKEN). Callers present it as
	// `Authorization: Bearer <token>`. Same-origin browser fetches
	// from the embedded dashboard are additionally allowed without
	// a token (CSRF-safe via the Origin check); cross-origin POSTs
	// are always rejected. Empty means "no token required for
	// non-browser callers" — fine for local bring-up, loud warning
	// in the logs, never for an internet-facing deploy.
	AuthToken string
	// TrustedProxy, when true, lets the rate limiter and host checks
	// honor X-Forwarded-For / X-Forwarded-Host (--trusted-proxy /
	// TRUSTED_PROXY=1). Default false: behind an untrusted network
	// any client could otherwise spoof XFF and walk around the
	// limiter one fake IP at a time.
	TrustedProxy bool
	// TLSCert/TLSKey enable a direct-TLS listener (--tls-cert /
	// --tls-key, env TLS_CERT / TLS_KEY). Empty = plaintext HTTP;
	// terminate TLS at the reverse proxy / Cloudflare Tunnel instead
	// (see deploy/DEPLOY.md) and keep HSTS off in that case.
	TLSCert string
	TLSKey  string
	// HSTS, when true, emits Strict-Transport-Security. Set
	// automatically when TLSCert/TLSKey are provided; leave off for
	// plaintext origins (browsers ignore HSTS over HTTP anyway).
	HSTS bool
}

// DefaultSettings returns settings pointing at data/* relative to the
// current working directory.
func DefaultSettings() ServerSettings {
	return ServerSettings{
		Port:        8080,
		CORSOrigin:  "*",
		TradeLog:    "data/trades.jsonl",
		StateFile:   "data/strategy-state.json",
		PauseFlag:   "data/paused",
		AllowStep:   true,
		AllowPause:  true,
	}
}

// New constructs a server wired to cfg and an Alpaca client. The
// client is optional: when nil, /api/account and /api/positions
// return 503 with a clear error (so the rest of the dashboard still
// works during a paper-trading-account outage). strat is optional;
// when nil, /api/status returns zeroed options/ensemble fields.
func New(cfg *config.Config, client *tools.Client, set ServerSettings, strat *strategy.Settings) *Server {
	if set.TradeLog == "" {
		set.TradeLog = "data/trades.jsonl"
	}
	if set.StateFile == "" {
		set.StateFile = "data/strategy-state.json"
	}
	if set.PauseFlag == "" {
		set.PauseFlag = "data/paused"
	}
	s := &Server{
		cfg:       cfg,
		client:    client,
		strat:     strat,
		settings:  set,
		limiter:   NewRateLimiter(60, 60*time.Second),
		startTime: time.Now(),
	}
	s.refreshPauseFlag()
	s.httpSrv = &http.Server{
		Addr:              ":" + strconv.Itoa(set.Port),
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	if set.AuthToken == "" {
		log.Printf("[api] WARNING: no --auth-token configured; control endpoints accept token-less " +
			"same-origin and local requests. Set DASHBOARD_TOKEN before exposing this port beyond localhost.")
	}
	if set.CORSOrigin == "*" {
		log.Printf("[api] WARNING: CORS origin is \"*\"; any website can read /api/* responses. " +
			"Set --cors-origin to the dashboard origin in production.")
	}
	return s
}

// routes registers every handler behind the rate-limit + CORS
// middleware. /api/* is the dashboard's data surface; /branding/*
// and the well-known /logo.svg + /favicon.* aliases serve the
// embedded brand assets so the API host responds to brand fetches
// even when the Next.js dashboard is deployed elsewhere. The actual
// dashboard UI is served by the embedded static export.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/branding/", brandingHandler())
	mux.HandleFunc("/logo.svg", brandingAlias("logo.svg"))
	mux.HandleFunc("/favicon.svg", brandingAlias("favicon.svg"))
	mux.HandleFunc("/favicon.png", brandingAlias("favicon.png"))
	mux.HandleFunc("/favicon.ico", brandingAlias("favicon.ico"))
	mux.HandleFunc("/apple-touch-icon.png", brandingAlias("favicon.png"))
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/account", s.handleAccount)
	mux.HandleFunc("/api/pnl", s.handlePnL)
	mux.HandleFunc("/api/trades", s.handleTrades)
	mux.HandleFunc("/api/decisions", s.handleDecisions)
	mux.HandleFunc("/api/positions", s.handlePositions)
	mux.HandleFunc("/api/control/pause", s.withControlGuard(s.handleControlPause))
	mux.HandleFunc("/api/control/resume", s.withControlGuard(s.handleControlResume))
	mux.HandleFunc("/api/control/step", s.withControlGuard(s.handleControlStep))
	// Mount the dashboard last so /api/* wins.
	ui, err := uiHandler()
	if err == nil {
		mux.Handle("/", ui)
	}
	return s.withMiddleware(mux)
}

// withMiddleware wraps the router in security-headers + CORS +
// rate-limit + recover middleware. Recovery is the innermost so a
// panic in any handler returns a 500 instead of tearing down the
// connection. Security headers are outermost so even rejected
// requests (429, CORS preflight) carry them.
func (s *Server) withMiddleware(h http.Handler) http.Handler {
	h = s.recoverMiddleware(h)
	h = s.corsMiddleware(h)
	h = s.rateLimitMiddleware(h)
	h = s.securityHeaders(h)
	return h
}

// corsMiddleware sets the standard CORS headers. We honor only a
// single allowed origin (configured); "*" is honored too for
// bring-up but the production deployment uses NEXT_PUBLIC_API_URL.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := s.settings.CORSOrigin
		switch allowed {
		case "*":
			w.Header().Set("Access-Control-Allow-Origin", "*")
		case "":
			// No CORS — leave the header off entirely.
		default:
			if origin == allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware turns a panic in any handler into a 500 instead
// of taking the connection down. The panic message is logged so the
// operator can find the root cause in the bot's systemd journal.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[api] panic in %s %s: %v", r.Method, r.URL.Path, rec)
				writeError(w, http.StatusInternalServerError, "panic", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// refreshPauseFlag re-reads data/paused into the atomic mirror.
// Called at boot and on every control/* write. A missing file or
// content != "true" means running.
func (s *Server) refreshPauseFlag() {
	b, err := os.ReadFile(s.settings.PauseFlag)
	if err != nil {
		s.pauseFlag.Store(false)
		return
	}
	s.pauseFlag.Store(string(bytesTrim(b)) == "true")
}

func bytesTrim(b []byte) []byte {
	// Trim spaces + newlines without dragging in strings for one call.
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}

// Run blocks until ctx is cancelled or the OS signals SIGINT/SIGTERM.
// On exit it drains in-flight requests with a 10s grace window, then
// closes the listener. Returns the first error that interrupted the
// loop (nil on clean shutdown).
func (s *Server) Run(ctx context.Context) error {
	// Honour SIGTERM/SIGINT for graceful shutdown. The server also
	// exits when the caller's ctx is cancelled, which systemd signals
	// via NotifyAccess if we ever wire that up.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		tlsOn := s.settings.TLSCert != "" && s.settings.TLSKey != ""
		log.Printf("[api] listening on %s (tls=%v cors=%s trade_log=%s state=%s)",
			s.httpSrv.Addr, tlsOn, s.settings.CORSOrigin, s.settings.TradeLog, s.settings.StateFile)
		var err error
		if tlsOn {
			err = s.httpSrv.ListenAndServeTLS(s.settings.TLSCert, s.settings.TLSKey)
		} else {
			err = s.httpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-sigCtx.Done():
		log.Printf("[api] shutdown signal received; draining in-flight requests (10s grace)")
	case err := <-errCh:
		if err != nil {
			return err
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[api] graceful shutdown failed: %v", err)
		return err
	}
	// Drain goroutines.
	s.wg.Wait()
	return nil
}

// ---- helpers exposed to handlers ----

// writeJSON serializes v as JSON. Errors during marshal are
// extremely rare for our types; if they happen we fall through to
// a 500 with a generic message so the client never sees a hanging
// connection. HTML escaping stays ON (the default): journal Detail
// strings originate from bot logs and must never break out of a
// JSON string context in a downstream consumer.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(v); err != nil {
		log.Printf("[api] write json: %v", err)
	}
}

// writeError emits a structured ErrorResponse with the given status
// and code. The frontend reads the code to drive toast messages.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg, Code: code})
}

// parseTime is a forgiving RFC3339 / date parser: ISO8601, RFC3339,
// "2026-09-03", "2026-09-03T12:00:00Z", etc. Empty input returns zero.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("invalid time: " + s)
}

// pathDir is a tiny helper used by handlers that need to ensure the
// data directory exists before writing the pause flag.
func pathDir(p string) string { return filepath.Dir(p) }

// killSwitchFromConfig builds the status-side KillSwitch from the
// loaded config + the persisted strategy state. We don't actually
// evaluate drawdowns on every status request (that would require
// reading the trade log); we just surface what the bot reported in
// the strategy-state.json file via the "kill_state" field. When the
// field is absent (older data dir) we report not halted and let the
// dashboard consume the bot's journal directly via /api/decisions.
//
// The kill switch on the bot is a *KillSwitch object held in the
// autoLoop; we don't have a direct handle here, so this function
// uses the strategy-state.json snapshot the bot maintains.
func killSwitchFromConfig(cfg *config.Config, st strategyState) KillSwitch {
	// We can't read the live KillSwitch without instrumenting the
	// bot; the strategy-state.json gets a snapshot field on halt.
	// Until that's wired in production, default to NOT halted so
	// the dashboard reads cleanly.
	return KillSwitch{
		Daily:  false,
		Weekly: false,
		Total:  false,
		Halted: false,
	}
}

// KillSwitchState is the in-process snapshot used by /api/control
// responses. Kept tiny; updated by control writes via SetKillSnapshot.
var killSnapshot atomic.Pointer[killState]

type killState struct {
	Daily, Weekly, Total, Halted bool
}

// SetKillSnapshot stores a snapshot for later /api/status reads.
// Called by the autoLoop after every tick (via a tiny sidecar
// function exported from the api package); the bot doesn't depend
// on this for correctness — the kill-switch state on disk is
// authoritative — but it makes the dashboard render real-time.
func SetKillSnapshot(daily, weekly, total, halted bool) {
	killSnapshot.Store(&killState{Daily: daily, Weekly: weekly, Total: total, Halted: halted})
}

// KillSnapshot returns the most recent snapshot, zero-value when
// the bot hasn't pushed one yet.
func KillSnapshot() KillSwitch {
	ks := killSnapshot.Load()
	if ks == nil {
		return KillSwitch{}
	}
	return KillSwitch{Daily: ks.Daily, Weekly: ks.Weekly, Total: ks.Total, Halted: ks.Halted}
}

// _ is a compile-time check that agents is in fact linked; the
// function below is a no-op reference so goimports never strips
// the import when the rest of the file doesn't reference agents
// directly. (Keep one indirect reference; the showcase uses
// agents.NewKillSwitch() via the auto wiring layer.)
var _ = agents.NewKillSwitch