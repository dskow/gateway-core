// Package metrics provides Prometheus instrumentation for the API gateway.
// All collectors live on a *Metrics struct that is constructed via New and
// injected into the components that emit them (DP-002). No package-level
// Prometheus state is kept here — this avoids double-registration panics,
// makes per-test isolation trivial (`metrics.New(prometheus.NewRegistry())`),
// and breaks the hidden coupling that the old Init/globals imposed.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics bundles every collector the gateway emits. Construct with New,
// then pass the *Metrics handle into each subsystem's constructor. The
// concrete fields are exported so emit sites read the same as before —
// `m.RequestsTotal.WithLabelValues(...)` — only the prefix changes.
type Metrics struct {
	RequestsTotal              *prometheus.CounterVec
	RequestDuration            *prometheus.HistogramVec
	ActiveConnections          prometheus.Gauge
	RateLimitHits              *prometheus.CounterVec
	AuthFailures               *prometheus.CounterVec
	BackendErrors              *prometheus.CounterVec
	RetryTotal                 *prometheus.CounterVec
	CircuitBreakerStateChanges *prometheus.CounterVec
	CircuitBreakerState        *prometheus.GaugeVec
	BulkheadRejections         *prometheus.CounterVec
	BulkheadInFlight           *prometheus.GaugeVec
	RateLimitClientsTracked    prometheus.Gauge
	RateLimitClientsEvicted    prometheus.Counter
	// ConfigReloadRollbacks counts rollbacks triggered when a config.Observer
	// returned an error or panicked during a reload (DP-001).
	ConfigReloadRollbacks *prometheus.CounterVec
}

// New constructs a Metrics bundle and registers every collector with reg.
// Metric names and label sets are stable with the pre-DP-002 globals so
// existing dashboards and scrape configs keep working. Pass
// prometheus.DefaultRegisterer for normal use, or prometheus.NewRegistry()
// in tests that need isolation from other suites.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_requests_total",
				Help: "Total HTTP requests processed",
			},
			[]string{"route", "method", "status"},
		),
		RequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "gateway_request_duration_seconds",
				Help:    "Request latency in seconds",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"route", "method"},
		),
		ActiveConnections: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "gateway_active_connections",
				Help: "Number of in-flight requests currently being processed",
			},
		),
		RateLimitHits: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_rate_limit_hits_total",
				Help: "Total rate limit rejections",
			},
			[]string{"route"},
		),
		AuthFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_auth_failures_total",
				Help: "Total authentication failures",
			},
			[]string{"reason"},
		),
		BackendErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_backend_errors_total",
				Help: "Total backend error responses (5xx)",
			},
			[]string{"route", "backend", "status"},
		),
		RetryTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_retries_total",
				Help: "Total retry attempts",
			},
			[]string{"route", "backend"},
		),
		CircuitBreakerStateChanges: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_circuit_breaker_state_changes_total",
				Help: "Total circuit breaker state transitions",
			},
			[]string{"backend", "from", "to"},
		),
		CircuitBreakerState: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gateway_circuit_breaker_state",
				Help: "Current circuit breaker state (0=closed, 1=open, 2=half-open)",
			},
			[]string{"backend"},
		),
		BulkheadRejections: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_bulkhead_rejections_total",
				Help: "Total requests rejected by bulkhead concurrency limiter",
			},
			[]string{"backend"},
		),
		BulkheadInFlight: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gateway_bulkhead_in_flight",
				Help: "Current number of in-flight requests per backend bulkhead",
			},
			[]string{"backend"},
		),
		RateLimitClientsTracked: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "gateway_ratelimit_clients_tracked",
				Help: "Current number of distinct client buckets held by the rate limiter",
			},
		),
		RateLimitClientsEvicted: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "gateway_ratelimit_clients_evicted_total",
				Help: "Total rate-limiter client entries evicted for idleness",
			},
		),
		ConfigReloadRollbacks: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_config_reload_rollbacks_total",
				Help: "Total config reloads rolled back because an observer errored or panicked",
			},
			[]string{"reason"},
		),
	}

	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDuration,
		m.ActiveConnections,
		m.RateLimitHits,
		m.AuthFailures,
		m.BackendErrors,
		m.RetryTotal,
		m.CircuitBreakerStateChanges,
		m.CircuitBreakerState,
		m.BulkheadRejections,
		m.BulkheadInFlight,
		m.RateLimitClientsTracked,
		m.RateLimitClientsEvicted,
		m.ConfigReloadRollbacks,
	)
	return m
}

// Handler returns an http.Handler that exports metrics gathered from g.
// Pass prometheus.DefaultGatherer to match the pre-DP-002 behavior.
func Handler(g prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(g, promhttp.HandlerOpts{})
}

// IncRollback records a single config reload rollback with the given
// reason label. Implements config.RollbackRecorder so the config package
// can count rollbacks without importing this package (DP-001).
func (m *Metrics) IncRollback(reason string) {
	m.ConfigReloadRollbacks.WithLabelValues(reason).Inc()
}
