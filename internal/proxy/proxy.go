// Package proxy provides a reverse proxy with route matching, path stripping,
// header injection, retries, and timeout handling.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dskow/go-api-gateway/internal/config"
	"github.com/dskow/go-api-gateway/internal/metrics"
	"github.com/dskow/go-api-gateway/internal/routing"
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

	route, ok := rt.matchRoute(r.URL.Path)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "no matching route")
		return
	}

	if len(route.Methods) > 0 && !methodAllowed(r.Method, route.Methods) {
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Sprintf("method %s not allowed for %s", r.Method, route.PathPrefix))
		return
	}

	metrics.ActiveConnections.Inc()
	defer metrics.ActiveConnections.Dec()

	proxy := rt.proxies[route.PathPrefix]

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

	// Wrap the response writer to capture the status code for metrics.
	recorder := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(r.Context(), route.Timeout())
		rWithCtx := r.WithContext(ctx)

		isFinal := attempt == maxAttempts

		if isFinal {
			// Final attempt: write directly to the real client.
			// Set latency header via a wrapper so it's written BEFORE
			// the reverse proxy calls WriteHeader on the response.
			lw := &latencyWriter{ResponseWriter: recorder, start: start}
			proxy.ServeHTTP(lw, rWithCtx)
			cancel()
			break
		}

		// Non-final attempt: buffer the full response so we can either
		// replay it on success or discard it and retry. This avoids the
		// double-backend-hit that the old discard+re-send approach had.
		buf := &responseBuffer{header: make(http.Header), statusCode: http.StatusOK}
		proxy.ServeHTTP(buf, rWithCtx)
		cancel()

		if !isRetryable(buf.statusCode) {
			// Success or non-retryable error — replay buffered response
			// to the real writer without hitting the backend again.
			// Set latency header BEFORE replay calls WriteHeader.
			w.Header().Set("X-Gateway-Latency", time.Since(start).String())
			buf.replayTo(recorder)
			break
		}

		metrics.RetryTotal.WithLabelValues(route.PathPrefix, route.Backend).Inc()

		rt.logger.Warn("retrying request",
			"path", originalPath,
			"backend", route.Backend,
			"attempt", attempt,
			"status", buf.statusCode,
		)

		backoff := time.Duration(100*(1<<(attempt-1))) * time.Millisecond
		time.Sleep(backoff)
	}

	latency := time.Since(start)

	statusStr := strconv.Itoa(recorder.statusCode)
	metrics.RequestsTotal.WithLabelValues(route.PathPrefix, r.Method, statusStr).Inc()
	metrics.RequestDuration.WithLabelValues(route.PathPrefix, r.Method).Observe(latency.Seconds())

	if recorder.statusCode >= 500 {
		metrics.BackendErrors.WithLabelValues(route.PathPrefix, route.Backend, statusStr).Inc()
	}
}

func (rt *Router) matchRoute(path string) (config.RouteConfig, bool) {
	for _, route := range rt.routes {
		if routing.MatchesPrefix(path, route.PathPrefix) {
			return route, true
		}
	}
	return config.RouteConfig{}, false
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

// Pre-serialized JSON error bodies avoid per-request json.Encoder allocations.
var (
	errBodyNotFound   = mustMarshalError(http.StatusNotFound, "no matching route")
	errBodyBadGateway = mustMarshalError(http.StatusBadGateway, "upstream service unavailable")
)

func mustMarshalError(status int, message string) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"error":   http.StatusText(status),
		"message": message,
	})
	return append(b, '\n')
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// Use pre-serialized body for common error messages to avoid
	// json.Encoder allocation on every error response.
	switch {
	case status == http.StatusNotFound && message == "no matching route":
		w.Write(errBodyNotFound)
	case status == http.StatusBadGateway && message == "upstream service unavailable":
		w.Write(errBodyBadGateway)
	default:
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   http.StatusText(status),
			"message": message,
		})
	}
}

// latencyWriter wraps an http.ResponseWriter and injects the
// X-Gateway-Latency header just before the first WriteHeader call.
// This ensures the header is set before the response is committed.
type latencyWriter struct {
	http.ResponseWriter
	start   time.Time
	written bool
}

func (lw *latencyWriter) WriteHeader(code int) {
	if !lw.written {
		lw.written = true
		lw.ResponseWriter.Header().Set("X-Gateway-Latency", time.Since(lw.start).String())
	}
	lw.ResponseWriter.WriteHeader(code)
}

func (lw *latencyWriter) Write(b []byte) (int, error) {
	if !lw.written {
		lw.written = true
		lw.ResponseWriter.Header().Set("X-Gateway-Latency", time.Since(lw.start).String())
	}
	return lw.ResponseWriter.Write(b)
}

// responseRecorder wraps http.ResponseWriter to capture the status code
// while still writing to the real client. Used for metrics reporting.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (rr *responseRecorder) WriteHeader(code int) {
	if !rr.written {
		rr.statusCode = code
		rr.written = true
	}
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	if !rr.written {
		rr.statusCode = http.StatusOK
		rr.written = true
	}
	return rr.ResponseWriter.Write(b)
}

// responseBuffer captures the full response (status, headers, body) in memory
// so it can be replayed to the real client on a successful non-final retry
// attempt. This replaces the old discard+re-send approach that hit the
// backend twice on every successful request with retries enabled.
type responseBuffer struct {
	header     http.Header
	body       bytes.Buffer
	statusCode int
	written    bool
}

func (b *responseBuffer) Header() http.Header { return b.header }

func (b *responseBuffer) WriteHeader(code int) {
	if !b.written {
		b.statusCode = code
		b.written = true
	}
}

func (b *responseBuffer) Write(p []byte) (int, error) {
	if !b.written {
		b.statusCode = http.StatusOK
		b.written = true
	}
	return b.body.Write(p)
}

// replayTo copies the buffered response (headers, status, body) to a real
// ResponseWriter. The recorder captures the status code for metrics.
func (b *responseBuffer) replayTo(rr *responseRecorder) {
	for k, vals := range b.header {
		for _, v := range vals {
			rr.Header().Add(k, v)
		}
	}
	rr.WriteHeader(b.statusCode)
	rr.Write(b.body.Bytes())
}
