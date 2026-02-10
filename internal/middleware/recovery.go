package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/dskow/gateway-core/internal/apierror"
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
						"request_id", GetRequestID(r.Context()),
					)
					apierror.WriteJSON(w, r, http.StatusInternalServerError, apierror.InternalError, "an unexpected error occurred")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
