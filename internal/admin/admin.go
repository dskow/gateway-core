// Package admin provides read-only admin API endpoints for runtime inspection
// of gateway state. All endpoints are protected by IP allowlist.
package admin

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/dskow/gateway-core/internal/circuitbreaker"
	"github.com/dskow/gateway-core/internal/config"
	"github.com/dskow/gateway-core/internal/ratelimit"
)

// Handler provides admin API endpoints.
type Handler struct {
	reloader    ConfigProvider
	limiter     *ratelimit.Limiter
	breakers    map[string]*circuitbreaker.CompositeBreaker
	routes      []config.RouteConfig
	allowedNets []*net.IPNet
	logger      *slog.Logger
}

// ConfigProvider abstracts config access for testability.
type ConfigProvider interface {
	Current() *config.Config
}

// New creates a new admin Handler. The allowlist CIDRs must be pre-validated
// (config validation ensures this).
func New(
	reloader ConfigProvider,
	limiter *ratelimit.Limiter,
	breakers map[string]*circuitbreaker.CompositeBreaker,
	routes []config.RouteConfig,
	allowlist []string,
	logger *slog.Logger,
) *Handler {
	nets := make([]*net.IPNet, 0, len(allowlist))
	for _, cidr := range allowlist {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue // already validated by config
		}
		nets = append(nets, ipNet)
	}
	return &Handler{
		reloader:    reloader,
		limiter:     limiter,
		breakers:    breakers,
		routes:      routes,
		allowedNets: nets,
		logger:      logger,
	}
}

// RegisterRoutes adds admin routes to the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/routes", h.guard(h.routesHandler))
	mux.HandleFunc("/admin/config", h.guard(h.configHandler))
	mux.HandleFunc("/admin/limiters", h.guard(h.limitersHandler))
}

// guard wraps a handler with IP allowlist checking.
func (h *Handler) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
				"error": "Method Not Allowed",
			})
			return
		}

		ip := extractIP(r.RemoteAddr)
		if !h.isAllowed(ip) {
			h.logger.Warn("admin access denied", "client_ip", ip, "path", r.URL.Path)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "Forbidden",
			})
			return
		}
		next(w, r)
	}
}

func (h *Handler) isAllowed(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range h.allowedNets {
		if n.Contains(ip) {
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

// routeStatus is the response type for /admin/routes.
type routeStatus struct {
	PathPrefix          string   `json:"path_prefix"`
	Backend             string   `json:"backend"`
	Methods             []string `json:"methods,omitempty"`
	AuthRequired        bool     `json:"auth_required"`
	TimeoutMs           int      `json:"timeout_ms"`
	CircuitBreakerState string   `json:"circuit_breaker_state"`
}

func (h *Handler) routesHandler(w http.ResponseWriter, r *http.Request) {
	statuses := make([]routeStatus, len(h.routes))
	for i, route := range h.routes {
		cbState := "unknown"
		if cb, ok := h.breakers[route.Backend]; ok && cb != nil {
			switch cb.State() {
			case circuitbreaker.StateClosed:
				cbState = "closed"
			case circuitbreaker.StateOpen:
				cbState = "open"
			case circuitbreaker.StateHalfOpen:
				cbState = "half-open"
			}
		}
		statuses[i] = routeStatus{
			PathPrefix:          route.PathPrefix,
			Backend:             route.Backend,
			Methods:             route.Methods,
			AuthRequired:        route.AuthRequired,
			TimeoutMs:           route.TimeoutMs,
			CircuitBreakerState: cbState,
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"routes": statuses})
}

func (h *Handler) configHandler(w http.ResponseWriter, r *http.Request) {
	cfg := h.reloader.Current()

	// Deep copy and redact sensitive fields.
	redacted := *cfg
	if redacted.Auth.JWTSecret != "" {
		redacted.Auth.JWTSecret = "***"
	}

	writeJSON(w, http.StatusOK, redacted)
}

func (h *Handler) limitersHandler(w http.ResponseWriter, r *http.Request) {
	entries := h.limiter.Snapshot()

	// Pagination: page/page_size from query params.
	pageSize := 100
	page := 0

	if ps := r.URL.Query().Get("page_size"); ps != "" {
		if v := parseInt(ps); v > 0 && v <= 1000 {
			pageSize = v
		}
	}
	if p := r.URL.Query().Get("page"); p != "" {
		if v := parseInt(p); v >= 0 {
			page = v
		}
	}

	total := len(entries)
	start := page * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries[start:end],
		"total":   total,
		"page":    page,
	})
}

func parseInt(s string) int {
	s = strings.TrimSpace(s)
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
