// Package circuitbreaker provides composable circuit breaker implementations
// for protecting the gateway against backend failures and load spikes.
package circuitbreaker

import "time"

// State represents the circuit breaker state.
type State int

const (
	StateClosed   State = iota // Normal operation; requests pass through.
	StateOpen                  // Failing; requests are rejected immediately.
	StateHalfOpen              // Probing; limited requests allowed to test recovery.
)

// String returns a human-readable state name.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Breaker is the common interface for all circuit breaker types.
type Breaker interface {
	// Allow reports whether a request may proceed. Returns false when the
	// circuit is open and the request should be rejected with 503.
	Allow() bool

	// RecordSuccess records a successful backend response with its latency.
	RecordSuccess(latency time.Duration)

	// RecordFailure records a failed backend response with its latency.
	RecordFailure(latency time.Duration)

	// State returns the current circuit breaker state.
	State() State

	// Reset forces the breaker back to closed state.
	Reset()
}
