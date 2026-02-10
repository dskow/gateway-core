package circuitbreaker

import "time"

// TimeoutBreaker wraps another Breaker and treats slow responses as failures.
// If a response completes successfully but its latency exceeds slowThreshold,
// the success is recorded as a failure on the inner breaker.
type TimeoutBreaker struct {
	inner         Breaker
	slowThreshold time.Duration
}

// NewTimeoutBreaker wraps inner and converts successes slower than threshold
// into failures.
func NewTimeoutBreaker(inner Breaker, slowThreshold time.Duration) *TimeoutBreaker {
	return &TimeoutBreaker{inner: inner, slowThreshold: slowThreshold}
}

func (t *TimeoutBreaker) Allow() bool {
	return t.inner.Allow()
}

func (t *TimeoutBreaker) RecordSuccess(latency time.Duration) {
	if latency > t.slowThreshold {
		t.inner.RecordFailure(latency)
		return
	}
	t.inner.RecordSuccess(latency)
}

func (t *TimeoutBreaker) RecordFailure(latency time.Duration) {
	t.inner.RecordFailure(latency)
}

func (t *TimeoutBreaker) State() State {
	return t.inner.State()
}

func (t *TimeoutBreaker) Reset() {
	t.inner.Reset()
}
