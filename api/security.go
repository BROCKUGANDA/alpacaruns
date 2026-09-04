// security.go — the API's security surface in one place: control-endpoint
// authorization, security response headers, strict per-endpoint rate
// limits, input validators, and response trimming.
//
// Threat model (deliberately narrow — this is a single-binary demo
// API, not a multi-tenant service):
//
//   - The dashboard UI is served from the SAME origin (embedded static
//     export), so browser control actions are same-origin fetches.
//   - Cross-site request forgery is blocked by rejecting control POSTs
//     whose Origin/Referer names a foreign host.
//   - Programmatic callers (curl, scripts) authenticate with a bearer
//     token (--auth-token / DASHBOARD_TOKEN). When no token is
//     configured, token-less non-browser requests are still allowed
//     (dev/bring-up parity) but the server logs a loud warning at
//     startup and cross-origin POSTs are always rejected.
//   - There are no sessions and no cookies anywhere in this system —
//     auth state is never stored client-side beyond the operator's
//     own localStorage copy of the token (dashboard), so there is no
//     session fixation, hijacking, or cookie-flag surface to harden.
//   - There is no SQL and no file upload surface: journal paths come
//     from server flags, never from request input (asserted by test).
//   - Alpaca secrets live in the process environment / .env file and
//     are never logged, never embedded in responses, and never sent
//     to the browser. The account number is masked (last 4 only).
package api

import (
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxControlBody caps control-endpoint request bodies. The control
// endpoints take no body at all — the cap exists so a hostile client
// can't park a gigabyte upload on a handler that ignores it.
const maxControlBody = 8 << 10 // 8 KiB

// controlBudget is the strict per-IP budget for the state-changing
// control endpoints: 20 requests / minute with a burst of 20. Enough
// for an operator clicking buttons; tight enough to blunt a bot loop.
const controlBudget = 20

var controlMu sync.Mutex
var controlLimiters = map[*Server]*RateLimiter{}

// controlLimiter returns the strict limiter for s, creating it on
// first use so zero-value test Servers keep working.
func (s *Server) controlLimiter() *RateLimiter {
	controlMu.Lock()
	defer controlMu.Unlock()
	l, ok := controlLimiters[s]
	if !ok {
		l = NewRateLimiter(controlBudget, time.Minute)
		controlLimiters[s] = l
	}
	return l
}

// authorizeControl gates the state-changing POST endpoints
// (/api/control/pause, /resume, /step). Policy, in order:
//
//  1. Valid `Authorization: Bearer <token>` (constant-time compare)
//     always passes — this is the scripted-operator path.
//  2. Same-origin browser fetch (Origin or Referer present and its
//     host matches the request Host) passes — this is the embedded
//     dashboard path, and it doubles as CSRF protection because a
//     foreign page's fetch always carries a foreign Origin.
//  3. No Origin/Referer at all (curl, health probes, server-side
//     scripts): passes only when no auth token is configured.
//     When a token IS configured, these callers must present it.
//  4. Anything else (foreign Origin/Referer) is rejected.
//
// There is deliberately no cookie/session branch: this API is
// stateless and sets no cookies, so there is no ambient authority
// for a foreign site to ride on beyond the Origin check above.
func (s *Server) authorizeControl(w http.ResponseWriter, r *http.Request) bool {
	if s.validBearer(r) {
		return true
	}
	origin, referer := r.Header.Get("Origin"), r.Header.Get("Referer")
	if origin == "" && referer == "" {
		if s.settings.AuthToken != "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "control endpoint requires a bearer token")
			return false
		}
		return true
	}
	if sameOrigin(r) {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden", "control endpoint rejects cross-origin requests")
	return false
}

// validBearer compares the request's bearer token against the
// configured AuthToken in constant time. Empty configured token
// never validates (avoids matching an empty Authorization header).
func (s *Server) validBearer(r *http.Request) bool {
	want := s.settings.AuthToken
	if want == "" {
		return false
	}
	h := r.Header.Get("Authorization")
	if h == "" {
		return false
	}
	// Accept "Bearer <token>" (case-insensitive scheme); anything
	// else is not a bearer credential.
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "bearer") {
		return false
	}
	got := strings.TrimSpace(parts[1])
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// sameOrigin reports whether the request's Origin (preferred) or
// Referer names the same host the request was sent to. Host
// comparison ignores port; scheme is irrelevant for authority.
func sameOrigin(r *http.Request) bool {
	host := requestHost(r)
	if host == "" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil {
			return false
		}
		return strings.EqualFold(u.Hostname(), host)
	}
	u, err := url.Parse(r.Header.Get("Referer"))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), host)
}

// untrustedClientKey is the RemoteAddr-only key: no proxy headers,
// ever. Used by the package-level clientKey helper and as the safe
// default inside (s *Server) clientKey.
func untrustedClientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requestHost returns the request's host without port, lowercased by
// EqualFold callers. Prefers X-Forwarded-Host only when the operator
// explicitly trusts the proxy (same trust gate as X-Forwarded-For).
func requestHost(r *http.Request) string {
	h := r.Host
	if h == "" {
		h = r.URL.Host
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// limitControlBody caps and drains the body of control POSTs so a
// client can't hold a handler open with a slow/giant upload.
func limitControlBody(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBody)
	return true
}

// checkControlRate applies the strict control budget; writes 429 +
// Retry-After and returns false when exhausted.
func (s *Server) checkControlRate(w http.ResponseWriter, r *http.Request) bool {
	if !s.controlLimiter().Allow(s.clientKey(r)) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many control requests; slow down")
		return false
	}
	return true
}

// clientKey extracts the rate-limit key, honoring X-Forwarded-For
// ONLY when the operator runs behind a trusted proxy
// (--trusted-proxy / TRUSTED_PROXY=1). Trusting XFF unconditionally
// lets any client rotate its apparent IP per request and walk around
// the limiter, so the default is RemoteAddr.
func (s *Server) clientKey(r *http.Request) string {
	if s.settings.TrustedProxy {
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			if i := strings.IndexByte(xf, ','); i > 0 {
				return strings.TrimSpace(xf[:i])
			}
			return strings.TrimSpace(xf)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// securityHeaders sets the baseline response headers on every
// response (API + embedded UI alike):
//
//   X-Content-Type-Options: nosniff — blocks MIME-sniffing XSS.
//   X-Frame-Options: DENY — the dashboard is same-origin served and
//     never embedded; deny clickjacking outright.
//   Referrer-Policy: no-referrer — trade/account data must not leak
//     into a Referer header on outbound navigation.
//   Permissions-Policy — the dashboard needs no camera, mic, or
//     geolocation; deny them all.
//   Strict-Transport-Security — sent only when serving TLS directly
//     (or behind a TLS-terminating proxy the operator has marked as
//     trusted); HSTS over plaintext HTTP would be ignored anyway.
//   Content-Security-Policy: default-src 'none'; frame-ancestors
//     'none' — applied to /api/* JSON responses only. The embedded
//     Next.js UI needs inline scripts to hydrate, so a strict CSP
//     there would break the dashboard; JSON endpoints carry no
//     active content, so locking them down is free.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if s.settings.HSTS {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
		}
		next.ServeHTTP(w, r)
	})
}

// ---- input validators (block field tampering) ----

// parseLimit parses ?limit= with a hard ceiling. Invalid or missing
// values are rejected with 400 — silent clamping would let a caller
// believe they paged the full log when they didn't.
func parseLimit(w http.ResponseWriter, v string) (int, bool) {
	const def, max = 100, 200
	if v == "" {
		return def, true
	}
	n, err := atoiStrict(v)
	if err != nil || n <= 0 || n > max {
		writeError(w, http.StatusBadRequest, "bad_param", "limit must be an integer 1..200")
		return 0, false
	}
	return n, true
}

// parseCursor parses ?cursor= (first-record index). Negative or
// non-numeric cursors are rejected, never coerced.
func parseCursor(w http.ResponseWriter, v string) (int64, bool) {
	if v == "" {
		return 0, true
	}
	n, err := atoi64Strict(v)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, "bad_param", "cursor must be a non-negative integer")
		return 0, false
	}
	return n, true
}

// parseSymbolFilter validates ?symbol= against a tight charset
// (tickers, crypto pairs with /, OCC-ish dots/dashes). Anything else
// is rejected — the filter is matched server-side, so a hostile
// value could otherwise only waste a scan, but strictness keeps the
// API contract honest and the logs clean.
func parseSymbolFilter(w http.ResponseWriter, v string) (string, bool) {
	if v == "" {
		return "", true
	}
	if len(v) > 32 || !isSymbolChars(v) {
		writeError(w, http.StatusBadRequest, "bad_param", "symbol contains invalid characters")
		return "", false
	}
	return v, true
}

// parsePathFilter validates ?path= against the dashboard's fixed
// vocabulary (agent | ensemble | manual | auto). Unknown buckets are
// rejected rather than passed through to an exact-match that can
// never hit. "auto" is the deterministic factor-engine path written
// by the auto loop (formerly the only path); agent/ensemble/manual
// cover the LLM-gated, layer-2 ensemble, and dashboard-CLI paths.
func parsePathFilter(w http.ResponseWriter, v string) (string, bool) {
	if v == "" {
		return "", true
	}
	switch v {
	case "agent", "ensemble", "manual", "auto":
		return v, true
	}
	writeError(w, http.StatusBadRequest, "bad_param", "path must be one of agent, ensemble, manual, auto")
	return "", false
}

func isSymbolChars(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '/' || c == '.' || c == '-' || c == ':' || c == '_':
		default:
			return false
		}
	}
	return true
}

func atoiStrict(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errNotInt
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errNotInt
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return 0, errNotInt
		}
	}
	return n, nil
}

func atoi64Strict(s string) (int64, error) {
	var n int64
	if s == "" {
		return 0, errNotInt
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errNotInt
		}
		n = n*10 + int64(c-'0')
		if n > 1<<40 {
			return 0, errNotInt
		}
	}
	return n, nil
}

// errNotInt is a sentinel so validators can distinguish "not a
// number" without importing strconv into this file's narrative.
var errNotInt = errNotIntType{}

type errNotIntType struct{}

func (errNotIntType) Error() string { return "not an integer" }

// maskAccountNumber trims the Alpaca account number to its last 4
// digits (****1234). The full number is an account identifier with
// no dashboard use — the UI only needs to show WHICH account is
// connected, and the last 4 does that without leaking it to anyone
// shoulder-surfing the demo or scraping the JSON.
func maskAccountNumber(acct string) string {
	acct = strings.TrimSpace(acct)
	if len(acct) <= 4 {
		return "****"
	}
	return "****" + acct[len(acct)-4:]
}

// withControlGuard wraps the state-changing control handlers with,
// in order: body cap, strict rate limit, authorization. Anything
// rejected short-circuits before the handler runs.
func (s *Server) withControlGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limitControlBody(w, r)
		if !s.checkControlRate(w, r) {
			logControlAuth(r, "rate_limited")
			return
		}
		if !s.authorizeControl(w, r) {
			logControlAuth(r, "denied")
			return
		}
		h(w, r)
	}
}

// logControlAuth logs control rejections without the credential.
func logControlAuth(r *http.Request, code string) {
	log.Printf("[api] control auth %s: %s %s", code, r.Method, r.URL.Path)
}
