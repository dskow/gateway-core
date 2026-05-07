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
	now         func() time.Time
	constraints *ConstraintRegistry
	bounds      *BoundsRegistry
	dampener    *DampenerRegistry
}

// Option configures an Envelope at construction time. Options are
// additive; each enables a single pipeline stage. Until every stage is
// available the Envelope still rejects unmatched proposals at the
// fallback stage — wiring up a stage never weakens the autonomous-safe
// contract, it only produces clearer rejection reasons.
type Option func(*Envelope)

// WithConstraints installs the immutable-constraints stage. Constraints
// run first in the pipeline; a violation short-circuits with stage
// "constraints" and the violation's structured reason. A nil registry
// disables the stage (equivalent to omitting the option).
func WithConstraints(r *ConstraintRegistry) Option {
	return func(e *Envelope) { e.constraints = r }
}

// WithBounds installs the bounded-deltas stage. Bounds run after
// constraints; a violation short-circuits with stage "bounds" and the
// violation's structured reason. A nil registry disables the stage
// (equivalent to omitting the option).
func WithBounds(r *BoundsRegistry) Option {
	return func(e *Envelope) { e.bounds = r }
}

// WithDampener installs the dampener stage. The dampener runs after
// bounds; a "cooldown_active" outcome short-circuits with stage
// "dampener" and DecisionDefer (with RetryAfter set), while a
// "below_hysteresis" outcome short-circuits with stage "dampener" and
// DecisionReject. A nil registry disables the stage (equivalent to
// omitting the option).
func WithDampener(r *DampenerRegistry) Option {
	return func(e *Envelope) { e.dampener = r }
}

// New returns an Envelope configured with the given options. With no
// options it is equivalent to NewAutonomousSafe: every proposal is
// rejected at the fallback stage.
func New(opts ...Option) *Envelope {
	e := &Envelope{now: time.Now}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	return e
}

// NewAutonomousSafe returns an Envelope that rejects every proposal with
// ErrFallback. It is equivalent to New() with no options and is preserved
// as a named constructor because the pattern's autonomous-safe default is
// itself a documented mode, not just an empty configuration.
func NewAutonomousSafe() *Envelope {
	return New()
}

// Submit hands a Proposal to the Envelope and returns the resulting
// Decision. The pipeline runs in fixed order:
//
//  1. context cancellation  → DecisionDefer, stage "intake"
//  2. constraints (if any)  → DecisionReject, stage "constraints"
//  3. bounds (if any)       → DecisionReject, stage "bounds"
//  4. dampener (if any)     → DecisionDefer (cooldown) or
//                              DecisionReject (hysteresis), stage "dampener"
//  5. fallback              → DecisionReject, stage "fallback"
//
// Later stages (shadow) will slot in between dampener and fallback as
// they are built.
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
	if e != nil && e.constraints != nil {
		if err := e.constraints.Evaluate(p); err != nil {
			return Decision{
				Kind:      DecisionReject,
				Stage:     "constraints",
				Reason:    err.Error(),
				DecidedAt: now,
			}
		}
	}
	if e != nil && e.bounds != nil {
		if err := e.bounds.Evaluate(p); err != nil {
			return Decision{
				Kind:      DecisionReject,
				Stage:     "bounds",
				Reason:    err.Error(),
				DecidedAt: now,
			}
		}
	}
	if e != nil && e.dampener != nil {
		if outcome := e.dampener.Evaluate(p, now); outcome != nil {
			kind := DecisionReject
			if outcome.Reason == "cooldown_active" {
				kind = DecisionDefer
			}
			return Decision{
				Kind:       kind,
				Stage:      "dampener",
				Reason:     outcome.Error(),
				RetryAfter: outcome.RetryAfter,
				DecidedAt:  now,
			}
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
