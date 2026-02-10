package middleware

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/dskow/gateway-core/internal/apierror"
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
					apierror.WriteJSON(w, r, http.StatusGatewayTimeout, apierror.DeadlineExceeded, "global request deadline exceeded")
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
//
// The claimed field uses atomic.Bool because the handler goroutine and the
// deadline goroutine race to claim the write (one calls WriteHeader/Write,
// the other calls tryClaimWrite after ctx.Done fires).
type deadlineWriter struct {
	http.ResponseWriter
	claimed atomic.Bool
}

// tryClaimWrite atomically claims the right to write. Returns true only
// once — the first caller wins. Uses CompareAndSwap for race-free
// coordination between the handler goroutine and the deadline goroutine.
func (dw *deadlineWriter) tryClaimWrite() bool {
	return dw.claimed.CompareAndSwap(false, true)
}

func (dw *deadlineWriter) WriteHeader(code int) {
	dw.claimed.Store(true)
	dw.ResponseWriter.WriteHeader(code)
}

func (dw *deadlineWriter) Write(b []byte) (int, error) {
	dw.claimed.Store(true)
	return dw.ResponseWriter.Write(b)
}
