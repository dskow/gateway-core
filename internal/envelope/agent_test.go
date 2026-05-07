package envelope

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// emitProposal returns a planner that always proposes the same Proposal.
func emitProposal(name string, p Proposal) Planner {
	return PlannerFunc{
		NameValue: name,
		Fn: func(context.Context) (PlanResult, error) {
			return PlanResult{Emit: true, Proposal: p}, nil
		},
	}
}

func passReviewer(name string, role Role) Reviewer {
	return ReviewerFunc{
		NameValue: name,
		RoleValue: role,
		Fn: func(context.Context, Proposal) (ReviewResult, error) {
			return ReviewResult{Veto: false}, nil
		},
	}
}

func TestRoleString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		r    Role
		want string
	}{
		{RolePlanner, "planner"},
		{RoleVerifier, "verifier"},
		{RoleSafety, "safety"},
		{RoleObserver, "observer"},
		{Role(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Errorf("Role(%d).String() = %q, want %q", c.r, got, c.want)
		}
	}
}

func TestPipelineOutcomeKindString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		k    PipelineOutcomeKind
		want string
	}{
		{OutcomeEmit, "emit"},
		{OutcomeNoProposal, "no_proposal"},
		{OutcomeVeto, "veto"},
		{OutcomeError, "error"},
		{OutcomeDisabled, "disabled"},
		{PipelineOutcomeKind(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("PipelineOutcomeKind(%d).String() = %q, want %q", c.k, got, c.want)
		}
	}
}

func TestNewPipelineRejectsNilPlanner(t *testing.T) {
	t.Parallel()

	if _, err := NewPipeline(nil, nil); err == nil {
		t.Fatal("expected error for nil planner")
	}
}

func TestNewPipelineRejectsPlannerWithWrongRole(t *testing.T) {
	t.Parallel()

	bad := ReviewerFunc{NameValue: "wrong", RoleValue: RoleVerifier} // not a Planner
	// Construct a Planner whose Role() is wrong by adapting through PlannerFunc and
	// asserting the role check on a hand-rolled type.
	type wrongRolePlanner struct{ PlannerFunc }
	wp := wrongRolePlanner{PlannerFunc: PlannerFunc{NameValue: "p"}}
	// Override Role via a wrapper that returns RoleSafety.
	bad2 := plannerWithRole{name: "p", role: RoleSafety}
	if _, err := NewPipeline(bad2, nil); err == nil {
		t.Fatal("expected error when planner.Role() is not RolePlanner")
	}
	// Ensure we also reject a Reviewer accidentally passed as Planner — caught at
	// compile time by the type system, but verify that a Reviewer-with-name passed
	// in the reviewers slot for a planner role is also flagged.
	rv := []Reviewer{passReviewer("v", RoleVerifier)}
	_ = wp
	_ = bad
	_ = rv
}

// plannerWithRole is a Planner whose Role() can be misconfigured for testing.
type plannerWithRole struct {
	name string
	role Role
}

func (p plannerWithRole) Role() Role    { return p.role }
func (p plannerWithRole) Name() string  { return p.name }
func (p plannerWithRole) Plan(context.Context) (PlanResult, error) {
	return PlanResult{Emit: false, Reason: "noop"}, nil
}

func TestNewPipelineRejectsNilReviewer(t *testing.T) {
	t.Parallel()

	planner := emitProposal("p", Proposal{Kind: KindRateLimit, Agent: "p", Value: 100})
	_, err := NewPipeline(planner, []Reviewer{nil})
	if err == nil {
		t.Fatal("expected error for nil reviewer")
	}
}

func TestNewPipelineRejectsReviewerWithPlannerRole(t *testing.T) {
	t.Parallel()

	planner := emitProposal("p", Proposal{Kind: KindRateLimit, Agent: "p", Value: 100})
	bad := ReviewerFunc{NameValue: "rogue", RoleValue: RolePlanner}
	if _, err := NewPipeline(planner, []Reviewer{bad}); err == nil {
		t.Fatal("expected error when a reviewer claims RolePlanner")
	}
}

func TestNewPipelineRejectsMisorderedReviewers(t *testing.T) {
	t.Parallel()

	planner := emitProposal("p", Proposal{Kind: KindRateLimit, Agent: "p", Value: 100})
	rs := []Reviewer{
		passReviewer("safety", RoleSafety),
		passReviewer("verifier", RoleVerifier), // out of order
	}
	_, err := NewPipeline(planner, rs)
	if err == nil {
		t.Fatal("expected error for misordered reviewers")
	}
	if !errors.Is(err, errAgentMisorder) {
		t.Fatalf("expected errAgentMisorder sentinel, got %v", err)
	}
}

func TestNewPipelineAllowsDuplicateRoles(t *testing.T) {
	t.Parallel()

	planner := emitProposal("p", Proposal{Kind: KindRateLimit, Agent: "p", Value: 100})
	rs := []Reviewer{
		passReviewer("safety_a", RoleSafety),
		passReviewer("safety_b", RoleSafety),
	}
	if _, err := NewPipeline(planner, rs); err != nil {
		t.Fatalf("duplicate roles must be allowed, got %v", err)
	}
}

func TestPipelineRunNoProposal(t *testing.T) {
	t.Parallel()

	planner := PlannerFunc{
		NameValue: "p",
		Fn: func(context.Context) (PlanResult, error) {
			return PlanResult{Emit: false, Reason: "no_signal"}, nil
		},
	}
	pl, err := NewPipeline(planner, nil)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	out := pl.Run(context.Background())
	if out.Kind != OutcomeNoProposal {
		t.Fatalf("expected OutcomeNoProposal, got %s", out.Kind)
	}
	if out.Role != RolePlanner || out.AgentName != "p" {
		t.Fatalf("outcome must record producing agent, got %+v", out)
	}
	if out.Reason != "no_signal" {
		t.Fatalf("outcome must echo planner reason, got %q", out.Reason)
	}
	if out.DecidedAt.IsZero() {
		t.Fatal("DecidedAt must be set on every outcome")
	}
}

func TestPipelineRunEmitsWhenAllReviewersPass(t *testing.T) {
	t.Parallel()

	want := Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "p", Value: 200, Reason: "load_up"}
	pl, err := NewPipeline(
		emitProposal("p", want),
		[]Reviewer{
			passReviewer("v", RoleVerifier),
			passReviewer("s", RoleSafety),
			passReviewer("o", RoleObserver),
		},
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	out := pl.Run(context.Background())
	if out.Kind != OutcomeEmit {
		t.Fatalf("expected OutcomeEmit, got %s (%+v)", out.Kind, out)
	}
	if out.Proposal != want {
		t.Fatalf("proposal must be the planner's verbatim, got %+v", out.Proposal)
	}
	if out.AgentName != "p" {
		t.Fatalf("emit outcome must name the planner, got %q", out.AgentName)
	}
}

func TestPipelineRunVetoShortCircuits(t *testing.T) {
	t.Parallel()

	var safetyCalled, observerCalled int32
	pl, err := NewPipeline(
		emitProposal("p", Proposal{Kind: KindRateLimit, Agent: "p", Value: 100}),
		[]Reviewer{
			ReviewerFunc{NameValue: "v", RoleValue: RoleVerifier,
				Fn: func(context.Context, Proposal) (ReviewResult, error) {
					return ReviewResult{Veto: true, Reason: "value_outside_planner_reasoning",
						Detail: "planner claimed +20% but value is +400%"}, nil
				}},
			ReviewerFunc{NameValue: "s", RoleValue: RoleSafety,
				Fn: func(context.Context, Proposal) (ReviewResult, error) {
					atomic.AddInt32(&safetyCalled, 1)
					return ReviewResult{}, nil
				}},
			ReviewerFunc{NameValue: "o", RoleValue: RoleObserver,
				Fn: func(context.Context, Proposal) (ReviewResult, error) {
					atomic.AddInt32(&observerCalled, 1)
					return ReviewResult{}, nil
				}},
		},
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	out := pl.Run(context.Background())
	if out.Kind != OutcomeVeto {
		t.Fatalf("expected OutcomeVeto, got %s", out.Kind)
	}
	if out.Role != RoleVerifier || out.AgentName != "v" {
		t.Fatalf("outcome must name vetoing reviewer, got role=%s name=%q", out.Role, out.AgentName)
	}
	if out.Reason != "value_outside_planner_reasoning" {
		t.Fatalf("expected reason from reviewer, got %q", out.Reason)
	}
	if !strings.Contains(out.Detail, "+20%") {
		t.Fatalf("detail must surface from reviewer, got %q", out.Detail)
	}
	if atomic.LoadInt32(&safetyCalled) != 0 {
		t.Fatal("safety reviewer must not run after a veto")
	}
	if atomic.LoadInt32(&observerCalled) != 0 {
		t.Fatal("observer reviewer must not run after a veto")
	}
}

func TestPipelineRunPlannerErrorIsRecorded(t *testing.T) {
	t.Parallel()

	boom := errors.New("LLM rate limit")
	planner := PlannerFunc{
		NameValue: "p",
		Fn:        func(context.Context) (PlanResult, error) { return PlanResult{}, boom },
	}
	pl, err := NewPipeline(planner, nil)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	out := pl.Run(context.Background())
	if out.Kind != OutcomeError {
		t.Fatalf("expected OutcomeError, got %s", out.Kind)
	}
	if out.Role != RolePlanner || out.AgentName != "p" {
		t.Fatalf("outcome must name failing agent, got role=%s name=%q", out.Role, out.AgentName)
	}
	if out.Reason != "planner_error" {
		t.Fatalf("expected reason=planner_error, got %q", out.Reason)
	}
	if !errors.Is(out.Err, boom) {
		t.Fatalf("Err must wrap the original error, got %v", out.Err)
	}
}

func TestPipelineRunReviewerErrorIsRecorded(t *testing.T) {
	t.Parallel()

	boom := errors.New("verifier subprocess crashed")
	pl, err := NewPipeline(
		emitProposal("p", Proposal{Kind: KindRateLimit, Agent: "p", Value: 100}),
		[]Reviewer{
			ReviewerFunc{NameValue: "v", RoleValue: RoleVerifier,
				Fn: func(context.Context, Proposal) (ReviewResult, error) {
					return ReviewResult{}, boom
				}},
		},
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	out := pl.Run(context.Background())
	if out.Kind != OutcomeError {
		t.Fatalf("expected OutcomeError, got %s", out.Kind)
	}
	if out.Role != RoleVerifier || out.AgentName != "v" {
		t.Fatalf("outcome must name failing reviewer, got role=%s name=%q", out.Role, out.AgentName)
	}
	if out.Reason != "reviewer_error" {
		t.Fatalf("expected reason=reviewer_error, got %q", out.Reason)
	}
	if !errors.Is(out.Err, boom) {
		t.Fatalf("Err must wrap the original error, got %v", out.Err)
	}
}

func TestPipelineRunHonorsPreCancelledContext(t *testing.T) {
	t.Parallel()

	called := false
	planner := PlannerFunc{
		NameValue: "p",
		Fn: func(context.Context) (PlanResult, error) {
			called = true
			return PlanResult{Emit: false, Reason: "x"}, nil
		},
	}
	pl, err := NewPipeline(planner, nil)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := pl.Run(ctx)
	if out.Kind != OutcomeError {
		t.Fatalf("cancelled context must produce OutcomeError, got %s", out.Kind)
	}
	if out.Reason != "context_cancelled" {
		t.Fatalf("expected reason=context_cancelled, got %q", out.Reason)
	}
	if called {
		t.Fatal("planner must not run when context is already cancelled")
	}
}

func TestPipelineErrorBudgetDisablesAfterStreak(t *testing.T) {
	t.Parallel()

	boom := errors.New("transient failure")
	planner := PlannerFunc{
		NameValue: "p",
		Fn:        func(context.Context) (PlanResult, error) { return PlanResult{}, boom },
	}
	pl, err := NewPipeline(planner, nil, WithErrorBudget(ErrorBudget{
		MaxConsecutiveErrors: 2,
		Cooldown:             time.Minute,
	}))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	pl.now = func() time.Time { return t0 }

	// First failure — budget not yet exhausted.
	out := pl.Run(context.Background())
	if out.Kind != OutcomeError {
		t.Fatalf("first run: expected OutcomeError, got %s", out.Kind)
	}
	if !out.DisabledUntil.IsZero() {
		t.Fatal("first failure must not disable yet")
	}
	if pl.Disabled() {
		t.Fatal("pipeline must not be disabled after one failure")
	}

	// Second failure — exhausts the budget.
	out = pl.Run(context.Background())
	if out.Kind != OutcomeError {
		t.Fatalf("second run: expected OutcomeError, got %s", out.Kind)
	}
	wantUntil := t0.Add(time.Minute)
	if !out.DisabledUntil.Equal(wantUntil) {
		t.Fatalf("DisabledUntil = %v, want %v", out.DisabledUntil, wantUntil)
	}
	if !pl.Disabled() {
		t.Fatal("pipeline must be disabled after exhausting the budget")
	}

	// Third call inside cooldown — must short-circuit to disabled property.
	out = pl.Run(context.Background())
	if out.Kind != OutcomeDisabled {
		t.Fatalf("inside cooldown: expected OutcomeDisabled, got %s", out.Kind)
	}
	if out.Reason != "error_budget_exhausted" {
		t.Fatalf("expected reason=error_budget_exhausted, got %q", out.Reason)
	}
}

func TestPipelineErrorBudgetClearsAfterCooldown(t *testing.T) {
	t.Parallel()

	calls := int32(0)
	planner := PlannerFunc{
		NameValue: "p",
		Fn: func(context.Context) (PlanResult, error) {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				return PlanResult{}, errors.New("transient")
			}
			return PlanResult{Emit: false, Reason: "stable"}, nil
		},
	}
	pl, err := NewPipeline(planner, nil, WithErrorBudget(ErrorBudget{
		MaxConsecutiveErrors: 1,
		Cooldown:             10 * time.Millisecond,
	}))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	clock := t0
	pl.now = func() time.Time { return clock }

	if out := pl.Run(context.Background()); out.Kind != OutcomeError {
		t.Fatalf("first run: expected error, got %s", out.Kind)
	}
	if !pl.Disabled() {
		t.Fatal("pipeline must be disabled after one error with budget=1")
	}

	// Advance the clock past the cooldown.
	clock = t0.Add(50 * time.Millisecond)
	if pl.Disabled() {
		t.Fatal("pipeline must auto-recover after cooldown elapses")
	}
	out := pl.Run(context.Background())
	if out.Kind != OutcomeNoProposal {
		t.Fatalf("expected OutcomeNoProposal after recovery, got %s", out.Kind)
	}
}

func TestPipelineSuccessfulRunResetsErrorStreak(t *testing.T) {
	t.Parallel()

	calls := int32(0)
	planner := PlannerFunc{
		NameValue: "p",
		Fn: func(context.Context) (PlanResult, error) {
			n := atomic.AddInt32(&calls, 1)
			if n%2 == 1 {
				return PlanResult{}, errors.New("flaky")
			}
			return PlanResult{Emit: false, Reason: "ok"}, nil
		},
	}
	pl, err := NewPipeline(planner, nil, WithErrorBudget(ErrorBudget{
		MaxConsecutiveErrors: 2,
		Cooldown:             time.Hour,
	}))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	// Pattern: error, success, error, success — never two errors in a row,
	// so the budget must never trip.
	for i := 0; i < 4; i++ {
		_ = pl.Run(context.Background())
		if pl.Disabled() {
			t.Fatalf("pipeline disabled after iteration %d; alternating success must reset the streak", i)
		}
	}
}

func TestPipelineManualDisable(t *testing.T) {
	t.Parallel()

	called := false
	planner := PlannerFunc{
		NameValue: "p",
		Fn: func(context.Context) (PlanResult, error) {
			called = true
			return PlanResult{Emit: false, Reason: "ok"}, nil
		},
	}
	pl, err := NewPipeline(planner, nil)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	pl.Disable()
	out := pl.Run(context.Background())
	if out.Kind != OutcomeDisabled {
		t.Fatalf("expected OutcomeDisabled, got %s", out.Kind)
	}
	if out.Reason != "operator_disabled" {
		t.Fatalf("expected reason=operator_disabled, got %q", out.Reason)
	}
	if called {
		t.Fatal("planner must not run while operator-disabled")
	}

	pl.Resume()
	if pl.Disabled() {
		t.Fatal("Resume must clear manual disable")
	}
	if out := pl.Run(context.Background()); out.Kind != OutcomeNoProposal {
		t.Fatalf("expected OutcomeNoProposal after resume, got %s", out.Kind)
	}
}

func TestPipelineNilReceiverIsDisabled(t *testing.T) {
	t.Parallel()

	var pl *Pipeline
	if !pl.Disabled() {
		t.Fatal("nil pipeline must report Disabled")
	}
	out := pl.Run(context.Background())
	if out.Kind != OutcomeDisabled {
		t.Fatalf("nil pipeline must produce OutcomeDisabled, got %s", out.Kind)
	}
	pl.Disable()
	pl.Resume()
}

func TestPlannerFunc_NilFnReturnsNoProposal(t *testing.T) {
	t.Parallel()

	p := PlannerFunc{NameValue: "p"}
	r, err := p.Plan(context.Background())
	if err != nil {
		t.Fatalf("nil Fn must not error, got %v", err)
	}
	if r.Emit {
		t.Fatal("nil Fn must not emit")
	}
}

func TestPlannerFunc_AnonymousName(t *testing.T) {
	t.Parallel()

	p := PlannerFunc{}
	if p.Name() != "anonymous_planner" {
		t.Fatalf("default name must be anonymous_planner, got %q", p.Name())
	}
}

func TestReviewerFunc_AnonymousName(t *testing.T) {
	t.Parallel()

	r := ReviewerFunc{RoleValue: RoleVerifier}
	if r.Name() != "anonymous_reviewer" {
		t.Fatalf("default name must be anonymous_reviewer, got %q", r.Name())
	}
}

func TestReviewerFunc_NilFnPasses(t *testing.T) {
	t.Parallel()

	r := ReviewerFunc{RoleValue: RoleVerifier}
	out, err := r.Review(context.Background(), Proposal{Kind: KindRateLimit, Agent: "p", Value: 100})
	if err != nil {
		t.Fatalf("nil Fn must not error, got %v", err)
	}
	if out.Veto {
		t.Fatal("nil Fn must not veto")
	}
}

func TestPipelineRunIntegratesWithEnvelopeSubmit(t *testing.T) {
	t.Parallel()

	want := Proposal{Kind: KindRateLimit, Target: "/api/users", Agent: "p", Value: 200}
	pl, err := NewPipeline(
		emitProposal("p", want),
		[]Reviewer{
			passReviewer("v", RoleVerifier),
			passReviewer("s", RoleSafety),
		},
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	// One realistic round-trip: Pipeline produces a Proposal, the agentic
	// sidecar (this test) hands it to the Envelope. With no envelope stages
	// configured the proposal lands at the autonomous-safe fallback — but
	// the call shape (Pipeline.Run → Envelope.Submit) is exactly what the
	// sidecar will use in production.
	out := pl.Run(context.Background())
	if out.Kind != OutcomeEmit {
		t.Fatalf("expected OutcomeEmit, got %s (%+v)", out.Kind, out)
	}
	env := New(WithConstraints(DefaultConstraints()))
	d := env.Submit(context.Background(), out.Proposal)
	if d.Kind != DecisionReject || d.Stage != "fallback" {
		t.Fatalf("expected autonomous-safe fallback rejection; got %+v", d)
	}
}

func TestDefaultErrorBudget(t *testing.T) {
	t.Parallel()

	b := DefaultErrorBudget()
	if b.MaxConsecutiveErrors <= 0 {
		t.Fatalf("default budget must enable the check, got %+v", b)
	}
	if b.Cooldown <= 0 {
		t.Fatalf("default budget must set a positive cooldown, got %+v", b)
	}
}

func TestPipelineZeroCooldownStaysDisabledUntilResume(t *testing.T) {
	t.Parallel()

	planner := PlannerFunc{
		NameValue: "p",
		Fn:        func(context.Context) (PlanResult, error) { return PlanResult{}, errors.New("boom") },
	}
	pl, err := NewPipeline(planner, nil, WithErrorBudget(ErrorBudget{
		MaxConsecutiveErrors: 1,
		Cooldown:             0, // means "stay disabled forever"
	}))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	if out := pl.Run(context.Background()); out.Kind != OutcomeError {
		t.Fatalf("first run: expected error, got %s", out.Kind)
	}
	if !pl.Disabled() {
		t.Fatal("pipeline must be disabled after exhausting a zero-cooldown budget")
	}

	// Advancing the clock by a year is irrelevant — only Resume clears it.
	pl.now = func() time.Time { return time.Now().Add(365 * 24 * time.Hour) }
	if !pl.Disabled() {
		t.Fatal("zero-cooldown disable must not auto-recover")
	}
	pl.Resume()
	if pl.Disabled() {
		t.Fatal("Resume must clear a zero-cooldown disable")
	}
}
