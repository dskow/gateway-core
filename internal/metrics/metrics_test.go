package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// DP-002: multiple *Metrics instances must coexist without double-registration
// panics. Proves there is no hidden package-level state behind the struct.
func TestMetrics_TwoInstancesCoexist(t *testing.T) {
	a := New(prometheus.NewRegistry())
	b := New(prometheus.NewRegistry())

	a.RequestsTotal.WithLabelValues("/a", "GET", "200").Inc()
	b.RequestsTotal.WithLabelValues("/b", "GET", "200").Inc()
	b.RequestsTotal.WithLabelValues("/b", "GET", "200").Inc()

	if got := testutil.ToFloat64(a.RequestsTotal.WithLabelValues("/a", "GET", "200")); got != 1 {
		t.Fatalf("a.RequestsTotal = %v, want 1", got)
	}
	if got := testutil.ToFloat64(b.RequestsTotal.WithLabelValues("/b", "GET", "200")); got != 2 {
		t.Fatalf("b.RequestsTotal = %v, want 2", got)
	}
	// Neither instance's series leaks into the other.
	if got := testutil.ToFloat64(a.RequestsTotal.WithLabelValues("/b", "GET", "200")); got != 0 {
		t.Fatalf("a saw b's series: got %v", got)
	}
}

// DP-002: the same collector names from the pre-refactor globals must still
// be exposed so existing dashboards and scrape configs keep working.
func TestMetrics_ExposesExpectedCollectorNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	// Exercise every collector so at least one sample exists per family.
	m.RequestsTotal.WithLabelValues("/x", "GET", "200").Inc()
	m.RequestDuration.WithLabelValues("/x", "GET").Observe(0.1)
	m.ActiveConnections.Inc()
	m.RateLimitHits.WithLabelValues("/x").Inc()
	m.AuthFailures.WithLabelValues("invalid_token").Inc()
	m.BackendErrors.WithLabelValues("/x", "http://b", "502").Inc()
	m.RetryTotal.WithLabelValues("/x", "http://b").Inc()
	m.CircuitBreakerStateChanges.WithLabelValues("http://b", "closed", "open").Inc()
	m.CircuitBreakerState.WithLabelValues("http://b").Set(1)
	m.BulkheadRejections.WithLabelValues("http://b").Inc()
	m.BulkheadInFlight.WithLabelValues("http://b").Set(0)
	m.RateLimitClientsTracked.Set(7)
	m.RateLimitClientsEvicted.Inc()
	m.ConfigReloadRollbacks.WithLabelValues("observer_error").Inc()

	rec := httptest.NewRecorder()
	Handler(reg).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	wanted := []string{
		"gateway_requests_total",
		"gateway_request_duration_seconds",
		"gateway_active_connections",
		"gateway_rate_limit_hits_total",
		"gateway_auth_failures_total",
		"gateway_backend_errors_total",
		"gateway_retries_total",
		"gateway_circuit_breaker_state_changes_total",
		"gateway_circuit_breaker_state",
		"gateway_bulkhead_rejections_total",
		"gateway_bulkhead_in_flight",
		"gateway_ratelimit_clients_tracked",
		"gateway_ratelimit_clients_evicted_total",
		"gateway_config_reload_rollbacks_total",
	}
	for _, name := range wanted {
		if !strings.Contains(out, name) {
			t.Errorf("%s missing from exported metrics", name)
		}
	}
	if rec.Code != http.StatusOK {
		t.Errorf("handler status = %d, want 200", rec.Code)
	}
}
