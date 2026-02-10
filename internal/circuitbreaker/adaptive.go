package circuitbreaker

import (
	"sync"
	"time"
)

// AdaptiveBreaker dynamically adjusts the failure-rate threshold of an inner
// FailureRateBreaker based on an exponentially weighted moving average (EWMA)
// of observed latencies. When latency rises above latencyCeiling, the threshold
// is tightened (lowered) so the breaker trips sooner, providing faster
// protection under degraded conditions.
type AdaptiveBreaker struct {
	mu    sync.Mutex
	inner *FailureRateBreaker

	ewmaLatency    float64       // EWMA of latency in nanoseconds
	alpha          float64       // smoothing factor (0 < alpha <= 1)
	baseThreshold  float64       // normal (relaxed) failure threshold
	minThreshold   float64       // tightest (most aggressive) threshold
	latencyCeiling time.Duration // latency above which threshold tightens
}

// NewAdaptiveBreaker wraps a FailureRateBreaker and adjusts its threshold
// dynamically. alpha controls EWMA responsiveness (higher = more reactive).
func NewAdaptiveBreaker(inner *FailureRateBreaker, baseThreshold, minThreshold float64, latencyCeiling time.Duration, alpha float64) *AdaptiveBreaker {
	return &AdaptiveBreaker{
		inner:          inner,
		alpha:          alpha,
		baseThreshold:  baseThreshold,
		minThreshold:   minThreshold,
		latencyCeiling: latencyCeiling,
	}
}

func (a *AdaptiveBreaker) Allow() bool {
	return a.inner.Allow()
}

func (a *AdaptiveBreaker) RecordSuccess(latency time.Duration) {
	a.inner.RecordSuccess(latency)
	a.updateThreshold(latency)
}

func (a *AdaptiveBreaker) RecordFailure(latency time.Duration) {
	a.inner.RecordFailure(latency)
	a.updateThreshold(latency)
}

func (a *AdaptiveBreaker) State() State {
	return a.inner.State()
}

func (a *AdaptiveBreaker) Reset() {
	a.inner.Reset()
	a.mu.Lock()
	a.ewmaLatency = 0
	a.inner.SetFailureThreshold(a.baseThreshold)
	a.mu.Unlock()
}

// updateThreshold recalculates the EWMA latency and adjusts the inner
// breaker's failure threshold accordingly.
func (a *AdaptiveBreaker) updateThreshold(latency time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()

	ns := float64(latency.Nanoseconds())
	if a.ewmaLatency == 0 {
		a.ewmaLatency = ns
	} else {
		a.ewmaLatency = a.alpha*ns + (1-a.alpha)*a.ewmaLatency
	}

	ceiling := float64(a.latencyCeiling.Nanoseconds())
	if a.ewmaLatency <= ceiling {
		// Latency normal — use base threshold.
		a.inner.SetFailureThreshold(a.baseThreshold)
		return
	}

	// Linearly interpolate: as EWMA goes from ceiling to 2*ceiling,
	// threshold goes from baseThreshold down to minThreshold.
	ratio := (a.ewmaLatency - ceiling) / ceiling
	if ratio > 1 {
		ratio = 1
	}
	threshold := a.baseThreshold - ratio*(a.baseThreshold-a.minThreshold)
	a.inner.SetFailureThreshold(threshold)
}
