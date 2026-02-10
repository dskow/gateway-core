// Package metrics provides Prometheus instrumentation for the API gateway.
// All metric collectors are registered on init via the Init function and
// exposed through the Handler for scraping.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// RequestsTotal counts total requests by route, method, and HTTP status code.
	RequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Total HTTP requests processed",
		},
		[]string{"route", "method", "status"},
	)

	// RequestDuration observes request latency in seconds by route and method.
	RequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "Request latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"route", "method"},
	)

	// ActiveConnections tracks the number of in-flight requests.
	ActiveConnections = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "gateway_active_connections",
			Help: "Number of in-flight requests currently being processed",
		},
	)

	// RateLimitHits counts rate limit rejections by route.
	RateLimitHits = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_rate_limit_hits_total",
			Help: "Total rate limit rejections",
		},
		[]string{"route"},
	)

	// AuthFailures counts authentication failures by reason.
	AuthFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_auth_failures_total",
			Help: "Total authentication failures",
		},
		[]string{"reason"},
	)

	// BackendErrors counts backend error responses by route, backend, and status.
	BackendErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_backend_errors_total",
			Help: "Total backend error responses (5xx)",
		},
		[]string{"route", "backend", "status"},
	)

	// RetryTotal counts retry attempts by route and backend.
	RetryTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_retries_total",
			Help: "Total retry attempts",
		},
		[]string{"route", "backend"},
	)
)

// Init registers all metric collectors with the default Prometheus registry.
// Must be called once at startup before handling requests.
func Init() {
	prometheus.MustRegister(
		RequestsTotal,
		RequestDuration,
		ActiveConnections,
		RateLimitHits,
		AuthFailures,
		BackendErrors,
		RetryTotal,
	)
}

// Handler returns an http.Handler that serves the Prometheus metrics endpoint.
func Handler() http.Handler {
	return promhttp.Handler()
}
