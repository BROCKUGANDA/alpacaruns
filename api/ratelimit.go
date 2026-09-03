// ratelimit.go — a tiny in-memory token-bucket rate limiter used by
// the server middleware. Sliding 60s window per remote IP; refills at
// 60 tokens / minute (1 req/s average, 60 burst). The bucket is a
// per-IP sync.Map; entries are pruned on each refill so the map
// doesn't leak long-lived IPs forever.
//
// This is intentionally simple: one process, one limiter, no
// distributed state. A multi-instance deployment would back this with
// Redis; for the showcase, in-memory is sufficient.
package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RateLimiter is a per-key token-bucket limiter. Default settings:
// 60 requests / 60 seconds, burst 60.
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     int           // tokens added per window
	window   time.Duration // window length
	burst    int           // max tokens (== rate for default config)
	lastPrune time.Time
}

type bucket struct {
	tokens   int
	lastFill time.Time
}

// NewRateLimiter constructs a limiter with the given tokens-per-window
// budget. window is the refill interval (60s = 1 req/s average).
func NewRateLimiter(rate int, window time.Duration) *RateLimiter {
	if rate <= 0 {
		rate = 60
	}
	if window <= 0 {
		window = 60 * time.Second
	}
	return &RateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		window:  window,
		burst:   rate,
		lastPrune: time.Now(),
	}
}

// Allow consumes one token for key. Returns true when the request
// fits inside the bucket, false when the caller should be rejected
// with HTTP 429.
func (r *RateLimiter) Allow(key string) bool {
	if key == "" {
		return true // no key, no rate limit (loopback diagnostics)
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()

	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{tokens: r.burst, lastFill: now}
		r.buckets[key] = b
	}
	// Refill: add tokens proportional to time since last refill,
	// clamped at burst.
	elapsed := now.Sub(b.lastFill)
	if elapsed >= r.window {
		// At least one window has passed — top up to burst.
		b.tokens = r.burst
		b.lastFill = now
	} else if elapsed > 0 {
		// Partial refill. refills are integer tokens; we floor.
		add := int(float64(r.rate) * (float64(elapsed) / float64(r.window)))
		if add > 0 {
			b.tokens += add
			if b.tokens > r.burst {
				b.tokens = r.burst
			}
			b.lastFill = b.lastFill.Add(time.Duration(add) * (r.window / time.Duration(r.rate)))
		}
	}
	// Opportunistic prune so the map doesn't grow unbounded.
	if now.Sub(r.lastPrune) > 5*time.Minute {
		for k, v := range r.buckets {
			if now.Sub(v.lastFill) > 10*time.Minute {
				delete(r.buckets, k)
			}
		}
		r.lastPrune = now
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// clientKey extracts the rate-limit key from a request. Honors
// X-Forwarded-For first (when behind a trusted proxy) then falls
// back to the remote address. Empty key means "no limit".
func clientKey(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		// Take the first address; trimmed of whitespace.
		if i := strings.IndexByte(xf, ','); i > 0 {
			return strings.TrimSpace(xf[:i])
		}
		return strings.TrimSpace(xf)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimitMiddleware is the http.Handler middleware that applies the
// limiter; 429 Too Many Requests when over budget.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.Allow(clientKey(r)) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}