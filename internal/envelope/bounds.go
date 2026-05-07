package envelope

import (
	"fmt"
	"time"
)

// BoundsViolation is the structured error returned when a proposal
// fails the bounded-deltas stage. It mirrors *ConstraintViolation in
// shape so audit consumers can treat both stages uniformly.
type BoundsViolation struct {
	// Kind is the proposal kind that produced the violation.
	Kind ProposalKind

	// Target is the proposal target (route prefix, backend URL, ...).
	// Empty for global proposals.
	Target string

	// Reason is a stable machine-readable code: "below_minimum",
	// "above_maximum", or "value_type_mismatch".
	Reason string

	// Detail is an optional human-readable elaboration; safe to log,
	// not part of the stable contract.
	Detail string
}

// Error implements the error interface. The format is stable and is
// what the Envelope places in Decision.Reason for a bounds rejection:
// "bounds(<kind>): <reason>" optionally followed by detail.
func (v *BoundsViolation) Error() string {
	if v == nil {
		return "envelope: nil bounds violation"
	}
	if v.Detail == "" {
		return fmt.Sprintf("bounds(%s): %s", v.Kind, v.Reason)
	}
	return fmt.Sprintf("bounds(%s): %s (%s)", v.Kind, v.Reason, v.Detail)
}

// IntBound is an inclusive integer range used for KindRateLimit
// proposals. Either side may be left unset; a zero-value IntBound is
// "any int allowed" and is the autonomous-safe default for a Kind that
// has no operator-authored bound.
type IntBound struct {
	min, max       int64
	hasMin, hasMax bool
}

// IntRange returns an IntBound enforcing min <= value <= max.
func IntRange(min, max int64) IntBound {
	return IntBound{min: min, max: max, hasMin: true, hasMax: true}
}

// IntAtLeast returns an IntBound enforcing value >= min.
func IntAtLeast(min int64) IntBound { return IntBound{min: min, hasMin: true} }

// IntAtMost returns an IntBound enforcing value <= max.
func IntAtMost(max int64) IntBound { return IntBound{max: max, hasMax: true} }

// FloatBound is an inclusive float range used for KindRouteWeight
// proposals. Either side may be left unset.
type FloatBound struct {
	min, max       float64
	hasMin, hasMax bool
}

// FloatRange returns a FloatBound enforcing min <= value <= max.
func FloatRange(min, max float64) FloatBound {
	return FloatBound{min: min, max: max, hasMin: true, hasMax: true}
}

// FloatAtLeast returns a FloatBound enforcing value >= min.
func FloatAtLeast(min float64) FloatBound { return FloatBound{min: min, hasMin: true} }

// FloatAtMost returns a FloatBound enforcing value <= max.
func FloatAtMost(max float64) FloatBound { return FloatBound{max: max, hasMax: true} }

// DurationBound is an inclusive duration range used for KindCacheTTL
// proposals. Either side may be left unset.
type DurationBound struct {
	min, max       time.Duration
	hasMin, hasMax bool
}

// DurationRange returns a DurationBound enforcing min <= value <= max.
func DurationRange(min, max time.Duration) DurationBound {
	return DurationBound{min: min, max: max, hasMin: true, hasMax: true}
}

// DurationAtLeast returns a DurationBound enforcing value >= min.
func DurationAtLeast(min time.Duration) DurationBound {
	return DurationBound{min: min, hasMin: true}
}

// DurationAtMost returns a DurationBound enforcing value <= max.
func DurationAtMost(max time.Duration) DurationBound {
	return DurationBound{max: max, hasMax: true}
}

// BoundsRegistry holds the absolute value-range bounds enforced by the
// bounded-deltas stage, keyed by ProposalKind. Each kind has its own
// optional bound. A nil registry, or a registry with no bounds set,
// allows every proposal — the autonomous-safe default still rejects at
// the fallback stage.
//
// A registry is safe for concurrent reads once construction is
// complete. The Set* methods are not safe to call concurrently with
// Evaluate; build the registry at startup and then treat it as
// immutable.
//
// Bounds are deliberately separate from constraints: constraints are
// well-formedness rules every proposal must satisfy, owned by the
// gateway codebase; bounds are environment-specific operating ranges
// authored by operators per deployment.
type BoundsRegistry struct {
	rateLimit   *IntBound
	routeWeight *FloatBound
	cacheTTL    *DurationBound
}

// NewBoundsRegistry returns an empty registry. An empty registry is
// valid and trivially passes every proposal at this stage.
func NewBoundsRegistry() *BoundsRegistry { return &BoundsRegistry{} }

// SetRateLimit installs the bound for KindRateLimit proposals,
// replacing any previous bound for that kind.
func (r *BoundsRegistry) SetRateLimit(b IntBound) {
	if r == nil {
		return
	}
	r.rateLimit = &b
}

// SetRouteWeight installs the bound for KindRouteWeight proposals.
func (r *BoundsRegistry) SetRouteWeight(b FloatBound) {
	if r == nil {
		return
	}
	r.routeWeight = &b
}

// SetCacheTTL installs the bound for KindCacheTTL proposals.
func (r *BoundsRegistry) SetCacheTTL(b DurationBound) {
	if r == nil {
		return
	}
	r.cacheTTL = &b
}

// Evaluate returns nil if p satisfies the registered bounds for its
// Kind, or a *BoundsViolation otherwise. A proposal whose Kind has no
// registered bound passes through.
func (r *BoundsRegistry) Evaluate(p Proposal) error {
	if r == nil {
		return nil
	}
	switch p.Kind {
	case KindRateLimit:
		if r.rateLimit == nil {
			return nil
		}
		return checkIntBound(p, *r.rateLimit)
	case KindRouteWeight:
		if r.routeWeight == nil {
			return nil
		}
		return checkFloatBound(p, *r.routeWeight)
	case KindCacheTTL:
		if r.cacheTTL == nil {
			return nil
		}
		return checkDurationBound(p, *r.cacheTTL)
	}
	return nil
}

func checkIntBound(p Proposal, b IntBound) error {
	n, ok := asInt64(p.Value)
	if !ok {
		return &BoundsViolation{
			Kind:   p.Kind,
			Target: p.Target,
			Reason: "value_type_mismatch",
			Detail: fmt.Sprintf("got %T, want integer", p.Value),
		}
	}
	if b.hasMin && n < b.min {
		return &BoundsViolation{
			Kind:   p.Kind,
			Target: p.Target,
			Reason: "below_minimum",
			Detail: fmt.Sprintf("got %d, min %d", n, b.min),
		}
	}
	if b.hasMax && n > b.max {
		return &BoundsViolation{
			Kind:   p.Kind,
			Target: p.Target,
			Reason: "above_maximum",
			Detail: fmt.Sprintf("got %d, max %d", n, b.max),
		}
	}
	return nil
}

func checkFloatBound(p Proposal, b FloatBound) error {
	f, ok := asFloat64(p.Value)
	if !ok {
		return &BoundsViolation{
			Kind:   p.Kind,
			Target: p.Target,
			Reason: "value_type_mismatch",
			Detail: fmt.Sprintf("got %T, want float", p.Value),
		}
	}
	if b.hasMin && f < b.min {
		return &BoundsViolation{
			Kind:   p.Kind,
			Target: p.Target,
			Reason: "below_minimum",
			Detail: fmt.Sprintf("got %v, min %v", f, b.min),
		}
	}
	if b.hasMax && f > b.max {
		return &BoundsViolation{
			Kind:   p.Kind,
			Target: p.Target,
			Reason: "above_maximum",
			Detail: fmt.Sprintf("got %v, max %v", f, b.max),
		}
	}
	return nil
}

func checkDurationBound(p Proposal, b DurationBound) error {
	d, ok := asDuration(p.Value)
	if !ok {
		return &BoundsViolation{
			Kind:   p.Kind,
			Target: p.Target,
			Reason: "value_type_mismatch",
			Detail: fmt.Sprintf("got %T, want time.Duration", p.Value),
		}
	}
	if b.hasMin && d < b.min {
		return &BoundsViolation{
			Kind:   p.Kind,
			Target: p.Target,
			Reason: "below_minimum",
			Detail: fmt.Sprintf("got %s, min %s", d, b.min),
		}
	}
	if b.hasMax && d > b.max {
		return &BoundsViolation{
			Kind:   p.Kind,
			Target: p.Target,
			Reason: "above_maximum",
			Detail: fmt.Sprintf("got %s, max %s", d, b.max),
		}
	}
	return nil
}
