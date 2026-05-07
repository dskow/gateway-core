package envelope

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConstraintRegistry_EmptyRegistryAllowsEverything(t *testing.T) {
	t.Parallel()

	r := NewConstraintRegistry()
	if err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}); err != nil {
		t.Fatalf("empty registry must not reject, got %v", err)
	}
}

func TestConstraintRegistry_NilRegistryIsSafe(t *testing.T) {
	t.Parallel()

	var r *ConstraintRegistry
	if err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}); err != nil {
		t.Fatalf("nil registry must not reject, got %v", err)
	}
	r.Register(constraintFunc{name: "noop", eval: func(Proposal) error { return nil }})
	if names := r.Names(); names != nil {
		t.Fatalf("nil registry must ignore Register; Names() = %v", names)
	}
}

func TestConstraintRegistry_ShortCircuitsOnFirstViolation(t *testing.T) {
	t.Parallel()

	var calls []string
	a := constraintFunc{name: "a", eval: func(Proposal) error {
		calls = append(calls, "a")
		return &ConstraintViolation{Constraint: "a", Reason: "always_fails"}
	}}
	b := constraintFunc{name: "b", eval: func(Proposal) error {
		calls = append(calls, "b")
		return nil
	}}

	r := NewConstraintRegistry()
	r.Register(a)
	r.Register(b)

	err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	if err == nil {
		t.Fatal("expected violation, got nil")
	}
	var v *ConstraintViolation
	if !errors.As(err, &v) {
		t.Fatalf("expected *ConstraintViolation, got %T", err)
	}
	if v.Constraint != "a" || v.Reason != "always_fails" {
		t.Fatalf("unexpected violation: %+v", v)
	}
	if len(calls) != 1 || calls[0] != "a" {
		t.Fatalf("expected only first constraint to run, got calls=%v", calls)
	}
}

func TestConstraintRegistry_WrapsNonViolationErrors(t *testing.T) {
	t.Parallel()

	r := NewConstraintRegistry()
	r.Register(constraintFunc{name: "buggy", eval: func(Proposal) error {
		return errors.New("oops")
	}})

	err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	var v *ConstraintViolation
	if !errors.As(err, &v) {
		t.Fatalf("expected *ConstraintViolation, got %T", err)
	}
	if v.Constraint != "buggy" || v.Reason != "constraint_internal_error" {
		t.Fatalf("unexpected wrapped violation: %+v", v)
	}
	if !strings.Contains(v.Detail, "oops") {
		t.Fatalf("expected wrapped detail to include underlying error, got %q", v.Detail)
	}
}

func TestConstraintRegistry_NamesReturnsRegistrationOrder(t *testing.T) {
	t.Parallel()

	r := NewConstraintRegistry()
	r.Register(constraintFunc{name: "first"})
	r.Register(constraintFunc{name: "second"})
	r.Register(constraintFunc{name: "third"})

	got := r.Names()
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("Names() length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestConstraintViolation_ErrorFormat(t *testing.T) {
	t.Parallel()

	with := &ConstraintViolation{Constraint: "rate_limit.positive", Reason: "non_positive_value", Detail: "got -1"}
	if got := with.Error(); got != "rate_limit.positive: non_positive_value (got -1)" {
		t.Fatalf("unexpected formatted error: %q", got)
	}
	without := &ConstraintViolation{Constraint: "agent.required", Reason: "missing_agent"}
	if got := without.Error(); got != "agent.required: missing_agent" {
		t.Fatalf("unexpected formatted error: %q", got)
	}
	var nilv *ConstraintViolation
	if got := nilv.Error(); got == "" {
		t.Fatal("nil violation.Error() must not be empty")
	}
}

func TestDefaultConstraints_AcceptsValidProposals(t *testing.T) {
	t.Parallel()

	r := DefaultConstraints()

	cases := []Proposal{
		{Kind: KindRateLimit, Agent: "planner", Value: 150},
		{Kind: KindRateLimit, Agent: "planner", Value: int64(200)},
		{Kind: KindRouteWeight, Agent: "planner", Value: 0.25},
		{Kind: KindRouteWeight, Agent: "planner", Value: 1.0},
		{Kind: KindRouteWeight, Agent: "planner", Value: 0.0},
		{Kind: KindCacheTTL, Agent: "planner", Value: 30 * time.Second},
		{Kind: KindCacheTTL, Agent: "planner", Value: time.Duration(0)},
		{Kind: KindCircuitBreaker, Agent: "planner", Value: "open"},
	}
	for _, p := range cases {
		if err := r.Evaluate(p); err != nil {
			t.Errorf("expected %+v to pass default constraints, got %v", p, err)
		}
	}
}

func TestDefaultConstraints_RejectsUnknownKind(t *testing.T) {
	t.Parallel()

	r := DefaultConstraints()
	err := r.Evaluate(Proposal{Kind: "nonsense", Agent: "planner", Value: 1})
	v := mustViolation(t, err)
	if v.Constraint != "kind.known" || v.Reason != "unknown_kind" {
		t.Fatalf("unexpected violation: %+v", v)
	}
}

func TestDefaultConstraints_RejectsMissingAgent(t *testing.T) {
	t.Parallel()

	r := DefaultConstraints()
	cases := []string{"", "   ", "\t"}
	for _, agent := range cases {
		err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: agent, Value: 100})
		v := mustViolation(t, err)
		if v.Constraint != "agent.required" || v.Reason != "missing_agent" {
			t.Fatalf("agent=%q: unexpected violation: %+v", agent, v)
		}
	}
}

func TestDefaultConstraints_RejectsNonPositiveRateLimit(t *testing.T) {
	t.Parallel()

	r := DefaultConstraints()
	cases := []any{0, -1, int64(-100), 0.0}
	for _, val := range cases {
		err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: val})
		v := mustViolation(t, err)
		if v.Constraint != "rate_limit.positive" || v.Reason != "non_positive_value" {
			t.Fatalf("value=%v: unexpected violation: %+v", val, v)
		}
	}
}

func TestDefaultConstraints_RejectsNonIntegerRateLimit(t *testing.T) {
	t.Parallel()

	r := DefaultConstraints()
	cases := []any{"100", 1.5, []int{100}, nil}
	for _, val := range cases {
		err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: val})
		v := mustViolation(t, err)
		if v.Constraint != "rate_limit.positive" || v.Reason != "value_type_mismatch" {
			t.Fatalf("value=%v: unexpected violation: %+v", val, v)
		}
	}
}

func TestDefaultConstraints_RejectsRouteWeightOutOfRange(t *testing.T) {
	t.Parallel()

	r := DefaultConstraints()
	cases := []any{-0.001, 1.0001, 2.0, -1.0}
	for _, val := range cases {
		err := r.Evaluate(Proposal{Kind: KindRouteWeight, Agent: "x", Value: val})
		v := mustViolation(t, err)
		if v.Constraint != "route_weight.unit_interval" || v.Reason != "out_of_range" {
			t.Fatalf("value=%v: unexpected violation: %+v", val, v)
		}
	}
}

func TestDefaultConstraints_RejectsRouteWeightWrongType(t *testing.T) {
	t.Parallel()

	r := DefaultConstraints()
	err := r.Evaluate(Proposal{Kind: KindRouteWeight, Agent: "x", Value: "0.5"})
	v := mustViolation(t, err)
	if v.Constraint != "route_weight.unit_interval" || v.Reason != "value_type_mismatch" {
		t.Fatalf("unexpected violation: %+v", v)
	}
}

func TestDefaultConstraints_RejectsNegativeCacheTTL(t *testing.T) {
	t.Parallel()

	r := DefaultConstraints()
	err := r.Evaluate(Proposal{Kind: KindCacheTTL, Agent: "x", Value: -1 * time.Second})
	v := mustViolation(t, err)
	if v.Constraint != "cache_ttl.non_negative" || v.Reason != "negative_duration" {
		t.Fatalf("unexpected violation: %+v", v)
	}
}

func TestDefaultConstraints_RejectsCacheTTLWrongType(t *testing.T) {
	t.Parallel()

	r := DefaultConstraints()
	err := r.Evaluate(Proposal{Kind: KindCacheTTL, Agent: "x", Value: "30s"})
	v := mustViolation(t, err)
	if v.Constraint != "cache_ttl.non_negative" || v.Reason != "value_type_mismatch" {
		t.Fatalf("unexpected violation: %+v", v)
	}
}

func TestDefaultConstraints_NamesAreStable(t *testing.T) {
	t.Parallel()

	got := DefaultConstraints().Names()
	want := []string{
		"kind.known",
		"agent.required",
		"rate_limit.positive",
		"route_weight.unit_interval",
		"cache_ttl.non_negative",
	}
	if len(got) != len(want) {
		t.Fatalf("DefaultConstraints names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DefaultConstraints[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func mustViolation(t *testing.T, err error) *ConstraintViolation {
	t.Helper()
	if err == nil {
		t.Fatal("expected violation, got nil")
	}
	var v *ConstraintViolation
	if !errors.As(err, &v) {
		t.Fatalf("expected *ConstraintViolation, got %T: %v", err, err)
	}
	return v
}
