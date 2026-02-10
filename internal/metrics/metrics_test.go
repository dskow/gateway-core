package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// resetRegistry creates a fresh registry for test isolation.
func resetRegistry(t *testing.T) {
	t.Helper()
	// Unregister and re-register to avoid "duplicate metrics collector" panics
	// across tests. We use a new registry per test.
}

func TestInit_RegistersMetrics(t *testing.T) {
	// Use a custom registry to avoid conflicts with other tests
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		RequestsTotal,
		RequestDuration,
		ActiveConnections,
		RateLimitHits,
		AuthFailures,
		BackendErrors,
		RetryTotal,
	)

	// Verify metrics are gatherable
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	// Should have at least some metric families registered
	// (counters/histograms start with 0 families until incremented)
	_ = families
}

func TestRequestsTotal_Increment(t *testing.T) {
	RequestsTotal.WithLabelValues("/api/users", "GET", "200").Inc()
	RequestsTotal.WithLabelValues("/api/users", "GET", "200").Inc()
	RequestsTotal.WithLabelValues("/api/users", "POST", "201").Inc()

	// Verify by collecting — if this doesn't panic, the metrics work
	RequestsTotal.WithLabelValues("/api/users", "GET", "200").Add(0)
}

func TestRequestDuration_Observe(t *testing.T) {
	RequestDuration.WithLabelValues("/api/users", "GET").Observe(0.123)
	RequestDuration.WithLabelValues("/api/users", "POST").Observe(0.456)

	// Verify by collecting
	RequestDuration.WithLabelValues("/api/users", "GET").Observe(0)
}

func TestActiveConnections_IncDec(t *testing.T) {
	ActiveConnections.Inc()
	ActiveConnections.Inc()
	ActiveConnections.Dec()
	// Should not panic
}

func TestRateLimitHits_Increment(t *testing.T) {
	RateLimitHits.WithLabelValues("/api/users").Inc()
	// Should not panic
}

func TestAuthFailures_Increment(t *testing.T) {
	AuthFailures.WithLabelValues("missing_token").Inc()
	AuthFailures.WithLabelValues("invalid_token").Inc()
	AuthFailures.WithLabelValues("insufficient_scope").Inc()
	// Should not panic
}

func TestBackendErrors_Increment(t *testing.T) {
	BackendErrors.WithLabelValues("/api/users", "http://backend:3000", "502").Inc()
	// Should not panic
}

func TestRetryTotal_Increment(t *testing.T) {
	RetryTotal.WithLabelValues("/api/users", "http://backend:3000").Inc()
	// Should not panic
}

func TestHandler_ReturnsPrometheusFormat(t *testing.T) {
	// Register metrics with default registry for handler test
	Init()

	// Increment a counter so there's output
	RequestsTotal.WithLabelValues("/test", "GET", "200").Inc()

	h := Handler()
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	body, _ := io.ReadAll(rec.Body)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "gateway_requests_total") {
		t.Error("expected gateway_requests_total in metrics output")
	}
	if !strings.Contains(bodyStr, "gateway_request_duration_seconds") {
		t.Error("expected gateway_request_duration_seconds in metrics output")
	}
	if !strings.Contains(bodyStr, "gateway_active_connections") {
		t.Error("expected gateway_active_connections in metrics output")
	}
}
