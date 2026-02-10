package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dskow/go-api-gateway/internal/config"
)

func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"path":    r.URL.Path,
			"method":  r.Method,
			"headers": flatHeaders(r.Header),
		})
	})
}

func flatHeaders(h http.Header) map[string]string {
	out := make(map[string]string)
	for k := range h {
		out[k] = h.Get(k)
	}
	return out
}

func TestRouter_RouteMatching(t *testing.T) {
	backend := httptest.NewServer(echoHandler())
	defer backend.Close()

	routes := []config.RouteConfig{
		{PathPrefix: "/api/users", Backend: backend.URL, TimeoutMs: 5000},
		{PathPrefix: "/api", Backend: backend.URL, TimeoutMs: 5000},
	}

	logger := slog.Default()
	router, err := New(routes, nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	// Should match the longer prefix
	req := httptest.NewRequest("GET", "/api/users/123", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestRouter_NoMatchingRoute(t *testing.T) {
	routes := []config.RouteConfig{
		{PathPrefix: "/api", Backend: "http://localhost:9999", TimeoutMs: 5000},
	}
	logger := slog.Default()
	router, err := New(routes, nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/unknown", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestRouter_MethodNotAllowed(t *testing.T) {
	backend := httptest.NewServer(echoHandler())
	defer backend.Close()

	routes := []config.RouteConfig{
		{PathPrefix: "/api", Backend: backend.URL, Methods: []string{"GET"}, TimeoutMs: 5000},
	}
	logger := slog.Default()
	router, err := New(routes, nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/test", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestRouter_PrefixStripping(t *testing.T) {
	var receivedPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	routes := []config.RouteConfig{
		{PathPrefix: "/api/users", Backend: backend.URL, StripPrefix: true, TimeoutMs: 5000},
	}
	logger := slog.Default()
	router, err := New(routes, nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/users/123", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if receivedPath != "/123" {
		t.Errorf("expected stripped path /123, got %q", receivedPath)
	}
}

func TestRouter_PrefixStripping_RootPath(t *testing.T) {
	var receivedPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	routes := []config.RouteConfig{
		{PathPrefix: "/api/users", Backend: backend.URL, StripPrefix: true, TimeoutMs: 5000},
	}
	logger := slog.Default()
	router, err := New(routes, nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/users", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if receivedPath != "/" {
		t.Errorf("expected stripped path /, got %q", receivedPath)
	}
}

func TestRouter_HeaderInjection(t *testing.T) {
	var receivedHeaders http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	routes := []config.RouteConfig{
		{
			PathPrefix: "/api",
			Backend:    backend.URL,
			TimeoutMs:  5000,
			Headers:    map[string]string{"X-Source": "gateway", "X-Custom": "value"},
		},
	}
	logger := slog.Default()
	router, err := New(routes, nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/test", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if receivedHeaders.Get("X-Source") != "gateway" {
		t.Errorf("expected X-Source=gateway, got %q", receivedHeaders.Get("X-Source"))
	}
	if receivedHeaders.Get("X-Custom") != "value" {
		t.Errorf("expected X-Custom=value, got %q", receivedHeaders.Get("X-Custom"))
	}
}

// Note: X-Request-ID generation and preservation tests moved to
// middleware/requestid_test.go (RequestID middleware now handles this).

func TestRouter_XForwardedFor(t *testing.T) {
	var receivedXFF string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedXFF = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	routes := []config.RouteConfig{
		{PathPrefix: "/api", Backend: backend.URL, TimeoutMs: 5000},
	}
	logger := slog.Default()
	router, err := New(routes, nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = "192.168.1.1:54321"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if receivedXFF != "192.168.1.1" {
		t.Errorf("expected X-Forwarded-For=192.168.1.1, got %q", receivedXFF)
	}
}

func TestRouter_GatewayLatencyHeader(t *testing.T) {
	backend := httptest.NewServer(echoHandler())
	defer backend.Close()

	routes := []config.RouteConfig{
		{PathPrefix: "/api", Backend: backend.URL, TimeoutMs: 5000},
	}
	logger := slog.Default()
	router, err := New(routes, nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/test", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	latency := rec.Header().Get("X-Gateway-Latency")
	if latency == "" {
		t.Error("expected X-Gateway-Latency header")
	}
}

func TestRouter_InvalidBackendURL(t *testing.T) {
	routes := []config.RouteConfig{
		{PathPrefix: "/api", Backend: "://bad-url", TimeoutMs: 5000},
	}
	logger := slog.Default()
	_, err := New(routes, nil, logger)
	if err == nil {
		t.Error("expected error for invalid backend URL")
	}
}

// Note: newUUID test moved to middleware/requestid_test.go.

// Note: matchesPrefix tests moved to internal/routing/match_test.go
// since the function was extracted into the shared routing package.

func TestRouter_PathBoundaryEnforcement(t *testing.T) {
	backend := httptest.NewServer(echoHandler())
	defer backend.Close()

	routes := []config.RouteConfig{
		{PathPrefix: "/api", Backend: backend.URL, TimeoutMs: 5000},
	}
	logger := slog.Default()
	router, err := New(routes, nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	// /api/test should match /api
	req := httptest.NewRequest("GET", "/api/test", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/api/test: expected 200, got %d", rec.Code)
	}

	// /api.evil.com should NOT match /api
	req2 := httptest.NewRequest("GET", "/api.evil.com/steal", nil)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("/api.evil.com/steal: expected 404, got %d", rec2.Code)
	}

	// /apiary should NOT match /api
	req3 := httptest.NewRequest("GET", "/apiary", nil)
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Errorf("/apiary: expected 404, got %d", rec3.Code)
	}
}
