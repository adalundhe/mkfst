package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// === per-source rate limit ===
//
// A token bucket per source IP, used to throttle workflow
// submissions. Defends against an authenticated user (or a
// compromised credential) flooding the bundle pipeline — bundling
// is CPU-bound and could DOS the server even when the bundle
// itself is rejected.
//
// Defaults are conservative; operators tune via the constructor.

// RateLimiter is a per-IP token bucket.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    int           // tokens per second
	burst   int           // bucket capacity
	gcEvery time.Duration // sweep idle entries this often
	lastGC  time.Time
}

type bucket struct {
	tokens    float64
	updatedAt time.Time
}

// NewRateLimiter returns a limiter with the given per-source
// rate (tokens/second) and burst capacity. Idle source entries
// are swept every minute by the next request that triggers GC.
func NewRateLimiter(ratePerSecond, burst int) *RateLimiter {
	if ratePerSecond <= 0 {
		ratePerSecond = 1
	}
	if burst <= 0 {
		burst = ratePerSecond
	}
	return &RateLimiter{
		buckets: map[string]*bucket{},
		rate:    ratePerSecond,
		burst:   burst,
		gcEvery: time.Minute,
		lastGC:  time.Now(),
	}
}

// Allow reports whether a request from sourceIP may proceed,
// consuming a token if so.
func (r *RateLimiter) Allow(sourceIP string) bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gcLocked(now)
	b, ok := r.buckets[sourceIP]
	if !ok {
		b = &bucket{tokens: float64(r.burst), updatedAt: now}
		r.buckets[sourceIP] = b
	}
	// Refill since last seen.
	elapsed := now.Sub(b.updatedAt).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * float64(r.rate)
		if b.tokens > float64(r.burst) {
			b.tokens = float64(r.burst)
		}
		b.updatedAt = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// gcLocked drops idle entries that haven't been touched in 5×
// gcEvery. Caller holds r.mu.
func (r *RateLimiter) gcLocked(now time.Time) {
	if now.Sub(r.lastGC) < r.gcEvery {
		return
	}
	cutoff := now.Add(-5 * r.gcEvery)
	for ip, b := range r.buckets {
		if b.updatedAt.Before(cutoff) {
			delete(r.buckets, ip)
		}
	}
	r.lastGC = now
}

// Middleware wraps an http.Handler with a per-source-IP rate limit.
// Sources are identified by the leftmost X-Forwarded-For entry when
// trustForwardedFor is true; otherwise the connection's RemoteAddr.
func (r *RateLimiter) Middleware(trustForwardedFor bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ip := extractSourceIP(req, trustForwardedFor)
		if !r.Allow(ip) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate_limited","reason":"per-source rate limit exceeded"}`))
			return
		}
		next.ServeHTTP(w, req)
	})
}

func extractSourceIP(req *http.Request, trustForwardedFor bool) string {
	if trustForwardedFor {
		if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
			// Leftmost entry is the original client.
			for i, c := range xff {
				if c == ',' {
					return trimSpace(xff[:i])
				}
			}
			return trimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

func trimSpace(s string) string {
	a, b := 0, len(s)
	for a < b && (s[a] == ' ' || s[a] == '\t') {
		a++
	}
	for b > a && (s[b-1] == ' ' || s[b-1] == '\t') {
		b--
	}
	return s[a:b]
}
