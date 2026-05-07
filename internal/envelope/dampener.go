package envelope

import (
	"fmt"
	"sync"
	"time"
)

// DampenerOutcome is the structured result returned when a proposal is
// stopped by the dampener stage. It mirrors *ConstraintViolation and
// *BoundsViolation in shape so audit consumers can treat the three
// stages uniformly. A nil outcome means the proposal passed.
//
// The dampener emits two distinct outcomes:
//
//   - cooldown_active — the proposal targets a (Kind, Target) whose last
//     applied change is still inside the cooldown window. The proposal is
//     not wrong, only untimely; the Envelope translates this into a
//     DecisionDefer with a precise RetryAfter.
//   - below_hysteresis — the proposal differs from the last applied
//     value by less than the configured hysteresis band. The proposal is
//     not just untimely but signal-too-small; the Envelope translates
//     this into a DecisionReject because resubmitting the same value
//     would fail again. The agent must observe a larger change before
//     resubmitting.
type DampenerOutcome struct {
	// Kind is the proposal kind that produced the outcome.
	Kind ProposalKind

	// Target is the proposal target. Empty for global proposals.
	Target string

	// Reason is a stable machine-readable code: "cooldown_active" or
	// "below_hysteresis".
	Reason string

	// Detail is an optional human-readable elaboration; safe to log,
	// not part of the stable contract.
	Detail string

	// RetryAfter is meaningful only for "cooldown_active"; it is the
	// minimum duration the agent must wait before resubmitting.
	RetryAfter time.Duration
}

// Error implements the error interface. The format is stable and is
// what the Envelope places in Decision.Reason for a dampener outcome:
// "dampener(<kind>): <reason>" optionally followed by detail.
func (o *DampenerOutcome) Error() string {
	if o == nil {
		return "envelope: nil dampener outcome"
	}
	if o.Detail == "" {
		return fmt.Sprintf("dampener(%s): %s", o.Kind, o.Reason)
	}
	return fmt.Sprintf("dampener(%s): %s (%s)", o.Kind, o.Reason, o.Detail)
}

// IntDampener combines cooldown and integer hysteresis for KindRateLimit
// proposals. A zero-value IntDampener disables both checks; either field
// may also be left zero independently.
type IntDampener struct {
	// Cooldown is the minimum interval between successive applied
	// changes for a given (Kind, Target). Zero disables the check.
	Cooldown time.Duration

	// Hysteresis is the minimum absolute difference a new value must
	// have from the last applied value. Zero disables the check.
	Hysteresis int64
}

// FloatDampener is the float-typed counterpart for KindRouteWeight.
type FloatDampener struct {
	Cooldown   time.Duration
	Hysteresis float64
}

// DurationDampener is the duration-typed counterpart for KindCacheTTL.
type DurationDampener struct {
	Cooldown   time.Duration
	Hysteresis time.Duration
}

// DampenerRegistry holds the per-Kind dampener policies and the
// per-(Kind, Target) state of recently applied changes. It enforces
// hysteresis and cooldown — the third stage of the Envelope pipeline.
//
// The registry is safe for concurrent use. Unlike ConstraintRegistry
// and BoundsRegistry, the dampener is stateful: the Envelope calls
// RecordApplied after a proposal reaches DecisionApply, and Evaluate
// reads that state on the next proposal for the same (Kind, Target).
//
// The hysteresis check is the symmetric "minimum step" formulation:
// a proposal passes when |new - last| >= H. Asymmetric, direction-aware
// hysteresis (different bands for increases and decreases) is a
// possible future extension; the current form is the more conservative
// of the two and matches the pattern's bias toward operator-authored
// safety.
type DampenerRegistry struct {
	mu          sync.Mutex
	rateLimit   *IntDampener
	routeWeight *FloatDampener
	cacheTTL    *DurationDampener
	state       map[dampenerKey]dampenerState
}

type dampenerKey struct {
	Kind   ProposalKind
	Target string
}

type dampenerState struct {
	Value     any
	AppliedAt time.Time
}

// NewDampenerRegistry returns an empty registry. An empty registry is
// valid and trivially passes every proposal at this stage — the
// autonomous-safe default still rejects at the fallback stage.
func NewDampenerRegistry() *DampenerRegistry {
	return &DampenerRegistry{state: make(map[dampenerKey]dampenerState)}
}

// SetRateLimit installs the dampener policy for KindRateLimit
// proposals, replacing any previous policy for that kind.
func (r *DampenerRegistry) SetRateLimit(d IntDampener) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rateLimit = &d
}

// SetRouteWeight installs the dampener policy for KindRouteWeight.
func (r *DampenerRegistry) SetRouteWeight(d FloatDampener) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routeWeight = &d
}

// SetCacheTTL installs the dampener policy for KindCacheTTL.
func (r *DampenerRegistry) SetCacheTTL(d DurationDampener) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cacheTTL = &d
}

// Evaluate returns nil if p satisfies the registered dampener policy
// for its (Kind, Target), or a *DampenerOutcome otherwise. A proposal
// whose Kind has no registered policy passes through, as does the
// first proposal seen for any (Kind, Target) — the dampener has no
// prior state to compare against.
//
// now is the timestamp the Envelope captured at intake; passing it
// rather than calling time.Now keeps the entire Submit call coherent
// against a single clock reading.
func (r *DampenerRegistry) Evaluate(p Proposal, now time.Time) *DampenerOutcome {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	cooldown, ok := r.cooldownFor(p.Kind)
	if !ok {
		return nil
	}

	prev, exists := r.state[dampenerKey{Kind: p.Kind, Target: p.Target}]
	if !exists {
		return nil
	}

	if cooldown > 0 {
		elapsed := now.Sub(prev.AppliedAt)
		if elapsed < cooldown {
			remaining := cooldown - elapsed
			return &DampenerOutcome{
				Kind:       p.Kind,
				Target:     p.Target,
				Reason:     "cooldown_active",
				Detail:     fmt.Sprintf("%s remaining", remaining),
				RetryAfter: remaining,
			}
		}
	}

	if outcome := r.checkHysteresis(p, prev); outcome != nil {
		return outcome
	}
	return nil
}

// cooldownFor reports the configured cooldown for kind and whether any
// policy is registered for that kind at all. Caller must hold r.mu.
func (r *DampenerRegistry) cooldownFor(kind ProposalKind) (time.Duration, bool) {
	switch kind {
	case KindRateLimit:
		if r.rateLimit == nil {
			return 0, false
		}
		return r.rateLimit.Cooldown, true
	case KindRouteWeight:
		if r.routeWeight == nil {
			return 0, false
		}
		return r.routeWeight.Cooldown, true
	case KindCacheTTL:
		if r.cacheTTL == nil {
			return 0, false
		}
		return r.cacheTTL.Cooldown, true
	}
	return 0, false
}

// checkHysteresis applies the per-Kind hysteresis band. Caller must
// hold r.mu. Returns nil if the band is disabled (zero) or the
// proposal clears it.
func (r *DampenerRegistry) checkHysteresis(p Proposal, prev dampenerState) *DampenerOutcome {
	switch p.Kind {
	case KindRateLimit:
		if r.rateLimit == nil || r.rateLimit.Hysteresis <= 0 {
			return nil
		}
		a, ok1 := asInt64(prev.Value)
		b, ok2 := asInt64(p.Value)
		if !ok1 || !ok2 {
			return nil
		}
		diff := a - b
		if diff < 0 {
			diff = -diff
		}
		if diff < r.rateLimit.Hysteresis {
			return &DampenerOutcome{
				Kind:   p.Kind,
				Target: p.Target,
				Reason: "below_hysteresis",
				Detail: fmt.Sprintf("|%d - %d| = %d < %d", b, a, diff, r.rateLimit.Hysteresis),
			}
		}
	case KindRouteWeight:
		if r.routeWeight == nil || r.routeWeight.Hysteresis <= 0 {
			return nil
		}
		a, ok1 := asFloat64(prev.Value)
		b, ok2 := asFloat64(p.Value)
		if !ok1 || !ok2 {
			return nil
		}
		diff := a - b
		if diff < 0 {
			diff = -diff
		}
		if diff < r.routeWeight.Hysteresis {
			return &DampenerOutcome{
				Kind:   p.Kind,
				Target: p.Target,
				Reason: "below_hysteresis",
				Detail: fmt.Sprintf("|%v - %v| = %v < %v", b, a, diff, r.routeWeight.Hysteresis),
			}
		}
	case KindCacheTTL:
		if r.cacheTTL == nil || r.cacheTTL.Hysteresis <= 0 {
			return nil
		}
		a, ok1 := asDuration(prev.Value)
		b, ok2 := asDuration(p.Value)
		if !ok1 || !ok2 {
			return nil
		}
		diff := a - b
		if diff < 0 {
			diff = -diff
		}
		if diff < r.cacheTTL.Hysteresis {
			return &DampenerOutcome{
				Kind:   p.Kind,
				Target: p.Target,
				Reason: "below_hysteresis",
				Detail: fmt.Sprintf("|%s - %s| = %s < %s", b, a, diff, r.cacheTTL.Hysteresis),
			}
		}
	}
	return nil
}

// RecordApplied stores p as the most recent applied change for its
// (Kind, Target). The Envelope calls this after a proposal reaches
// DecisionApply; tests may call it directly to set up state.
//
// Calls for kinds that have no registered policy are recorded anyway:
// the cost is one map entry, and recording unconditionally means a
// later SetX that turns the policy on starts from a correct baseline.
func (r *DampenerRegistry) RecordApplied(p Proposal, at time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		r.state = make(map[dampenerKey]dampenerState)
	}
	r.state[dampenerKey{Kind: p.Kind, Target: p.Target}] = dampenerState{
		Value:     p.Value,
		AppliedAt: at,
	}
}
