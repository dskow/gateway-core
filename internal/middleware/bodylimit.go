package middleware

import (
	"net/http"

	"github.com/dskow/gateway-core/internal/apierror"
)

// BodyLimit returns middleware that limits the size of request bodies.
// Requests exceeding maxBytes receive a 413 Request Entity Too Large response.
// It checks Content-Length upfront for an early reject and also wraps the body
// with http.MaxBytesReader as a safety net for chunked/streaming requests.
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Early reject: if Content-Length is known and exceeds limit, reject immediately
			if r.ContentLength > maxBytes {
				WriteBodyLimitError(w, r)
				return
			}
			// Safety net: wrap body with MaxBytesReader for chunked/streaming bodies
			if r.Body != nil && r.ContentLength != 0 {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WriteBodyLimitError writes a 413 JSON error response. Called by handlers
// that detect a MaxBytesReader error.
func WriteBodyLimitError(w http.ResponseWriter, r *http.Request) {
	apierror.WriteJSON(w, r, http.StatusRequestEntityTooLarge, apierror.BodyTooLarge, "request body exceeds maximum allowed size")
}
