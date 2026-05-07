package envelope

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Constraint is an operator-authored, immutable rule that every Proposal
// must satisfy before any later Envelope stage runs. Constraints are the
// constitutional layer of the pattern: no agent, no later stage, and no
// configuration overlay can relax them. The only way to change a
// Constraint is to ship new code through the same review process as any
// other gateway change.
//
// Constraint implementations must be pure: they take a Proposal and
// return either nil (allowed) or a *ConstraintViolation (denied). They
// must not mutate the Proposal, must not perform I/O, and must not
// depend on time or external state. A Constraint that needs any of those
// belongs in a later stage.
type Constraint interface {
	// Name is a stable identifier used in audit logs and decision reasons.
	// It must be a short, lower-case, dot-or-underscore-separated token
	// (for example, "rate_limit.positive" or "agent.required").
	Name() string

	// Evaluate returns nil if the proposal satisfies this constraint, or
	// a *ConstraintViolation otherwise. Returning a non-violation error
	// is reserved for genuine programmer errors and is treated by the
	// registry as a violation with reason "constraint_internal_error".
	Evaluate(p Proposal) error
}

// ConstraintViolation is the structured error returned by a Constraint
// that rejects a proposal. It carries the constraint's Name, a stable
// machine-readable Reason code, and an optional human-readable Detail
// for logs.
type ConstraintViolation struct {
	Constraint string
	Reason     string
	Detail     string
}

// Error implements the error interface. The format is stable and is
// what the Envelope places in Decision.Reason for a constraint
// rejection: "<constraint>: <reason>" optionally followed by detail.
func (v *ConstraintViolation) Error() string {
	if v == nil {
		return "envelope: nil constraint violation"
	}
	if v.Detail == "" {
		return fmt.Sprintf("%s: %s", v.Constraint, v.Reason)
	}
	return fmt.Sprintf("%s: %s (%s)", v.Constraint, v.Reason, v.Detail)
}

// constraintFunc is the function-typed adapter used by the package's
// baseline constraints. It is unexported because operators should
// register Constraints by name (and review them as code), not by
// passing in arbitrary closures from outside the package.
type constraintFunc struct {
	name string
	eval func(Proposal) error
}

func (c constraintFunc) Name() string                  { return c.name }
func (c constraintFunc) Evaluate(p Proposal) error     { return c.eval(p) }

// ConstraintRegistry holds an ordered list of Constraints. Proposals
// are evaluated against each constraint in registration order; the
// first violating constraint short-circuits the rest. Order matters
// only for the error returned (the first violation wins) — the set of
// proposals that pass is identical regardless of order.
//
// A registry is safe for concurrent reads once construction is
// complete. Register is not safe to call concurrently with Evaluate;
// build the registry at startup and then treat it as immutable.
type ConstraintRegistry struct {
	constraints []Constraint
}

// NewConstraintRegistry returns an empty registry. An empty registry
// is valid and trivially passes every proposal — the autonomous-safe
// default still rejects at the fallback stage.
func NewConstraintRegistry() *ConstraintRegistry {
	return &ConstraintRegistry{}
}

// Register appends c to the registry. Names need not be unique, but
// duplicates are noisy in audit logs and are discouraged.
func (r *ConstraintRegistry) Register(c Constraint) {
	if r == nil || c == nil {
		return
	}
	r.constraints = append(r.constraints, c)
}

// Names returns the registered constraint names in registration order.
// Useful for boot-time logging and tests.
func (r *ConstraintRegistry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.constraints))
	for _, c := range r.constraints {
		out = append(out, c.Name())
	}
	return out
}

// Evaluate runs every registered constraint against p, in order.
// Returns nil if all constraints pass, or the first *ConstraintViolation
// produced by a failing constraint. A constraint that returns a non-violation
// error is wrapped in a synthetic *ConstraintViolation with the name of
// the offending constraint and reason "constraint_internal_error" — this
// keeps the Envelope's rejection path uniform regardless of what a
// custom constraint returns.
func (r *ConstraintRegistry) Evaluate(p Proposal) error {
	if r == nil {
		return nil
	}
	for _, c := range r.constraints {
		err := c.Evaluate(p)
		if err == nil {
			continue
		}
		var v *ConstraintViolation
		if errors.As(err, &v) {
			return v
		}
		return &ConstraintViolation{
			Constraint: c.Name(),
			Reason:     "constraint_internal_error",
			Detail:     err.Error(),
		}
	}
	return nil
}

// DefaultConstraints returns the registry of baseline, operator-authored
// constraints that ship with the gateway. They encode well-formedness
// rules every proposal must satisfy regardless of which agent produced
// it; tighter, environment-specific constraints are layered on top by
// the operator.
//
// Current baseline:
//   - kind.known         — Proposal.Kind must be one of the recognized kinds.
//   - agent.required     — Proposal.Agent must be non-empty (audit lineage).
//   - rate_limit.positive       — KindRateLimit values must be positive integers.
//   - route_weight.unit_interval — KindRouteWeight values must be in [0.0, 1.0].
//   - cache_ttl.non_negative    — KindCacheTTL values must be non-negative durations.
//
// The list is intentionally conservative. Adding a constraint here is
// a code-review-gated change; relaxing one is, by design, never an
// agent-driven decision.
func DefaultConstraints() *ConstraintRegistry {
	r := NewConstraintRegistry()
	r.Register(constraintFunc{name: "kind.known", eval: requireKnownKind})
	r.Register(constraintFunc{name: "agent.required", eval: requireAgent})
	r.Register(constraintFunc{name: "rate_limit.positive", eval: rateLimitPositive})
	r.Register(constraintFunc{name: "route_weight.unit_interval", eval: routeWeightUnitInterval})
	r.Register(constraintFunc{name: "cache_ttl.non_negative", eval: cacheTTLNonNegative})
	return r
}

func requireKnownKind(p Proposal) error {
	switch p.Kind {
	case KindRateLimit, KindCircuitBreaker, KindRouteWeight, KindCacheTTL:
		return nil
	}
	return &ConstraintViolation{
		Constraint: "kind.known",
		Reason:     "unknown_kind",
		Detail:     fmt.Sprintf("kind=%q", p.Kind),
	}
}

func requireAgent(p Proposal) error {
	if strings.TrimSpace(p.Agent) != "" {
		return nil
	}
	return &ConstraintViolation{
		Constraint: "agent.required",
		Reason:     "missing_agent",
	}
}

func rateLimitPositive(p Proposal) error {
	if p.Kind != KindRateLimit {
		return nil
	}
	n, ok := asInt64(p.Value)
	if !ok {
		return &ConstraintViolation{
			Constraint: "rate_limit.positive",
			Reason:     "value_type_mismatch",
			Detail:     fmt.Sprintf("got %T, want integer", p.Value),
		}
	}
	if n <= 0 {
		return &ConstraintViolation{
			Constraint: "rate_limit.positive",
			Reason:     "non_positive_value",
			Detail:     fmt.Sprintf("got %d, want > 0", n),
		}
	}
	return nil
}

func routeWeightUnitInterval(p Proposal) error {
	if p.Kind != KindRouteWeight {
		return nil
	}
	f, ok := asFloat64(p.Value)
	if !ok {
		return &ConstraintViolation{
			Constraint: "route_weight.unit_interval",
			Reason:     "value_type_mismatch",
			Detail:     fmt.Sprintf("got %T, want float in [0,1]", p.Value),
		}
	}
	if f < 0.0 || f > 1.0 {
		return &ConstraintViolation{
			Constraint: "route_weight.unit_interval",
			Reason:     "out_of_range",
			Detail:     fmt.Sprintf("got %v, want value in [0,1]", f),
		}
	}
	return nil
}

func cacheTTLNonNegative(p Proposal) error {
	if p.Kind != KindCacheTTL {
		return nil
	}
	d, ok := asDuration(p.Value)
	if !ok {
		return &ConstraintViolation{
			Constraint: "cache_ttl.non_negative",
			Reason:     "value_type_mismatch",
			Detail:     fmt.Sprintf("got %T, want time.Duration", p.Value),
		}
	}
	if d < 0 {
		return &ConstraintViolation{
			Constraint: "cache_ttl.non_negative",
			Reason:     "negative_duration",
			Detail:     fmt.Sprintf("got %s", d),
		}
	}
	return nil
}

// asInt64 accepts the integer Go types most likely to arrive through
// JSON or YAML unmarshaling (int, int64, int32, and the float64 form
// that encoding/json uses for whole numbers when the destination is
// any). It returns false for non-integer floats and for non-numeric
// types so the constraint surfaces a structured type-mismatch instead
// of a silent truncation.
func asInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case uint:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		if x > 1<<62 {
			return 0, false
		}
		return int64(x), true
	case float64:
		if x != float64(int64(x)) {
			return 0, false
		}
		return int64(x), true
	}
	return 0, false
}

func asFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func asDuration(v any) (time.Duration, bool) {
	switch x := v.(type) {
	case time.Duration:
		return x, true
	case int64:
		return time.Duration(x), true
	case int:
		return time.Duration(x), true
	}
	return 0, false
}
