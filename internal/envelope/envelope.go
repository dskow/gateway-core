package envelope

import (
	"context"
	"errors"
	"time"
)

// ErrFallback is the canonical error returned by the autonomous-safe default
// Envelope. It signals that no agent pipeline is configured and every
// proposal is therefore rejected by design.
var ErrFallback = errors.New("envelope: autonomous-safe fallback; no agent pipeline configured")

// Envelope is the entry point for all agent-produced configuration
// proposals. Until the full pipeline (constraints, bounds, dampener, shadow
// simulator) is implemented, the zero-value Envelope and the result of
// NewAutonomousSafe both reject every proposal. This is intentional and
// matches the pattern's autonomous-safe default — see docs/AGENTIC_ENVELOPE.md.
//
// Envelope is safe for concurrent use. Submit must not block on, depend on,
// or mutate the data path; callers may invoke it from any goroutine.
type Envelope struct {
	now func() time.Time
}

// NewAutonomousSafe returns an Envelope that rejects every proposal with
// ErrFallback. This is the only mode currently implemented; additional
// constructors will be introduced as the pipeline stages are built.
func NewAutonomousSafe() *Envelope {
	return &Envelope{now: time.Now}
}

// Submit hands a Proposal to the Envelope and returns the resulting
// Decision. The current implementation always returns DecisionReject with
// reason ErrFallback; future implementations will route the proposal
// through Constraints → Bounds → Dampener → Shadow before returning.
//
// Submit honors context cancellation: a cancelled context returns a
// DecisionDefer with the context's error as the reason and a RetryAfter of
// zero (the caller decides when to retry).
func (e *Envelope) Submit(ctx context.Context, p Proposal) Decision {
	now := e.timeNow()
	if err := ctx.Err(); err != nil {
		return Decision{
			Kind:      DecisionDefer,
			Stage:     "intake",
			Reason:    err.Error(),
			DecidedAt: now,
		}
	}
	return Decision{
		Kind:      DecisionReject,
		Stage:     "fallback",
		Reason:    ErrFallback.Error(),
		DecidedAt: now,
	}
}

func (e *Envelope) timeNow() time.Time {
	if e == nil || e.now == nil {
		return time.Now()
	}
	return e.now()
}
