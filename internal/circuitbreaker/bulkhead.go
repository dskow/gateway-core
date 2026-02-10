package circuitbreaker

import (
	"time"

	"github.com/dskow/go-api-gateway/internal/metrics"
)

// BulkheadBreaker limits the number of concurrent in-flight requests to a
// backend. It wraps an inner Breaker and rejects requests when the concurrency
// limit is reached, preventing goroutine pileups and resource starvation.
type BulkheadBreaker struct {
	inner   Breaker
	sem     chan struct{}
	backend string
}

// NewBulkheadBreaker creates a concurrency-limiting breaker that allows at most
// maxConcurrent in-flight requests before rejecting.
func NewBulkheadBreaker(inner Breaker, maxConcurrent int, backend string) *BulkheadBreaker {
	return &BulkheadBreaker{
		inner:   inner,
		sem:     make(chan struct{}, maxConcurrent),
		backend: backend,
	}
}

// Allow tries to acquire a concurrency slot and then checks the inner breaker.
// If the concurrency limit is reached, returns false without blocking.
// If Allow returns true, the caller MUST call Release when the request completes.
func (b *BulkheadBreaker) Allow() bool {
	select {
	case b.sem <- struct{}{}:
		// Acquired slot — check inner breaker.
		metrics.BulkheadInFlight.WithLabelValues(b.backend).Set(float64(len(b.sem)))
		if !b.inner.Allow() {
			// Inner breaker rejected — release slot immediately.
			<-b.sem
			metrics.BulkheadInFlight.WithLabelValues(b.backend).Set(float64(len(b.sem)))
			return false
		}
		return true
	default:
		// Concurrency limit reached.
		metrics.BulkheadRejections.WithLabelValues(b.backend).Inc()
		return false
	}
}

// Release frees a concurrency slot after a request completes. Must be called
// exactly once for every Allow() that returned true.
func (b *BulkheadBreaker) Release() {
	<-b.sem
	metrics.BulkheadInFlight.WithLabelValues(b.backend).Set(float64(len(b.sem)))
}

func (b *BulkheadBreaker) RecordSuccess(latency time.Duration) {
	b.inner.RecordSuccess(latency)
}

func (b *BulkheadBreaker) RecordFailure(latency time.Duration) {
	b.inner.RecordFailure(latency)
}

func (b *BulkheadBreaker) State() State {
	return b.inner.State()
}

func (b *BulkheadBreaker) Reset() {
	b.inner.Reset()
}
