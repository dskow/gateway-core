package envelope

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBoundsRegistry_NilIsSafe(t *testing.T) {
	t.Parallel()

	var r *BoundsRegistry
	if err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}); err != nil {
		t.Fatalf("nil registry must not reject, got %v", err)
	}
	// Set* on a nil receiver must be a safe no-op.
	r.SetRateLimit(IntRange(10, 20))
	r.SetRouteWeight(FloatRange(0, 1))
	r.SetCacheTTL(DurationRange(0, time.Second))
}

func TestBoundsRegistry_EmptyAllowsEverything(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	cases := []Proposal{
		{Kind: KindRateLimit, Agent: "x", Value: 100},
		{Kind: KindRouteWeight, Agent: "x", Value: 0.5},
		{Kind: KindCacheTTL, Agent: "x", Value: 30 * time.Second},
		{Kind: KindCircuitBreaker, Agent: "x", Value: "open"},
	}
	for _, p := range cases {
		if err := r.Evaluate(p); err != nil {
			t.Errorf("empty registry must allow %+v, got %v", p, err)
		}
	}
}

func TestBoundsRegistry_RateLimit_WithinRangeAllowed(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetRateLimit(IntRange(10, 10000))
	cases := []any{10, 10000, int64(500), int32(100), 200.0}
	for _, val := range cases {
		err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: val})
		if err != nil {
			t.Errorf("value=%v: expected pass, got %v", val, err)
		}
	}
}

func TestBoundsRegistry_RateLimit_BelowMinimumRejected(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetRateLimit(IntRange(10, 10000))
	err := r.Evaluate(Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 5})
	v := mustBoundsViolation(t, err)
	if v.Reason != "below_minimum" {
		t.Fatalf("expected reason=below_minimum, got %+v", v)
	}
	if v.Kind != KindRateLimit || v.Target != "/api/users" {
		t.Fatalf("violation must echo Kind and Target, got %+v", v)
	}
	if !strings.Contains(v.Detail, "min 10") {
		t.Fatalf("detail must include the configured minimum, got %q", v.Detail)
	}
}

func TestBoundsRegistry_RateLimit_AboveMaximumRejected(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetRateLimit(IntRange(10, 10000))
	err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 100000})
	v := mustBoundsViolation(t, err)
	if v.Reason != "above_maximum" {
		t.Fatalf("expected reason=above_maximum, got %+v", v)
	}
	if !strings.Contains(v.Detail, "max 10000") {
		t.Fatalf("detail must include the configured maximum, got %q", v.Detail)
	}
}

func TestBoundsRegistry_RateLimit_OnlyMin(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetRateLimit(IntAtLeast(50))

	if err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 1_000_000}); err != nil {
		t.Fatalf("no upper bound must allow large values, got %v", err)
	}
	err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 49})
	v := mustBoundsViolation(t, err)
	if v.Reason != "below_minimum" {
		t.Fatalf("expected below_minimum, got %+v", v)
	}
}

func TestBoundsRegistry_RateLimit_OnlyMax(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetRateLimit(IntAtMost(500))

	if err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 1}); err != nil {
		t.Fatalf("no lower bound must allow small positive values, got %v", err)
	}
	err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 501})
	v := mustBoundsViolation(t, err)
	if v.Reason != "above_maximum" {
		t.Fatalf("expected above_maximum, got %+v", v)
	}
}

func TestBoundsRegistry_RateLimit_TypeMismatch(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetRateLimit(IntRange(10, 10000))
	cases := []any{"100", 1.5, []int{100}, nil}
	for _, val := range cases {
		err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: val})
		v := mustBoundsViolation(t, err)
		if v.Reason != "value_type_mismatch" {
			t.Fatalf("value=%v: expected value_type_mismatch, got %+v", val, v)
		}
	}
}

func TestBoundsRegistry_RouteWeight_RangeChecks(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetRouteWeight(FloatRange(0.1, 0.9))

	if err := r.Evaluate(Proposal{Kind: KindRouteWeight, Agent: "x", Value: 0.5}); err != nil {
		t.Fatalf("0.5 must be within [0.1, 0.9], got %v", err)
	}

	below := r.Evaluate(Proposal{Kind: KindRouteWeight, Agent: "x", Value: 0.05})
	if v := mustBoundsViolation(t, below); v.Reason != "below_minimum" {
		t.Fatalf("0.05 expected below_minimum, got %+v", v)
	}

	above := r.Evaluate(Proposal{Kind: KindRouteWeight, Agent: "x", Value: 1.0})
	if v := mustBoundsViolation(t, above); v.Reason != "above_maximum" {
		t.Fatalf("1.0 expected above_maximum, got %+v", v)
	}
}

func TestBoundsRegistry_RouteWeight_TypeMismatch(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetRouteWeight(FloatRange(0, 1))
	err := r.Evaluate(Proposal{Kind: KindRouteWeight, Agent: "x", Value: "0.5"})
	v := mustBoundsViolation(t, err)
	if v.Reason != "value_type_mismatch" {
		t.Fatalf("expected value_type_mismatch, got %+v", v)
	}
}

func TestBoundsRegistry_CacheTTL_RangeChecks(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetCacheTTL(DurationRange(time.Second, time.Hour))

	if err := r.Evaluate(Proposal{Kind: KindCacheTTL, Agent: "x", Value: time.Minute}); err != nil {
		t.Fatalf("1m must be within [1s, 1h], got %v", err)
	}

	below := r.Evaluate(Proposal{Kind: KindCacheTTL, Agent: "x", Value: time.Millisecond})
	if v := mustBoundsViolation(t, below); v.Reason != "below_minimum" {
		t.Fatalf("1ms expected below_minimum, got %+v", v)
	}

	above := r.Evaluate(Proposal{Kind: KindCacheTTL, Agent: "x", Value: 24 * time.Hour})
	if v := mustBoundsViolation(t, above); v.Reason != "above_maximum" {
		t.Fatalf("24h expected above_maximum, got %+v", v)
	}
}

func TestBoundsRegistry_CacheTTL_TypeMismatch(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetCacheTTL(DurationAtMost(time.Hour))
	err := r.Evaluate(Proposal{Kind: KindCacheTTL, Agent: "x", Value: "30s"})
	v := mustBoundsViolation(t, err)
	if v.Reason != "value_type_mismatch" {
		t.Fatalf("expected value_type_mismatch, got %+v", v)
	}
}

func TestBoundsRegistry_KindWithNoBoundIsAllowed(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetRateLimit(IntRange(10, 100))

	// RouteWeight has no bound configured, so any value passes this stage.
	if err := r.Evaluate(Proposal{Kind: KindRouteWeight, Agent: "x", Value: 99.0}); err != nil {
		t.Fatalf("kind without bound must pass, got %v", err)
	}
	// CircuitBreaker has no bound configured.
	if err := r.Evaluate(Proposal{Kind: KindCircuitBreaker, Agent: "x", Value: "open"}); err != nil {
		t.Fatalf("kind without bound must pass, got %v", err)
	}
}

func TestBoundsRegistry_SetReplacesPriorBound(t *testing.T) {
	t.Parallel()

	r := NewBoundsRegistry()
	r.SetRateLimit(IntRange(10, 100))
	r.SetRateLimit(IntRange(1000, 2000)) // replaces the prior bound

	if err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 50}); err == nil {
		t.Fatal("50 must now be below_minimum after the replacement bound")
	}
	if err := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 1500}); err != nil {
		t.Fatalf("1500 must be within the replacement bound, got %v", err)
	}
}

func TestBoundsViolation_ErrorFormat(t *testing.T) {
	t.Parallel()

	with := &BoundsViolation{Kind: KindRateLimit, Reason: "below_minimum", Detail: "got 5, min 10"}
	if got := with.Error(); got != "bounds(rate_limit): below_minimum (got 5, min 10)" {
		t.Fatalf("unexpected formatted error: %q", got)
	}
	without := &BoundsViolation{Kind: KindCacheTTL, Reason: "above_maximum"}
	if got := without.Error(); got != "bounds(cache_ttl): above_maximum" {
		t.Fatalf("unexpected formatted error: %q", got)
	}
	var nilv *BoundsViolation
	if got := nilv.Error(); got == "" {
		t.Fatal("nil violation.Error() must not be empty")
	}
}

func mustBoundsViolation(t *testing.T, err error) *BoundsViolation {
	t.Helper()
	if err == nil {
		t.Fatal("expected violation, got nil")
	}
	var v *BoundsViolation
	if !errors.As(err, &v) {
		t.Fatalf("expected *BoundsViolation, got %T: %v", err, err)
	}
	return v
}
