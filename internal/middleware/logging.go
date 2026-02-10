// Package middleware provides common HTTP middleware for the API gateway
// including structured logging, CORS, and panic recovery.
package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// statusRecorder wraps http.ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.statusCode = code
	sr.ResponseWriter.WriteHeader(code)
}

// Logging returns middleware that logs each request as structured JSON
// including method, path, status code, latency, and client IP.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			recorder := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(recorder, r)

			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", recorder.statusCode,
				"latency_ms", time.Since(start).Milliseconds(),
				"client_ip", r.RemoteAddr,
				"request_id", GetRequestID(r.Context()),
			)
		})
	}
}
