// Package ratelimit provides per-client-IP token bucket rate limiting
// middleware for the API gateway.
package ratelimit

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dskow/go-api-gateway/internal/config"
	"golang.org/x/time/rate"
)

type client struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Limiter tracks per-client rate limiters and performs periodic cleanup
// of stale entries.
type Limiter struct {
	mu           sync.Mutex
	clients      map[string]*client
	rate         rate.Limit
	burst        int
	routes       []config.RouteConfig
	trustedCIDRs []*net.IPNet
	logger       *slog.Logger
	stopCh       chan struct{}
}

// New creates a new Limiter with the given global rate limit settings and
// route-level overrides. It starts a background goroutine that cleans up
// stale client entries every minute. trustedProxies is a list of CIDR strings
// (e.g. "10.0.0.0/8") whose X-Forwarded-For headers are trusted.
func New(cfg config.RateLimitConfig, routes []config.RouteConfig, trustedProxies []string, logger *slog.Logger) *Limiter {
	cidrs := parseCIDRs(trustedProxies, logger)
	l := &Limiter{
		clients:      make(map[string]*client),
		rate:         rate.Limit(cfg.RequestsPerSecond),
		burst:        cfg.BurstSize,
		routes:       routes,
		trustedCIDRs: cidrs,
		logger:       logger,
		stopCh:       make(chan struct{}),
	}
	go l.cleanup()
	return l
}

func parseCIDRs(cidrs []string, logger *slog.Logger) []*net.IPNet {
	var nets []*net.IPNet
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			logger.Warn("invalid trusted proxy CIDR, skipping", "cidr", cidr, "error", err)
			continue
		}
		nets = append(nets, ipNet)
	}
	return nets
}

// Stop terminates the background cleanup goroutine.
func (l *Limiter) Stop() {
	close(l.stopCh)
}

// Middleware returns an HTTP middleware that enforces rate limits.
func (l *Limiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := l.clientIP(r)
			rateLimit, burst := l.limitsForPath(r.URL.Path)

			limiter := l.getLimiter(ip, rateLimit, burst)
			if !limiter.Allow() {
				l.logger.Warn("rate limit exceeded", "client_ip", ip, "path", r.URL.Path)
				retryAfter := fmt.Sprintf("%.0f", 1.0/float64(rateLimit))
				w.Header().Set("Retry-After", retryAfter)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error":   "Too Many Requests",
					"message": "rate limit exceeded, retry later",
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// clientIP extracts the real client IP. X-Forwarded-For is only trusted when
// the direct peer (RemoteAddr) is in the trusted proxies list.
func (l *Limiter) clientIP(r *http.Request) string {
	peerIP := extractIP(r.RemoteAddr)

	if len(l.trustedCIDRs) > 0 && l.isTrusted(peerIP) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Walk right-to-left, return first non-trusted IP
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				ip := strings.TrimSpace(parts[i])
				if ip != "" && !l.isTrusted(ip) {
					return ip
				}
			}
		}
	}

	return peerIP
}

func (l *Limiter) isTrusted(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, cidr := range l.trustedCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func extractIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// pathMatchesPrefix checks if path matches prefix with boundary enforcement.
// The path must either equal the prefix, the prefix must end with "/",
// or the character after the prefix in path must be "/".
func pathMatchesPrefix(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if len(path) == len(prefix) {
		return true
	}
	if prefix[len(prefix)-1] == '/' {
		return true
	}
	return path[len(prefix)] == '/'
}

func (l *Limiter) limitsForPath(path string) (rate.Limit, int) {
	var bestMatch *config.RateLimitConfig
	bestLen := 0
	for _, route := range l.routes {
		if route.RateOverride != nil && pathMatchesPrefix(path, route.PathPrefix) {
			if len(route.PathPrefix) > bestLen {
				bestLen = len(route.PathPrefix)
				bestMatch = route.RateOverride
			}
		}
	}
	if bestMatch != nil {
		return rate.Limit(bestMatch.RequestsPerSecond), bestMatch.BurstSize
	}
	return l.rate, l.burst
}

func (l *Limiter) getLimiter(ip string, r rate.Limit, burst int) *rate.Limiter {
	key := fmt.Sprintf("%s:%v:%d", ip, r, burst)
	l.mu.Lock()
	defer l.mu.Unlock()

	if c, exists := l.clients[key]; exists {
		c.lastSeen = time.Now()
		return c.limiter
	}

	limiter := rate.NewLimiter(r, burst)
	l.clients[key] = &client{limiter: limiter, lastSeen: time.Now()}
	return limiter
}

func (l *Limiter) cleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.mu.Lock()
			for key, c := range l.clients {
				if time.Since(c.lastSeen) > 3*time.Minute {
					delete(l.clients, key)
				}
			}
			l.mu.Unlock()
		case <-l.stopCh:
			return
		}
	}
}
