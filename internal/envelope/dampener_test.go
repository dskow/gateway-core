package envelope

import (
	"strings"
	"testing"
	"time"
)

func TestDampenerRegistry_NilIsSafe(t *testing.T) {
	t.Parallel()

	var r *DampenerRegistry
	if o := r.Evaluate(Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}, time.Now()); o != nil {
		t.Fatalf("nil registry must not stop a proposal, got %+v", o)
	}
	// Set* on a nil receiver must be a safe no-op.
	r.SetRateLimit(IntDampener{Cooldown: time.Second, Hysteresis: 10})
	r.SetRouteWeight(FloatDampener{Cooldown: time.Second, Hysteresis: 0.1})
	r.SetCacheTTL(DurationDampener{Cooldown: time.Second, Hysteresis: time.Second})
	// RecordApplied on a nil receiver must be a safe no-op.
	r.RecordApplied(Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}, time.Now())
}

func TestDampenerRegistry_EmptyAllowsEverything(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	cases := []Proposal{
		{Kind: KindRateLimit, Agent: "x", Value: 100},
		{Kind: KindRouteWeight, Agent: "x", Value: 0.5},
		{Kind: KindCacheTTL, Agent: "x", Value: 30 * time.Second},
		{Kind: KindCircuitBreaker, Agent: "x", Value: "open"},
	}
	for _, p := range cases {
		if o := r.Evaluate(p, time.Now()); o != nil {
			t.Errorf("empty registry must allow %+v, got %+v", p, o)
		}
	}
}

func TestDampenerRegistry_FirstProposalForKeyAlwaysPasses(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetRateLimit(IntDampener{Cooldown: time.Hour, Hysteresis: 10})

	// No prior RecordApplied for this (Kind, Target). The dampener
	// has nothing to compare against and must pass.
	o := r.Evaluate(Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 100}, time.Now())
	if o != nil {
		t.Fatalf("first proposal for a key must pass, got %+v", o)
	}
}

func TestDampenerRegistry_CooldownDefersThenAllows(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetRateLimit(IntDampener{Cooldown: 10 * time.Second})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	r.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 100}, t0)

	// Inside the cooldown window — must defer.
	o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 200},
		t0.Add(3*time.Second),
	)
	if o == nil {
		t.Fatal("expected dampener outcome, got nil")
	}
	if o.Reason != "cooldown_active" {
		t.Fatalf("expected cooldown_active, got %+v", o)
	}
	if o.Kind != KindRateLimit || o.Target != "/api/users" {
		t.Fatalf("outcome must echo Kind and Target, got %+v", o)
	}
	if want := 7 * time.Second; o.RetryAfter != want {
		t.Fatalf("RetryAfter = %s, want %s", o.RetryAfter, want)
	}
	if !strings.Contains(o.Detail, "7s") {
		t.Fatalf("detail must include the remaining time, got %q", o.Detail)
	}

	// At the cooldown boundary — must pass.
	if o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 200},
		t0.Add(10*time.Second),
	); o != nil {
		t.Fatalf("at the cooldown boundary the proposal must pass, got %+v", o)
	}

	// After the cooldown — must pass.
	if o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 200},
		t0.Add(20*time.Second),
	); o != nil {
		t.Fatalf("after cooldown the proposal must pass, got %+v", o)
	}
}

func TestDampenerRegistry_HysteresisRejectsSmallChanges(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetRateLimit(IntDampener{Hysteresis: 50})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	r.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 100}, t0)

	// Within the band: |102 - 100| = 2 < 50.
	o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 102},
		t0.Add(time.Hour),
	)
	if o == nil {
		t.Fatal("expected dampener outcome, got nil")
	}
	if o.Reason != "below_hysteresis" {
		t.Fatalf("expected below_hysteresis, got %+v", o)
	}
	if o.RetryAfter != 0 {
		t.Fatalf("hysteresis outcome must not set RetryAfter, got %s", o.RetryAfter)
	}
	if !strings.Contains(o.Detail, "< 50") {
		t.Fatalf("detail must include the configured band, got %q", o.Detail)
	}

	// Just outside the band: |150 - 100| = 50 >= 50.
	if o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 150},
		t0.Add(time.Hour),
	); o != nil {
		t.Fatalf("at the hysteresis boundary the proposal must pass, got %+v", o)
	}

	// Far outside: also below works in the other direction.
	if o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 25},
		t0.Add(time.Hour),
	); o != nil {
		t.Fatalf("|25-100|=75 >= 50, must pass, got %+v", o)
	}
}

func TestDampenerRegistry_HysteresisDisabledByZero(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetRateLimit(IntDampener{Cooldown: time.Hour}) // Hysteresis omitted

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	r.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 100}, t0)

	// After cooldown elapses, any value differing by even 1 must pass.
	if o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 101},
		t0.Add(2*time.Hour),
	); o != nil {
		t.Fatalf("zero hysteresis must allow tiny changes, got %+v", o)
	}
}

func TestDampenerRegistry_CooldownChecksBeforeHysteresis(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetRateLimit(IntDampener{Cooldown: time.Minute, Hysteresis: 50})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	r.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 100}, t0)

	// Proposal violates BOTH cooldown (5s in) and hysteresis (|105-100|<50).
	// Cooldown is the cheaper, more time-precise check; it must win.
	o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 105},
		t0.Add(5*time.Second),
	)
	if o == nil || o.Reason != "cooldown_active" {
		t.Fatalf("cooldown must short-circuit hysteresis, got %+v", o)
	}
}

func TestDampenerRegistry_TargetIsolation(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetRateLimit(IntDampener{Cooldown: time.Hour})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	r.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 100}, t0)

	// A change on a different target must not be dampened by /api/users' cooldown.
	if o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/orders", Agent: "x", Value: 200},
		t0.Add(time.Second),
	); o != nil {
		t.Fatalf("dampener must isolate by Target, got %+v", o)
	}
}

func TestDampenerRegistry_KindIsolation(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetRateLimit(IntDampener{Cooldown: time.Hour})
	r.SetCacheTTL(DurationDampener{Cooldown: time.Hour})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	r.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 100}, t0)

	// A KindCacheTTL change for the same Target must not be dampened by
	// the rate-limit cooldown — they are different parameters.
	if o := r.Evaluate(
		Proposal{Kind: KindCacheTTL, Target: "/api/users", Agent: "x", Value: 30 * time.Second},
		t0.Add(time.Second),
	); o != nil {
		t.Fatalf("dampener must isolate by Kind, got %+v", o)
	}
}

func TestDampenerRegistry_KindWithoutPolicyIsAllowed(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetRateLimit(IntDampener{Cooldown: time.Hour, Hysteresis: 50})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	// Even after recording state, a kind without a policy must pass.
	r.RecordApplied(Proposal{Kind: KindRouteWeight, Target: "/api/users", Value: 0.5}, t0)

	if o := r.Evaluate(
		Proposal{Kind: KindRouteWeight, Target: "/api/users", Agent: "x", Value: 0.51},
		t0.Add(time.Second),
	); o != nil {
		t.Fatalf("kind without policy must pass, got %+v", o)
	}
	// CircuitBreaker has no Set method at all — must always pass.
	if o := r.Evaluate(
		Proposal{Kind: KindCircuitBreaker, Agent: "x", Value: "open"},
		t0.Add(time.Second),
	); o != nil {
		t.Fatalf("kind with no Set method must pass, got %+v", o)
	}
}

func TestDampenerRegistry_RouteWeight(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetRouteWeight(FloatDampener{Cooldown: 30 * time.Second, Hysteresis: 0.1})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	r.RecordApplied(Proposal{Kind: KindRouteWeight, Target: "blue", Value: 0.5}, t0)

	// In cooldown → defer.
	o := r.Evaluate(
		Proposal{Kind: KindRouteWeight, Target: "blue", Agent: "x", Value: 0.7},
		t0.Add(5*time.Second),
	)
	if o == nil || o.Reason != "cooldown_active" {
		t.Fatalf("expected cooldown_active, got %+v", o)
	}

	// After cooldown, hysteresis applies. |0.55 - 0.5| = 0.05 < 0.1.
	o = r.Evaluate(
		Proposal{Kind: KindRouteWeight, Target: "blue", Agent: "x", Value: 0.55},
		t0.Add(time.Minute),
	)
	if o == nil || o.Reason != "below_hysteresis" {
		t.Fatalf("expected below_hysteresis, got %+v", o)
	}

	// |0.7 - 0.5| = 0.2 >= 0.1 → pass.
	if o := r.Evaluate(
		Proposal{Kind: KindRouteWeight, Target: "blue", Agent: "x", Value: 0.7},
		t0.Add(time.Minute),
	); o != nil {
		t.Fatalf("expected pass, got %+v", o)
	}
}

func TestDampenerRegistry_CacheTTL(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetCacheTTL(DurationDampener{Cooldown: time.Minute, Hysteresis: 5 * time.Second})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	r.RecordApplied(Proposal{Kind: KindCacheTTL, Target: "/api/users", Value: 30 * time.Second}, t0)

	// Within hysteresis: |31s - 30s| = 1s < 5s.
	o := r.Evaluate(
		Proposal{Kind: KindCacheTTL, Target: "/api/users", Agent: "x", Value: 31 * time.Second},
		t0.Add(2*time.Minute),
	)
	if o == nil || o.Reason != "below_hysteresis" {
		t.Fatalf("expected below_hysteresis, got %+v", o)
	}

	// Outside hysteresis: |40s - 30s| = 10s >= 5s.
	if o := r.Evaluate(
		Proposal{Kind: KindCacheTTL, Target: "/api/users", Agent: "x", Value: 40 * time.Second},
		t0.Add(2*time.Minute),
	); o != nil {
		t.Fatalf("expected pass, got %+v", o)
	}
}

func TestDampenerRegistry_HysteresisIgnoresTypeMismatch(t *testing.T) {
	t.Parallel()

	// Constraints/bounds are responsible for type validation. The dampener
	// must not crash on an unexpected value type that slipped past — it
	// just falls through (the proposal proceeds as if the band passed).
	r := NewDampenerRegistry()
	r.SetRateLimit(IntDampener{Hysteresis: 50})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	// Record a noninteger value (shouldn't happen in practice).
	r.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: "100"}, t0)

	if o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 105},
		t0.Add(time.Hour),
	); o != nil {
		t.Fatalf("hysteresis must not stop a proposal it cannot evaluate, got %+v", o)
	}
}

func TestDampenerRegistry_RecordAppliedReplacesPriorState(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetRateLimit(IntDampener{Cooldown: time.Hour, Hysteresis: 50})

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	r.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 100}, t0)
	r.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 200}, t0.Add(2*time.Hour))

	// Hysteresis is now measured from 200, not 100. |205-200|=5 < 50.
	o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 205},
		t0.Add(4*time.Hour),
	)
	if o == nil || o.Reason != "below_hysteresis" {
		t.Fatalf("hysteresis must use the most recent applied value, got %+v", o)
	}
}

func TestDampenerOutcome_ErrorFormat(t *testing.T) {
	t.Parallel()

	with := &DampenerOutcome{Kind: KindRateLimit, Reason: "cooldown_active", Detail: "3s remaining"}
	if got := with.Error(); got != "dampener(rate_limit): cooldown_active (3s remaining)" {
		t.Fatalf("unexpected formatted error: %q", got)
	}
	without := &DampenerOutcome{Kind: KindCacheTTL, Reason: "below_hysteresis"}
	if got := without.Error(); got != "dampener(cache_ttl): below_hysteresis" {
		t.Fatalf("unexpected formatted error: %q", got)
	}
	var nilo *DampenerOutcome
	if got := nilo.Error(); got == "" {
		t.Fatal("nil outcome.Error() must not be empty")
	}
}

func TestDampenerRegistry_SetReplacesPriorPolicy(t *testing.T) {
	t.Parallel()

	r := NewDampenerRegistry()
	r.SetRateLimit(IntDampener{Cooldown: time.Hour})
	r.SetRateLimit(IntDampener{Cooldown: time.Second}) // replaces

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	r.RecordApplied(Proposal{Kind: KindRateLimit, Target: "/api/users", Value: 100}, t0)

	// 2s after applied: would still be in cooldown under the original
	// 1h policy, but the replacement 1s policy already elapsed.
	if o := r.Evaluate(
		Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 200},
		t0.Add(2*time.Second),
	); o != nil {
		t.Fatalf("replacement policy must take effect, got %+v", o)
	}
}
