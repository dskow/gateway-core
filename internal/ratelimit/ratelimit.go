// Package ratelimit provides per-client-IP token bucket rate limiting
// middleware for the API gateway.
package ratelimit

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dskow/gateway-core/internal/config"
	"github.com/dskow/gateway-core/internal/metrics"
	"github.com/dskow/gateway-core/internal/routing"
	"golang.org/x/time/rate"
)

type client struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// clientKey avoids fmt.Sprintf allocation in the hot path. The composite
// key encodes IP, rate, and burst so different route overrides get
// separate buckets.
type clientKey struct {
	ip    string
	rate  rate.Limit
	burst int
}

// Limiter tracks per-client rate limiters and performs periodic cleanup
// of stale entries.
type Limiter struct {
	mu           sync.RWMutex
	clients      map[clientKey]*client
	rate         rate.Limit
	burst        int
	routes       []config.RouteConfig
	trustedCIDRs []*net.IPNet
	logger       *slog.Logger
	stopCh       chan struct{}
}

// Pre-serialized 429 JSON body avoids json.Encoder allocation per rejection.
var errBodyTooManyRequests = []byte(`{"error":"Too Many Requests","message":"rate limit exceeded, retry later"}` + "\n")

// New creates a new Limiter with the given global rate limit settings and
// route-level overrides. It starts a background goroutine that cleans up
// stale client entries every minute. trustedProxies is a list of CIDR strings
// (e.g. "10.0.0.0/8") whose X-Forwarded-For headers are trusted.
func New(cfg config.RateLimitConfig, routes []config.RouteConfig, trustedProxies []string, logger *slog.Logger) *Limiter {
	cidrs := parseCIDRs(trustedProxies, logger)
	l := &Limiter{
		clients:      make(map[clientKey]*client),
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

// UpdateConfig hot-reloads the global rate limit settings and route overrides.
// Existing per-client limiters are cleared so new limits take effect immediately.
func (l *Limiter) UpdateConfig(cfg config.RateLimitConfig, routes []config.RouteConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.rate = rate.Limit(cfg.RequestsPerSecond)
	l.burst = cfg.BurstSize
	l.routes = routes

	// Clear existing limiters so new rates apply on next request.
	l.clients = make(map[clientKey]*client)
}

// Middleware returns an HTTP middleware that enforces rate limits.
func (l *Limiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := l.clientIP(r)

			// Single route scan returns rate, burst, and prefix — avoids
			// the old double-iteration of limitsForPath + routeForPath.
			rateLimit, burst, routePrefix := l.limitsForPath(r.URL.Path)

			limiter := l.getLimiter(ip, rateLimit, burst)
			if !limiter.Allow() {
				l.logger.Warn("rate limit exceeded", "client_ip", ip, "path", r.URL.Path)
				metrics.RateLimitHits.WithLabelValues(routePrefix).Inc()
				retryAfter := strconv.FormatFloat(1.0/float64(rateLimit), 'f', 0, 64)
				w.Header().Set("Retry-After", retryAfter)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write(errBodyTooManyRequests)
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

// limitsForPath returns the rate limit, burst, and matching route prefix
// for the given path. This combines the old limitsForPath + routeForPath
// into a single route scan to avoid iterating routes twice on rate-limit hits.
func (l *Limiter) limitsForPath(path string) (rate.Limit, int, string) {
	var bestOverride *config.RateLimitConfig
	bestLen := 0
	bestPrefix := "unknown"

	for _, route := range l.routes {
		if routing.MatchesPrefix(path, route.PathPrefix) && len(route.PathPrefix) > bestLen {
			bestLen = len(route.PathPrefix)
			bestPrefix = route.PathPrefix
			if route.RateOverride != nil {
				bestOverride = route.RateOverride
			}
		}
	}

	if bestOverride != nil {
		return rate.Limit(bestOverride.RequestsPerSecond), bestOverride.BurstSize, bestPrefix
	}
	return l.rate, l.burst, bestPrefix
}

// getLimiter returns or creates a rate limiter for the given client key.
// Uses RWMutex: read-lock for existing clients (common path), write-lock
// only for new insertions. rate.Limiter is internally goroutine-safe so
// Allow() does not need to be called under our lock.
func (l *Limiter) getLimiter(ip string, r rate.Limit, burst int) *rate.Limiter {
	key := clientKey{ip: ip, rate: r, burst: burst}

	// Fast path: read-lock for existing clients (the common case).
	l.mu.RLock()
	if c, exists := l.clients[key]; exists {
		// Avoid time.Now() on every hit — only update lastSeen if stale.
		// The cleanup threshold is 3 minutes; refreshing once per minute
		// is sufficient to prevent eviction.
		if time.Since(c.lastSeen) > 1*time.Minute {
			l.mu.RUnlock()
			l.mu.Lock()
			c.lastSeen = time.Now()
			l.mu.Unlock()
		} else {
			l.mu.RUnlock()
		}
		return c.limiter
	}
	l.mu.RUnlock()

	// Slow path: need write lock to insert new client.
	l.mu.Lock()
	defer l.mu.Unlock()

	// Double-check after acquiring write lock.
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
