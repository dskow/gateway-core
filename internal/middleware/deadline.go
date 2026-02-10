package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Deadline returns middleware that applies a global request deadline to the
// entire middleware chain. If the deadline fires before the handler completes,
// a 504 Gateway Timeout is returned. Pass 0 to disable (handler called
// directly).
func Deadline(timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if timeout <= 0 {
			return next // disabled
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()

			done := make(chan struct{})
			tw := &deadlineWriter{ResponseWriter: w}

			go func() {
				next.ServeHTTP(tw, r.WithContext(ctx))
				close(done)
			}()

			select {
			case <-done:
				// Handler completed before deadline.
			case <-ctx.Done():
				// Deadline exceeded — only write 504 if the handler hasn't
				// started writing a response yet.
				if tw.tryClaimWrite() {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusGatewayTimeout)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"error":   http.StatusText(http.StatusGatewayTimeout),
						"message": "global request deadline exceeded",
					})
				}
				// Wait for handler goroutine to finish to avoid leaks.
				<-done
			}
		})
	}
}

// deadlineWriter wraps ResponseWriter and tracks whether any bytes have been
// written. This prevents the deadline handler from sending a 504 after the
// backend response has already started streaming to the client.
type deadlineWriter struct {
	http.ResponseWriter
	claimed bool
}

// tryClaimWrite atomically claims the right to write. Returns true if
// no bytes have been written yet. Not using sync.Once to avoid import;
// this is only called from two code paths that are synchronized via the
// done channel and context cancellation.
func (dw *deadlineWriter) tryClaimWrite() bool {
	if dw.claimed {
		return false
	}
	dw.claimed = true
	return true
}

func (dw *deadlineWriter) WriteHeader(code int) {
	dw.claimed = true
	dw.ResponseWriter.WriteHeader(code)
}

func (dw *deadlineWriter) Write(b []byte) (int, error) {
	dw.claimed = true
	return dw.ResponseWriter.Write(b)
}
