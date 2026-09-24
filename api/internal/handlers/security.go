package handlers

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Browser-facing defences. The API is reachable from the public internet
// through the SSH reverse tunnel, and the session cookie is SameSite=Lax --
// which is a *site* rule, so any sibling host under the same registrable
// domain counts as same-site. Origin checks are what actually separate them.

// sameOrigin reports whether the request's Origin is the API's own origin or
// one of the configured app origins. An empty Origin means a non-browser
// caller (CLI, agent, curl), which carries a token and cannot be tricked by
// a third-party page.
func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, a := range s.Origins {
		if a != "" && strings.EqualFold(o, strings.TrimRight(a, "/")) {
			return true
		}
	}
	return false
}

// csrfGuard rejects cross-origin state changes that ride on the session
// cookie. Token-authenticated calls are exempt: a cross-site page cannot read
// the token, so there is nothing to forge.
func (s *Server) csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if bearer(r) == "" && !s.sameOrigin(r) {
			writeJSON(w, 403, v1.Error{Error: "cross-origin request refused", Code: "forbidden"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets the handful that matter for a JSON API plus a static
// SPA: no MIME sniffing, no framing, no referrer leakage to other sites.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// rateLimiter is a per-IP token bucket. It exists mostly to stop credential
// stuffing and runaway scripts; the limits are far above normal CLI use.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
	last    time.Time // last sweep
}

type bucket struct {
	tokens float64
	seen   time.Time
}

func newRateLimiter(perMinute, burst int) *rateLimiter {
	return &rateLimiter{
		buckets: map[string]*bucket{},
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
		last:    time.Now(),
	}
}

// allow consumes one token for key, reporting whether it was available.
func (l *rateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	// Drop idle buckets now and then so the map cannot grow without bound.
	if now.Sub(l.last) > 5*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 5*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.last = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.seen).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.seen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Agents hold long-lived connections and stream logs; limiting them
		// would throttle training output, not an attacker.
		if !strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/api/v1/agent/") {
			next.ServeHTTP(w, r)
			return
		}
		if !s.limiter.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "10")
			writeJSON(w, 429, v1.Error{Error: "too many requests", Code: "rate_limited"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP prefers the proxy header: the API sits behind nginx in the public
// deployment, where RemoteAddr is always the tunnel.
func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		if i := strings.IndexByte(f, ','); i > 0 {
			return strings.TrimSpace(f[:i])
		}
		return strings.TrimSpace(f)
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return host
}
