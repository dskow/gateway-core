package middleware

import (
	"net/http"
	"strings"
)

// CORSConfig holds CORS middleware settings.
type CORSConfig struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	MaxAge         string
}

// DefaultCORSConfig returns sensible CORS defaults.
func DefaultCORSConfig() CORSConfig {
	return CORSConfig{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders: []string{"Authorization", "Content-Type", "X-Request-ID"},
		MaxAge:         "86400",
	}
}

// CORS returns middleware that handles Cross-Origin Resource Sharing headers.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	origins := strings.Join(cfg.AllowedOrigins, ", ")
	methods := strings.Join(cfg.AllowedMethods, ", ")
	headers := strings.Join(cfg.AllowedHeaders, ", ")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only set CORS headers when the request includes an Origin
			// header (browser cross-origin request). Non-browser clients
			// (curl, backend services) skip the overhead entirely.
			if r.Header.Get("Origin") != "" {
				w.Header().Set("Access-Control-Allow-Origin", origins)
				w.Header().Set("Access-Control-Allow-Methods", methods)
				w.Header().Set("Access-Control-Allow-Headers", headers)
				w.Header().Set("Access-Control-Max-Age", cfg.MaxAge)
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
