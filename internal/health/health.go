// Package health provides health check and readiness probe HTTP handlers.
package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/dskow/go-api-gateway/internal/config"
)

// Handler provides /health and /ready endpoints.
type Handler struct {
	routes []config.RouteConfig
	logger *slog.Logger
}

// New creates a new health check Handler.
func New(routes []config.RouteConfig, logger *slog.Logger) *Handler {
	return &Handler{routes: routes, logger: logger}
}

// RegisterRoutes adds health check routes to the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", h.liveness)
	mux.HandleFunc("/ready", h.readiness)
}

func (h *Handler) liveness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
	})
}

func (h *Handler) readiness(w http.ResponseWriter, r *http.Request) {
	allReady := true
	results := make(map[string]string, len(h.routes))

	for _, route := range h.routes {
		u, err := url.Parse(route.Backend)
		if err != nil {
			results[route.PathPrefix] = "invalid URL"
			allReady = false
			continue
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
			results[route.PathPrefix] = "unreachable"
			allReady = false
			h.logger.Warn("backend unreachable", "route", route.PathPrefix, "backend", route.Backend, "error", err)
		} else {
			conn.Close()
			results[route.PathPrefix] = "ok"
		}
	}

	status := http.StatusOK
	statusStr := "ready"
	if !allReady {
		status = http.StatusServiceUnavailable
		statusStr = "not ready"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   statusStr,
		"backends": results,
	})
}

func hasPort(host string) bool {
	_, _, err := net.SplitHostPort(host)
	return err == nil
}
