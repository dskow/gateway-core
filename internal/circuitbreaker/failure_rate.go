package circuitbreaker

import (
	"log/slog"
	"sync"
	"time"

	"github.com/dskow/go-api-gateway/internal/metrics"
)

// outcome records a single request result in the sliding window.
type outcome struct {
	failed bool
}

// FailureRateBreaker implements a sliding-window failure-rate circuit breaker.
// It opens when the failure ratio over the most recent windowSize outcomes
// exceeds failureThreshold.
type FailureRateBreaker struct {
	mu sync.Mutex

	state   State
	backend string
	logger  *slog.Logger

	// Sliding window implemented as a ring buffer.
	window   []outcome
	head     int // next write position
	count    int // number of outcomes recorded (up to windowSize)
	failures int // number of failures in the current window

	windowSize       int
	failureThreshold float64
	resetTimeout     time.Duration
	halfOpenMax      int

	halfOpenSuccess int
	openedAt        time.Time
}

// NewFailureRateBreaker creates a failure-rate circuit breaker for the given backend.
func NewFailureRateBreaker(backend string, windowSize int, failureThreshold float64, resetTimeout time.Duration, halfOpenMax int, logger *slog.Logger) *FailureRateBreaker {
	return &FailureRateBreaker{
		state:            StateClosed,
		backend:          backend,
		logger:           logger,
		window:           make([]outcome, windowSize),
		windowSize:       windowSize,
		failureThreshold: failureThreshold,
		resetTimeout:     resetTimeout,
		halfOpenMax:      halfOpenMax,
	}
}

func (b *FailureRateBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(b.openedAt) >= b.resetTimeout {
			b.transitionTo(StateHalfOpen)
			return true
		}
		return false
	case StateHalfOpen:
		return true
	default:
		return true
	}
}

func (b *FailureRateBreaker) RecordSuccess(_ time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		b.recordOutcome(false)
	case StateHalfOpen:
		b.halfOpenSuccess++
		if b.halfOpenSuccess >= b.halfOpenMax {
			b.transitionTo(StateClosed)
		}
	}
}

func (b *FailureRateBreaker) RecordFailure(_ time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		b.recordOutcome(true)
		if b.count >= b.windowSize && b.failureRate() >= b.failureThreshold {
			b.transitionTo(StateOpen)
		}
	case StateHalfOpen:
		b.transitionTo(StateOpen)
	}
}

func (b *FailureRateBreaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *FailureRateBreaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transitionTo(StateClosed)
}

// SetFailureThreshold dynamically updates the failure threshold. Used by the
// adaptive breaker to tighten or relax the threshold at runtime.
func (b *FailureRateBreaker) SetFailureThreshold(t float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failureThreshold = t
}

// recordOutcome writes a result into the ring buffer and maintains the
// running failure count. Must be called with b.mu held.
func (b *FailureRateBreaker) recordOutcome(failed bool) {
	// If the window is full, evict the oldest entry.
	if b.count == b.windowSize {
		if b.window[b.head].failed {
			b.failures--
		}
	} else {
		b.count++
	}

	b.window[b.head] = outcome{failed: failed}
	if failed {
		b.failures++
	}
	b.head = (b.head + 1) % b.windowSize
}

// failureRate returns the current failure ratio. Must be called with b.mu held.
func (b *FailureRateBreaker) failureRate() float64 {
	if b.count == 0 {
		return 0
	}
	return float64(b.failures) / float64(b.count)
}

// transitionTo changes the breaker state, emitting metrics and logging.
// Must be called with b.mu held.
func (b *FailureRateBreaker) transitionTo(newState State) {
	if b.state == newState {
		return
	}

	from := b.state
	b.state = newState

	metrics.CircuitBreakerStateChanges.WithLabelValues(b.backend, from.String(), newState.String()).Inc()
	metrics.CircuitBreakerState.WithLabelValues(b.backend).Set(float64(newState))

	b.logger.Info("circuit breaker state change",
		"backend", b.backend,
		"from", from.String(),
		"to", newState.String(),
	)

	switch newState {
	case StateClosed:
		// Reset window and half-open counters.
		b.head = 0
		b.count = 0
		b.failures = 0
		b.halfOpenSuccess = 0
	case StateOpen:
		b.openedAt = time.Now()
		b.halfOpenSuccess = 0
	case StateHalfOpen:
		b.halfOpenSuccess = 0
	}
}
