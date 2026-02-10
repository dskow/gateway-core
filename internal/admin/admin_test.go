package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/dskow/gateway-core/internal/circuitbreaker"
	"github.com/dskow/gateway-core/internal/config"
	"github.com/dskow/gateway-core/internal/ratelimit"
)

// mockConfigProvider implements ConfigProvider for testing.
type mockConfigProvider struct {
	cfg *config.Config
}

func (m *mockConfigProvider) Current() *config.Config { return m.cfg }

func testHandler(t *testing.T, allowlist []string) (*Handler, *ratelimit.Limiter) {
	t.Helper()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	routes := []config.RouteConfig{
		{
			PathPrefix:   "/api/users",
			Backend:      "http://localhost:3001",
			Methods:      []string{"GET", "POST"},
			AuthRequired: true,
			TimeoutMs:    5000,
		},
	}

	cfg := &config.Config{
		Auth: config.AuthConfig{
			Enabled:   true,
			JWTSecret: "super-secret-key",
			Issuer:    "test",
			Audience:  "test",
		},
		Routes: routes,
	}

	limiter := ratelimit.New(
		config.RateLimitConfig{RequestsPerSecond: 100, BurstSize: 50},
		routes, nil, logger,
	)

	breakers := map[string]*circuitbreaker.CompositeBreaker{
		"http://localhost:3001": circuitbreaker.NewComposite("http://localhost:3001", circuitbreaker.Config{
			WindowSize:       10,
			FailureThreshold: 0.5,
			ResetTimeout:     30e9,
			HalfOpenMax:      2,
		}, logger),
	}

	reloader := &mockConfigProvider{cfg: cfg}

	h := New(reloader, limiter, breakers, routes, allowlist, logger)
	return h, limiter
}

func TestRoutesEndpoint(t *testing.T) {
	h, limiter := testHandler(t, []string{"127.0.0.0/8"})
	defer limiter.Stop()

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/admin/routes", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp map[string][]routeStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	routes := resp["routes"]
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}
	if routes[0].PathPrefix != "/api/users" {
		t.Errorf("path_prefix = %q, want /api/users", routes[0].PathPrefix)
	}
	if routes[0].CircuitBreakerState != "closed" {
		t.Errorf("circuit_breaker_state = %q, want closed", routes[0].CircuitBreakerState)
	}
}

func TestConfigEndpoint_RedactsSecret(t *testing.T) {
	h, limiter := testHandler(t, []string{"127.0.0.0/8"})
	defer limiter.Stop()

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/admin/config", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	if !contains(body, `"***"`) {
		t.Error("expected jwt_secret to be redacted")
	}
	if contains(body, "super-secret-key") {
		t.Error("jwt_secret was not redacted!")
	}
}

func TestIPAllowlist_Denied(t *testing.T) {
	h, limiter := testHandler(t, []string{"10.0.0.0/8"})
	defer limiter.Stop()

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/admin/routes", nil)
	req.RemoteAddr = "192.168.1.1:1234"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestIPAllowlist_Allowed(t *testing.T) {
	h, limiter := testHandler(t, []string{"192.168.0.0/16"})
	defer limiter.Stop()

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/admin/routes", nil)
	req.RemoteAddr = "192.168.1.100:5678"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestLimitersEndpoint(t *testing.T) {
	h, limiter := testHandler(t, []string{"127.0.0.0/8"})
	defer limiter.Stop()

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/admin/limiters", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := resp["total"]; !ok {
		t.Error("expected 'total' field in response")
	}
	if _, ok := resp["entries"]; !ok {
		t.Error("expected 'entries' field in response")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, limiter := testHandler(t, []string{"127.0.0.0/8"})
	defer limiter.Stop()

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/admin/routes", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
