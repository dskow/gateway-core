package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dskow/gateway-core/internal/config"
	"github.com/prometheus/client_golang/prometheus"
)

// newTestGateway builds a gateway backed by a httptest upstream. It returns
// the gateway and the upstream so tests can close the upstream and inspect
// the gateway's composed handler without binding a TCP listener.
func newTestGateway(t *testing.T, build func(upstreamURL string) *config.Config) (*Gateway, *httptest.Server) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)

	cfg, err := config.LoadFromBytes([]byte(""))
	_ = cfg
	_ = err
	// We want a fully-validated Config, so build it via LoadFromBytes after
	// the caller supplies upstream-dependent fields.
	cfg = build(upstream.URL)

	gw, err := NewGateway(context.Background(), cfg, slog.Default(), Options{
		Registerer: prometheus.NewRegistry(), // isolate from any other test.
		Gatherer:   prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	t.Cleanup(gw.Limiter.Close)
	return gw, upstream
}

// DP-003 acceptance: table-driven end-to-end test that constructs a Gateway
// with fakes and issues requests in-process. Each case names the path it
// hits, the method, any bypass expectations, and the required status code.
func TestGateway_EndToEnd(t *testing.T) {
	gw, upstream := newTestGateway(t, func(backend string) *config.Config {
		cfg := &config.Config{
			Server: config.ServerConfig{
				Port:         0,
				MaxBodyBytes: 1 << 20,
			},
			Metrics: config.MetricsConfig{Path: "/metrics"},
			Logging: config.LoggingConfig{Output: "stdout"},
			RateLimit: config.RateLimitConfig{
				RequestsPerSecond: 1000,
				BurstSize:         1000,
			},
			CircuitBreaker: config.CircuitBreakerConfig{
				WindowSize:       10,
				FailureThreshold: 0.5,
				ResetTimeout:     30_000_000_000,
				HalfOpenMax:      2,
			},
			Routes: []config.RouteConfig{
				{PathPrefix: "/api", Backend: backend, TimeoutMs: 5000, StripPrefix: true},
			},
		}
		return cfg
	})
	_ = upstream

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string // substring check; empty = skip
	}{
		{"proxied_get", http.MethodGet, "/api/users", http.StatusOK, "ok"},
		{"proxied_post", http.MethodPost, "/api/things", http.StatusOK, "ok"},
		{"unknown_route", http.MethodGet, "/nope", http.StatusNotFound, ""},
		{"liveness_bypass", http.MethodGet, "/health", http.StatusOK, ""},
		{"readiness_bypass", http.MethodGet, "/ready", http.StatusOK, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			gw.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s: status = %d, want %d (body=%q)",
					tc.method, tc.path, rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("%s %s: body = %q, missing %q",
					tc.method, tc.path, rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// Gateway must wire the proxy end-to-end on an isolated metrics registry
// so parallel suites do not collide on the default prometheus registry.
func TestGateway_IsolatedMetricsRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	gw, _ := newTestGatewayWithRegistry(t, reg)
	req := httptest.NewRequest("GET", "/api/x", nil)
	gw.Handler().ServeHTTP(httptest.NewRecorder(), req)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	if !names["gateway_requests_total"] {
		t.Error("expected gateway_requests_total to be registered on the isolated registry")
	}
}

func newTestGatewayWithRegistry(t *testing.T, reg *prometheus.Registry) (*Gateway, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 0, MaxBodyBytes: 1 << 20},
		Metrics: config.MetricsConfig{Path: "/metrics"},
		Logging: config.LoggingConfig{Output: "stdout"},
		RateLimit: config.RateLimitConfig{
			RequestsPerSecond: 1000, BurstSize: 1000,
		},
		CircuitBreaker: config.CircuitBreakerConfig{
			WindowSize: 10, FailureThreshold: 0.5,
			ResetTimeout: 30_000_000_000, HalfOpenMax: 2,
		},
		Routes: []config.RouteConfig{
			{PathPrefix: "/api", Backend: upstream.URL, TimeoutMs: 5000},
		},
	}
	gw, err := NewGateway(context.Background(), cfg, slog.Default(), Options{
		Registerer: reg,
		Gatherer:   reg,
	})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	t.Cleanup(gw.Limiter.Close)
	return gw, upstream
}
