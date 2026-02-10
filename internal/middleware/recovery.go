package middleware

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recovery returns middleware that recovers from panics, logs the stack trace,
// and returns a 500 Internal Server Error JSON response.
func Recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					stack := string(debug.Stack())
					logger.Error("panic recovered",
						"error", err,
						"stack", stack,
						"method", r.Method,
						"path", r.URL.Path,
					)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"error":   "Internal Server Error",
						"message": "an unexpected error occurred",
					})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
