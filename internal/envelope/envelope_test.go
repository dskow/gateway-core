package envelope

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAutonomousSafeRejectsByDefault(t *testing.T) {
	t.Parallel()

	e := NewAutonomousSafe()
	d := e.Submit(context.Background(), Proposal{
		Kind:   KindRateLimit,
		Target: "/api/users",
		Value:  150,
		Agent:  "test-agent",
		Reason: "load looks fine",
	})

	if d.Kind != DecisionReject {
		t.Fatalf("autonomous-safe envelope must reject, got %s", d.Kind)
	}
	if d.Stage != "fallback" {
		t.Fatalf("expected stage=fallback, got %q", d.Stage)
	}
	if !strings.Contains(d.Reason, "autonomous-safe") {
		t.Fatalf("decision reason must reference fallback mode, got %q", d.Reason)
	}
	if d.DecidedAt.IsZero() {
		t.Fatal("DecidedAt must be set on every Decision")
	}
}

func TestSubmitHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	e := NewAutonomousSafe()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := e.Submit(ctx, Proposal{Kind: KindRateLimit, Agent: "x"})
	if d.Kind != DecisionDefer {
		t.Fatalf("cancelled context must produce DecisionDefer, got %s", d.Kind)
	}
	if d.Stage != "intake" {
		t.Fatalf("expected stage=intake on cancellation, got %q", d.Stage)
	}
}

func TestZeroValueEnvelopeIsUsable(t *testing.T) {
	t.Parallel()

	// The pattern's contract: the gateway runs identically with the Envelope
	// present or absent. A zero-value *Envelope must therefore not panic.
	var e Envelope
	d := e.Submit(context.Background(), Proposal{Kind: KindCacheTTL, Agent: "x"})
	if d.Kind != DecisionReject {
		t.Fatalf("zero-value envelope must reject, got %s", d.Kind)
	}
}

func TestDecisionKindString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		k    DecisionKind
		want string
	}{
		{DecisionReject, "reject"},
		{DecisionDefer, "defer"},
		{DecisionApply, "apply"},
		{DecisionKind(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("DecisionKind(%d).String() = %q, want %q", c.k, got, c.want)
		}
	}
}

func TestSubmitStampsDecidedAt(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	e := &Envelope{now: func() time.Time { return fixed }}

	d := e.Submit(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x"})
	if !d.DecidedAt.Equal(fixed) {
		t.Fatalf("DecidedAt = %v, want %v", d.DecidedAt, fixed)
	}
}

func TestNewWithNoOptionsMatchesAutonomousSafe(t *testing.T) {
	t.Parallel()

	e := New()
	d := e.Submit(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	if d.Kind != DecisionReject || d.Stage != "fallback" {
		t.Fatalf("New() with no options must reject at fallback; got kind=%s stage=%q", d.Kind, d.Stage)
	}
}

func TestSubmitRunsConstraintsBeforeFallback(t *testing.T) {
	t.Parallel()

	r := NewConstraintRegistry()
	r.Register(constraintFunc{
		name: "test.always_fails",
		eval: func(Proposal) error {
			return &ConstraintViolation{Constraint: "test.always_fails", Reason: "by_design"}
		},
	})

	e := New(WithConstraints(r))
	d := e.Submit(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	if d.Kind != DecisionReject {
		t.Fatalf("expected reject, got %s", d.Kind)
	}
	if d.Stage != "constraints" {
		t.Fatalf("expected stage=constraints, got %q", d.Stage)
	}
	if !strings.Contains(d.Reason, "test.always_fails") || !strings.Contains(d.Reason, "by_design") {
		t.Fatalf("reason must include constraint name and code, got %q", d.Reason)
	}
	if d.DecidedAt.IsZero() {
		t.Fatal("DecidedAt must be set on every Decision")
	}
}

func TestSubmitFallsThroughWhenConstraintsPass(t *testing.T) {
	t.Parallel()

	r := NewConstraintRegistry()
	r.Register(constraintFunc{
		name: "test.always_passes",
		eval: func(Proposal) error { return nil },
	})

	e := New(WithConstraints(r))
	d := e.Submit(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	if d.Kind != DecisionReject {
		t.Fatalf("expected reject (pipeline still incomplete), got %s", d.Kind)
	}
	if d.Stage != "fallback" {
		t.Fatalf("a passing constraint must let the proposal reach fallback; got stage=%q", d.Stage)
	}
}

func TestSubmitConstraintsRunAfterContextCheck(t *testing.T) {
	t.Parallel()

	called := false
	r := NewConstraintRegistry()
	r.Register(constraintFunc{
		name: "test.records_call",
		eval: func(Proposal) error { called = true; return nil },
	})

	e := New(WithConstraints(r))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := e.Submit(ctx, Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	if d.Kind != DecisionDefer || d.Stage != "intake" {
		t.Fatalf("cancelled context must defer at intake; got kind=%s stage=%q", d.Kind, d.Stage)
	}
	if called {
		t.Fatal("constraints must not run when the context is already cancelled")
	}
}

func TestNewIgnoresNilOption(t *testing.T) {
	t.Parallel()

	e := New(nil)
	d := e.Submit(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	if d.Kind != DecisionReject || d.Stage != "fallback" {
		t.Fatalf("nil option must be a no-op; got kind=%s stage=%q", d.Kind, d.Stage)
	}
}

func TestSubmitRunsBoundsAfterConstraints(t *testing.T) {
	t.Parallel()

	br := NewBoundsRegistry()
	br.SetRateLimit(IntRange(10, 100))

	e := New(WithConstraints(DefaultConstraints()), WithBounds(br))
	d := e.Submit(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 500})
	if d.Kind != DecisionReject {
		t.Fatalf("expected reject, got %s", d.Kind)
	}
	if d.Stage != "bounds" {
		t.Fatalf("expected stage=bounds, got %q", d.Stage)
	}
	if !strings.Contains(d.Reason, "above_maximum") {
		t.Fatalf("reason must include the bounds violation code, got %q", d.Reason)
	}
}

func TestSubmitConstraintFailureSkipsBounds(t *testing.T) {
	t.Parallel()

	br := NewBoundsRegistry()
	br.SetRateLimit(IntRange(10, 100))

	cr := NewConstraintRegistry()
	cr.Register(constraintFunc{
		name: "test.always_fails",
		eval: func(Proposal) error {
			return &ConstraintViolation{Constraint: "test.always_fails", Reason: "by_design"}
		},
	})

	// Value 500 violates both constraints (synthetic) and bounds (above_maximum).
	// The pipeline must short-circuit at constraints; the bounds rejection
	// reason must not appear in the decision.
	e := New(WithConstraints(cr), WithBounds(br))
	d := e.Submit(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 500})
	if d.Stage != "constraints" {
		t.Fatalf("constraint failure must short-circuit before bounds; got stage=%q", d.Stage)
	}
	if strings.Contains(d.Reason, "above_maximum") {
		t.Fatalf("constraint stage must not surface a bounds reason, got %q", d.Reason)
	}
}

func TestSubmitFallsThroughWhenBoundsPass(t *testing.T) {
	t.Parallel()

	br := NewBoundsRegistry()
	br.SetRateLimit(IntRange(10, 1000))

	e := New(WithConstraints(DefaultConstraints()), WithBounds(br))
	d := e.Submit(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 500})
	if d.Kind != DecisionReject {
		t.Fatalf("expected reject (pipeline still incomplete), got %s", d.Kind)
	}
	if d.Stage != "fallback" {
		t.Fatalf("a passing bound must let the proposal reach fallback; got stage=%q", d.Stage)
	}
}

func TestSubmitBoundsRunAfterContextCheck(t *testing.T) {
	t.Parallel()

	br := NewBoundsRegistry()
	br.SetRateLimit(IntRange(10, 100))

	e := New(WithBounds(br))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := e.Submit(ctx, Proposal{Kind: KindRateLimit, Agent: "x", Value: 500})
	if d.Kind != DecisionDefer || d.Stage != "intake" {
		t.Fatalf("cancelled context must defer at intake; got kind=%s stage=%q", d.Kind, d.Stage)
	}
}

func TestSubmitDampenerCooldownDefers(t *testing.T) {
	t.Parallel()

	dr := NewDampenerRegistry()
	dr.SetRateLimit(IntDampener{Cooldown: 10 * time.Second})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	dr.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 100}, t0)

	e := New(WithDampener(dr))
	e.now = func() time.Time { return t0.Add(3 * time.Second) }

	d := e.Submit(context.Background(), Proposal{
		Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 200,
	})
	if d.Kind != DecisionDefer {
		t.Fatalf("cooldown must produce DecisionDefer, got %s", d.Kind)
	}
	if d.Stage != "dampener" {
		t.Fatalf("expected stage=dampener, got %q", d.Stage)
	}
	if want := 7 * time.Second; d.RetryAfter != want {
		t.Fatalf("RetryAfter = %s, want %s", d.RetryAfter, want)
	}
	if !strings.Contains(d.Reason, "cooldown_active") {
		t.Fatalf("reason must include cooldown_active, got %q", d.Reason)
	}
}

func TestSubmitDampenerHysteresisRejects(t *testing.T) {
	t.Parallel()

	dr := NewDampenerRegistry()
	dr.SetRateLimit(IntDampener{Hysteresis: 50})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	dr.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 100}, t0)

	e := New(WithDampener(dr))
	e.now = func() time.Time { return t0.Add(time.Hour) }

	d := e.Submit(context.Background(), Proposal{
		Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 102,
	})
	if d.Kind != DecisionReject {
		t.Fatalf("hysteresis must produce DecisionReject, got %s", d.Kind)
	}
	if d.Stage != "dampener" {
		t.Fatalf("expected stage=dampener, got %q", d.Stage)
	}
	if d.RetryAfter != 0 {
		t.Fatalf("hysteresis decision must not set RetryAfter, got %s", d.RetryAfter)
	}
	if !strings.Contains(d.Reason, "below_hysteresis") {
		t.Fatalf("reason must include below_hysteresis, got %q", d.Reason)
	}
}

func TestSubmitFallsThroughWhenDampenerPasses(t *testing.T) {
	t.Parallel()

	dr := NewDampenerRegistry()
	dr.SetRateLimit(IntDampener{Cooldown: time.Second, Hysteresis: 10})

	// No prior RecordApplied, so the first proposal for this key passes
	// the dampener and falls through to the fallback rejection.
	e := New(WithConstraints(DefaultConstraints()), WithBounds(NewBoundsRegistry()), WithDampener(dr))
	d := e.Submit(context.Background(), Proposal{
		Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 100,
	})
	if d.Kind != DecisionReject {
		t.Fatalf("expected reject (pipeline still incomplete), got %s", d.Kind)
	}
	if d.Stage != "fallback" {
		t.Fatalf("a passing dampener must let the proposal reach fallback; got stage=%q", d.Stage)
	}
}

func TestSubmitBoundsFailureSkipsDampener(t *testing.T) {
	t.Parallel()

	br := NewBoundsRegistry()
	br.SetRateLimit(IntRange(10, 100))

	dr := NewDampenerRegistry()
	dr.SetRateLimit(IntDampener{Cooldown: time.Hour})
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	dr.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 50}, t0)

	e := New(WithBounds(br), WithDampener(dr))
	e.now = func() time.Time { return t0.Add(time.Second) }

	// Value 500 is above the bound AND would be in cooldown. Bounds
	// must short-circuit; the dampener stage must not be visible.
	d := e.Submit(context.Background(), Proposal{
		Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 500,
	})
	if d.Stage != "bounds" {
		t.Fatalf("bounds failure must short-circuit dampener; got stage=%q", d.Stage)
	}
	if strings.Contains(d.Reason, "cooldown") {
		t.Fatalf("bounds stage must not surface a dampener reason, got %q", d.Reason)
	}
}

func TestSubmitDampenerRunsAfterContextCheck(t *testing.T) {
	t.Parallel()

	dr := NewDampenerRegistry()
	dr.SetRateLimit(IntDampener{Cooldown: time.Hour})
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	dr.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 100}, t0)

	e := New(WithDampener(dr))
	e.now = func() time.Time { return t0.Add(time.Second) }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := e.Submit(ctx, Proposal{
		Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 200,
	})
	if d.Kind != DecisionDefer || d.Stage != "intake" {
		t.Fatalf("cancelled context must defer at intake; got kind=%s stage=%q", d.Kind, d.Stage)
	}
}
