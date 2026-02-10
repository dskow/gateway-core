// Package proxy provides a reverse proxy with route matching, path stripping,
// header injection, retries, and timeout handling.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dskow/go-api-gateway/internal/config"
)

// Router matches incoming requests to configured routes and proxies
// them to the appropriate backend.
type Router struct {
	routes  []config.RouteConfig
	proxies map[string]*httputil.ReverseProxy
	logger  *slog.Logger
}

// New creates a Router from the given route configurations. Routes are
// sorted by path prefix length (longest first) for correct matching.
func New(routes []config.RouteConfig, logger *slog.Logger) (*Router, error) {
	sorted := make([]config.RouteConfig, len(routes))
	copy(sorted, routes)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i].PathPrefix) > len(sorted[j].PathPrefix)
	})

	proxies := make(map[string]*httputil.ReverseProxy, len(routes))
	for _, route := range sorted {
		target, err := url.Parse(route.Backend)
		if err != nil {
			return nil, fmt.Errorf("invalid backend URL %q for route %q: %w", route.Backend, route.PathPrefix, err)
		}
		rte := route // capture for closure
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("proxy error", "error", err, "backend", rte.Backend, "path", r.URL.Path)
			writeJSONError(w, http.StatusBadGateway, "upstream service unavailable")
		}
		proxies[route.PathPrefix] = proxy
	}

	return &Router{
		routes:  sorted,
		proxies: proxies,
		logger:  logger,
	}, nil
}

// ServeHTTP implements http.Handler. It matches the request to a route,
// validates the HTTP method, injects headers, and proxies with retries.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = newUUID()
	}
	w.Header().Set("X-Request-ID", requestID)

	route, ok := rt.matchRoute(r.URL.Path)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "no matching route")
		return
	}

	if len(route.Methods) > 0 && !methodAllowed(r.Method, route.Methods) {
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Sprintf("method %s not allowed for %s", r.Method, route.PathPrefix))
		return
	}

	proxy := rt.proxies[route.PathPrefix]

	r.Header.Set("X-Request-ID", requestID)

	for k, v := range route.Headers {
		r.Header.Set(k, v)
	}

	originalPath := r.URL.Path
	if route.StripPrefix {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, route.PathPrefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
	}

	maxAttempts := route.RetryAttempts + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(r.Context(), route.Timeout())
		rWithCtx := r.WithContext(ctx)

		isFinal := attempt == maxAttempts

		if isFinal {
			// Final attempt writes directly to the client
			proxy.ServeHTTP(w, rWithCtx)
			cancel()
			break
		}

		// Non-final attempts: capture response to check status
		rec := &statusCapture{ResponseWriter: newDiscardWriter(), statusCode: http.StatusOK}
		proxy.ServeHTTP(rec, rWithCtx)
		cancel()

		if !isRetryable(rec.statusCode) {
			// Success or non-retryable error — re-send to the real writer
			ctx2, cancel2 := context.WithTimeout(r.Context(), route.Timeout())
			proxy.ServeHTTP(w, r.WithContext(ctx2))
			cancel2()
			break
		}

		rt.logger.Warn("retrying request",
			"path", originalPath,
			"backend", route.Backend,
			"attempt", attempt,
			"status", rec.statusCode,
		)

		backoff := time.Duration(100*(1<<(attempt-1))) * time.Millisecond
		time.Sleep(backoff)
	}

	latency := time.Since(start)
	w.Header().Set("X-Gateway-Latency", latency.String())
}

func (rt *Router) matchRoute(path string) (config.RouteConfig, bool) {
	for _, route := range rt.routes {
		if matchesPrefix(path, route.PathPrefix) {
			return route, true
		}
	}
	return config.RouteConfig{}, false
}

// matchesPrefix checks if path matches prefix with boundary enforcement.
// The path must either equal the prefix, the prefix must end with "/",
// or the character after the prefix in path must be "/".
func matchesPrefix(path, prefix string) bool {
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

// MatchRoute exposes route matching for use by other packages (e.g., auth middleware).
func (rt *Router) MatchRoute(path string) (config.RouteConfig, bool) {
	return rt.matchRoute(path)
}

func methodAllowed(method string, allowed []string) bool {
	for _, m := range allowed {
		if strings.EqualFold(method, m) {
			return true
		}
	}
	return false
}

func isRetryable(status int) bool {
	return status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

func newUUID() string {
	var uuid [16]byte
	_, _ = rand.Read(uuid[:])
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant 2
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:])
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error":   http.StatusText(status),
		"message": message,
	})
}

// statusCapture captures only the status code; body is discarded.
type statusCapture struct {
	http.ResponseWriter
	statusCode int
}

func (s *statusCapture) WriteHeader(code int) {
	s.statusCode = code
	s.ResponseWriter.WriteHeader(code)
}

// discardWriter discards all output — used during non-final retry attempts.
type discardWriter struct {
	header http.Header
}

func newDiscardWriter() *discardWriter {
	return &discardWriter{header: make(http.Header)}
}

func (d *discardWriter) Header() http.Header         { return d.header }
func (d *discardWriter) Write(p []byte) (int, error)  { return len(p), nil }
func (d *discardWriter) WriteHeader(_ int)            {}
