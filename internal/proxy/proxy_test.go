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
	router, err := New(routes, logger)
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
	router, err := New(routes, logger)
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
	router, err := New(routes, logger)
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
	router, err := New(routes, logger)
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
	router, err := New(routes, logger)
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
	router, err := New(routes, logger)
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

func TestRouter_RequestIDGenerated(t *testing.T) {
	backend := httptest.NewServer(echoHandler())
	defer backend.Close()

	routes := []config.RouteConfig{
		{PathPrefix: "/api", Backend: backend.URL, TimeoutMs: 5000},
	}
	logger := slog.Default()
	router, err := New(routes, logger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/test", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	reqID := rec.Header().Get("X-Request-ID")
	if reqID == "" {
		t.Error("expected X-Request-ID header to be set")
	}
}

func TestRouter_RequestIDPreserved(t *testing.T) {
	backend := httptest.NewServer(echoHandler())
	defer backend.Close()

	routes := []config.RouteConfig{
		{PathPrefix: "/api", Backend: backend.URL, TimeoutMs: 5000},
	}
	logger := slog.Default()
	router, err := New(routes, logger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.Header.Set("X-Request-ID", "my-custom-id")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	reqID := rec.Header().Get("X-Request-ID")
	if reqID != "my-custom-id" {
		t.Errorf("expected preserved X-Request-ID, got %q", reqID)
	}
}

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
	router, err := New(routes, logger)
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
	router, err := New(routes, logger)
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
	_, err := New(routes, logger)
	if err == nil {
		t.Error("expected error for invalid backend URL")
	}
}

func TestNewUUID(t *testing.T) {
	id := newUUID()
	if len(id) != 36 {
		t.Errorf("expected UUID length 36, got %d: %q", len(id), id)
	}
	// Check format: 8-4-4-4-12
	parts := []int{8, 4, 4, 4, 12}
	idx := 0
	for i, p := range parts {
		end := idx + p
		if i < len(parts)-1 {
			if id[end] != '-' {
				t.Errorf("expected dash at position %d", end)
			}
			end++ // skip dash
		}
		idx = end
	}
}

func TestMatchesPrefix(t *testing.T) {
	tests := []struct {
		path   string
		prefix string
		want   bool
	}{
		{"/api/users/123", "/api/users", true},
		{"/api/users", "/api/users", true},
		{"/api/", "/api/", true},
		{"/api/test", "/api/", true},
		{"/api.evil.com/steal", "/api", false},
		{"/api-extended", "/api", false},
		{"/apiary", "/api", false},
		{"/api", "/api", true},
		{"/api/test", "/api", true},
		{"/other", "/api", false},
	}

	for _, tt := range tests {
		t.Run(tt.path+"_vs_"+tt.prefix, func(t *testing.T) {
			got := matchesPrefix(tt.path, tt.prefix)
			if got != tt.want {
				t.Errorf("matchesPrefix(%q, %q) = %v, want %v", tt.path, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestRouter_PathBoundaryEnforcement(t *testing.T) {
	backend := httptest.NewServer(echoHandler())
	defer backend.Close()

	routes := []config.RouteConfig{
		{PathPrefix: "/api", Backend: backend.URL, TimeoutMs: 5000},
	}
	logger := slog.Default()
	router, err := New(routes, logger)
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
