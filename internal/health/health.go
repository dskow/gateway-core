// Package health provides health check and readiness probe HTTP handlers.
package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/dskow/gateway-core/internal/circuitbreaker"
	"github.com/dskow/gateway-core/internal/config"
)

// Pre-serialized liveness response avoids json.Encoder allocation.
var livenessBody = []byte(`{"status":"ok"}` + "\n")

const readinessCacheTTL = 5 * time.Second

// Handler provides /health and /ready endpoints.
type Handler struct {
	routes   []config.RouteConfig
	breakers map[string]*circuitbreaker.CompositeBreaker
	logger   *slog.Logger

	// Cached readiness result to avoid TCP-dialling every backend on
	// every /ready poll. Protected by cacheMu.
	cacheMu      sync.RWMutex
	cachedResult []byte
	cachedStatus int
	cachedAt     time.Time
}

// New creates a new health check Handler. breakers maps backend URLs to
// their circuit breaker instances (may be nil for backends without breakers).
func New(routes []config.RouteConfig, breakers map[string]*circuitbreaker.CompositeBreaker, logger *slog.Logger) *Handler {
	return &Handler{routes: routes, breakers: breakers, logger: logger}
}

// RegisterRoutes adds health check routes to the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", h.liveness)
	mux.HandleFunc("/ready", h.readiness)
}

func (h *Handler) liveness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(livenessBody)
}

func (h *Handler) readiness(w http.ResponseWriter, r *http.Request) {
	// Serve from cache if fresh.
	h.cacheMu.RLock()
	if h.cachedResult != nil && time.Since(h.cachedAt) < readinessCacheTTL {
		body := h.cachedResult
		status := h.cachedStatus
		h.cacheMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
		return
	}
	h.cacheMu.RUnlock()

	type backendResult struct {
		prefix  string
		backend string
		status  string
		ok      bool
	}

	ch := make(chan backendResult, len(h.routes))
	for _, route := range h.routes {
		go func(route config.RouteConfig) {
			// Fast path: use circuit breaker state if available.
			if cb, exists := h.breakers[route.Backend]; exists && cb != nil {
				st := cb.State()
				switch st {
				case circuitbreaker.StateOpen:
					ch <- backendResult{prefix: route.PathPrefix, backend: route.Backend, status: "circuit-open", ok: false}
					return
				case circuitbreaker.StateHalfOpen:
					ch <- backendResult{prefix: route.PathPrefix, backend: route.Backend, status: "circuit-half-open", ok: true}
					return
				}
				// StateClosed — fall through to TCP dial for definitive check.
			}

			u, err := url.Parse(route.Backend)
			if err != nil {
				ch <- backendResult{prefix: route.PathPrefix, backend: route.Backend, status: "invalid URL", ok: false}
				return
			}

			host := u.Host
			if !hasPort(host) {
				switch u.Scheme {
				case "https":
					host += ":443"
				default:
					host += ":80"
				}
			}

			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", host)
			cancel()

			if err != nil {
				h.logger.Warn("backend unreachable", "route", route.PathPrefix, "backend", route.Backend, "error", err)
				ch <- backendResult{prefix: route.PathPrefix, backend: route.Backend, status: "unreachable", ok: false}
				return
			}
			conn.Close()
			ch <- backendResult{prefix: route.PathPrefix, backend: route.Backend, status: "ok", ok: true}
		}(route)
	}

	// Collect results and group by backend to determine readiness.
	// New logic: 503 only when ALL backends for any given route are down.
	// (Currently each route maps to one backend, but this is forward-compatible.)
	results := make(map[string]string, len(h.routes))
	anyRouteFullyDown := false

	for range h.routes {
		res := <-ch
		results[res.prefix] = res.status
		if !res.ok {
			anyRouteFullyDown = true
		}
	}

	httpStatus := http.StatusOK
	statusStr := "ready"
	if anyRouteFullyDown {
		httpStatus = http.StatusServiceUnavailable
		statusStr = "not ready"
	}

	body, _ := json.Marshal(map[string]interface{}{
		"status":   statusStr,
		"backends": results,
	})
	body = append(body, '\n')

	// Cache the result.
	h.cacheMu.Lock()
	h.cachedResult = body
	h.cachedStatus = httpStatus
	h.cachedAt = time.Now()
	h.cacheMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	w.Write(body)
}

func hasPort(host string) bool {
	_, _, err := net.SplitHostPort(host)
	return err == nil
}
