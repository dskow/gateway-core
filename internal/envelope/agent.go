package envelope

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Role names which slot of the multi-agent pipeline an Agent fills. The
// pipeline is linear and ordered: a Planner produces the proposal, then
// each Reviewer in non-decreasing-Role order may veto it. The contribution
// claim of the pattern (§11 of docs/AGENTIC_ENVELOPE.md) names exactly
// these four roles; new roles are not added without updating the design
// document.
type Role int

const (
	// RolePlanner is the producer. Exactly one Planner runs per pipeline
	// invocation and is the only agent permitted to author a Proposal.
	RolePlanner Role = iota

	// RoleVerifier checks correctness of the planner's proposal — does
	// the value the Planner picked actually achieve the stated reason,
	// is the math sound, are referenced targets real.
	RoleVerifier

	// RoleSafety applies safety heuristics specific to the agent layer.
	// Some checks overlap with the Envelope's Constraints stage but
	// run earlier and produce structured agent-level vetoes (e.g.,
	// "this proposal looks like an oscillation pattern this agent
	// should not have produced").
	RoleSafety

	// RoleObserver is the longest-horizon reviewer. It can veto based
	// on patterns across recent decisions (e.g., the planner has been
	// emitting identical proposals for an hour — something is wrong
	// with the producer, hold the line).
	RoleObserver
)

// String returns a stable, lower-case role name suitable for logs.
func (r Role) String() string {
	switch r {
	case RolePlanner:
		return "planner"
	case RoleVerifier:
		return "verifier"
	case RoleSafety:
		return "safety"
	case RoleObserver:
		return "observer"
	}
	return "unknown"
}

// Agent is the common interface every pipeline participant satisfies.
// Concrete agents implement either Planner or Reviewer; Agent itself is
// just the metadata an audit log needs.
type Agent interface {
	// Role identifies this agent's pipeline slot. The Pipeline enforces
	// non-decreasing Role order at construction time.
	Role() Role

	// Name is a stable identifier used in audit logs and on
	// PipelineOutcome.AgentName when this agent vetoes or errors.
	Name() string
}

// Planner is the producer slot. The pipeline runs exactly one Planner
// per invocation and uses its result as the working proposal that
// every later Reviewer evaluates.
//
// A Planner that has nothing to propose returns PlanResult{Emit: false}
// with a Reason; this is not an error and does not consume the error
// budget. It is the autonomous-safe steady state: most of the time, an
// agent should propose nothing.
type Planner interface {
	Agent

	// Plan returns the planner's verdict for the current cycle. A
	// non-nil error indicates the planner itself failed (LLM timeout,
	// missing dependency, etc.) and counts toward the error budget.
	Plan(ctx context.Context) (PlanResult, error)
}

// PlanResult is what a Planner returns from one cycle. Emit gates
// whether a Proposal was authored at all; when Emit is false, the
// Reason field explains why no proposal was emitted.
type PlanResult struct {
	// Emit is true when Proposal is populated and should flow down
	// the pipeline.
	Emit bool

	// Proposal is the authored proposal. The Pipeline copies it
	// verbatim into the eventual PipelineOutcome — Reviewers may not
	// rewrite it.
	Proposal Proposal

	// Reason is a stable code or human-readable explanation for
	// Emit=false. Logged but not parsed by the pipeline.
	Reason string
}

// Reviewer is the veto slot. Every non-Planner agent in the pipeline
// implements this interface. A Reviewer evaluates the working proposal
// and either passes (returns ReviewResult{Veto: false}) or vetoes
// with a structured reason. The first vetoing reviewer short-circuits
// the rest; later reviewers do not run.
//
// A Reviewer that returns a non-nil error counts toward the error
// budget. This is intentional: a flaky reviewer should not silently
// approve proposals the rest of the pipeline assumes it scrutinized.
type Reviewer interface {
	Agent

	// Review inspects p and returns a structured pass/veto verdict.
	// p is the proposal as authored by the Planner; reviewers must
	// not retain or mutate it.
	Review(ctx context.Context, p Proposal) (ReviewResult, error)
}

// ReviewResult is what a Reviewer returns from one cycle.
type ReviewResult struct {
	// Veto is true when the reviewer rejects the proposal.
	Veto bool

	// Reason is a stable machine-readable code (for example,
	// "value_outside_planner_reasoning" or "dissent_pattern_detected").
	// Required when Veto is true.
	Reason string

	// Detail is an optional human-readable elaboration; safe to log,
	// not part of the stable contract.
	Detail string
}

// PipelineOutcomeKind classifies the result of a pipeline run.
type PipelineOutcomeKind int

const (
	// OutcomeEmit means the planner produced a proposal and every
	// reviewer passed. Proposal is populated; the caller (typically
	// the agentic sidecar) should hand it to Envelope.Submit.
	OutcomeEmit PipelineOutcomeKind = iota

	// OutcomeNoProposal means the planner declined to emit. This is
	// the autonomous-safe steady state and the reason the deterministic
	// core remains unaffected when agents are doing nothing useful.
	OutcomeNoProposal

	// OutcomeVeto means a reviewer vetoed the planner's proposal.
	// AgentName, Role, Reason, and Detail describe which reviewer and
	// why.
	OutcomeVeto

	// OutcomeError means an agent returned a non-nil error. The
	// pipeline records the failure against its error budget; if the
	// budget is exhausted the next Run returns OutcomeDisabled until
	// the configured cooldown elapses.
	OutcomeError

	// OutcomeDisabled means the pipeline declined to run any agents:
	// either an operator manually disabled it, or the error budget is
	// exhausted. The control plane is unaffected — the pipeline is
	// simply silent.
	OutcomeDisabled
)

// String returns a human-readable outcome name.
func (k PipelineOutcomeKind) String() string {
	switch k {
	case OutcomeEmit:
		return "emit"
	case OutcomeNoProposal:
		return "no_proposal"
	case OutcomeVeto:
		return "veto"
	case OutcomeError:
		return "error"
	case OutcomeDisabled:
		return "disabled"
	}
	return "unknown"
}

// PipelineOutcome is the full structured result of one Run. It is
// shaped for audit logs: every interesting field is populated for the
// outcome kinds it applies to and zero for those it does not.
type PipelineOutcome struct {
	Kind PipelineOutcomeKind

	// Proposal is populated only on OutcomeEmit.
	Proposal Proposal

	// Role is the role of the agent that produced this outcome (the
	// planner for Emit / NoProposal, the vetoing reviewer for Veto,
	// the failing agent for Error). Zero for OutcomeDisabled.
	Role Role

	// AgentName is the name of the agent that produced this outcome.
	// Empty for OutcomeDisabled.
	AgentName string

	// Reason is a stable code or human-readable explanation. Empty
	// for OutcomeEmit.
	Reason string

	// Detail is an optional elaboration; safe to log, not part of the
	// stable contract.
	Detail string

	// Err is non-nil only on OutcomeError. Inspect it for context.Canceled
	// / context.DeadlineExceeded if you care about the failure mode.
	Err error

	// DisabledUntil is meaningful only on OutcomeDisabled produced by
	// an exhausted error budget; it is the wall-clock time the
	// pipeline expects to allow Run to proceed again. Zero if the
	// disable is operator-driven (no automatic recovery).
	DisabledUntil time.Time

	// DecidedAt is the time the pipeline produced this outcome.
	DecidedAt time.Time
}

// ErrorBudget controls when the pipeline self-disables after a streak
// of agent errors. The zero value disables the budget entirely:
// errors are surfaced but the pipeline never disables itself.
//
// The budget is intentionally simple — consecutive failures plus a
// fixed cooldown. A more sophisticated budget (rolling window,
// per-agent counts, exponential backoff) is a future extension.
type ErrorBudget struct {
	// MaxConsecutiveErrors is the number of consecutive OutcomeError
	// runs that triggers a disable. Zero or negative disables the
	// budget.
	MaxConsecutiveErrors int

	// Cooldown is how long the pipeline stays disabled after the
	// budget is exhausted. The next Run after Cooldown elapses runs
	// agents again; a successful run resets the consecutive counter.
	// Zero means "stay disabled forever" — the operator must
	// explicitly call Resume.
	Cooldown time.Duration
}

// DefaultErrorBudget returns a conservative budget suitable for
// production: three consecutive errors trigger a sixty-second
// cooldown. These numbers are deliberately small; a flaky agent
// should pause quickly so the deterministic core is not bombarded
// with junk proposals.
func DefaultErrorBudget() ErrorBudget {
	return ErrorBudget{MaxConsecutiveErrors: 3, Cooldown: 60 * time.Second}
}

// Pipeline runs a Planner followed by zero or more Reviewers in
// non-decreasing Role order, enforces an error budget, and exposes a
// manual operator disable. It is the canonical implementation of the
// "linear, ordered agent pipeline" element of the pattern (§11.5 of
// AGENTIC_ENVELOPE.md).
//
// Pipeline is safe for concurrent use, but a single Run executes all
// of its agents sequentially — concurrent Run calls run independent
// pipelines against shared error-budget state. The expected use is
// one sidecar goroutine driving Run on a schedule.
type Pipeline struct {
	planner   Planner
	reviewers []Reviewer
	budget    ErrorBudget

	now func() time.Time

	mu                sync.Mutex
	consecutiveErrors int
	disabledUntil     time.Time // zero means not auto-disabled
	manualDisable     bool
}

// PipelineOption configures a Pipeline at construction time.
type PipelineOption func(*Pipeline)

// WithErrorBudget sets the pipeline's error budget. Without this
// option the budget is the zero value (no automatic disable).
func WithErrorBudget(b ErrorBudget) PipelineOption {
	return func(p *Pipeline) { p.budget = b }
}

// errAgentMisorder is returned by NewPipeline when the supplied
// reviewers are not in non-decreasing Role order. It is a sentinel
// for tests; production callers should treat any error from
// NewPipeline as fatal.
var errAgentMisorder = errors.New("envelope: pipeline reviewers must be in non-decreasing role order")

// NewPipeline constructs a pipeline with the given Planner and
// Reviewers. Reviewers must be supplied in non-decreasing Role order
// (Verifier before Safety before Observer); duplicate roles are
// allowed because an operator may want, e.g., two safety reviewers
// composing different checks.
//
// A nil Planner is rejected: the pattern requires exactly one
// producer slot. A pipeline with a Planner and zero Reviewers is
// legal and useful for tests and minimal deployments.
func NewPipeline(planner Planner, reviewers []Reviewer, opts ...PipelineOption) (*Pipeline, error) {
	if planner == nil {
		return nil, errors.New("envelope: pipeline requires a non-nil Planner")
	}
	if planner.Role() != RolePlanner {
		return nil, fmt.Errorf("envelope: planner agent %q has role %s, want planner",
			planner.Name(), planner.Role())
	}
	prev := RolePlanner
	for i, r := range reviewers {
		if r == nil {
			return nil, fmt.Errorf("envelope: reviewer at index %d is nil", i)
		}
		if r.Role() == RolePlanner {
			return nil, fmt.Errorf("envelope: reviewer at index %d has role planner; only one planner is allowed", i)
		}
		if r.Role() < prev {
			return nil, fmt.Errorf("envelope: reviewer at index %d has role %s after %s: %w",
				i, r.Role(), prev, errAgentMisorder)
		}
		prev = r.Role()
	}
	p := &Pipeline{
		planner:   planner,
		reviewers: append([]Reviewer(nil), reviewers...),
		now:       time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	return p, nil
}

// Disable sets the operator manual-disable flag. While the flag is
// set every Run returns OutcomeDisabled regardless of error-budget
// state. Use Resume to clear it.
func (p *Pipeline) Disable() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.manualDisable = true
}

// Resume clears both the operator manual-disable flag and the
// error-budget cooldown. The next Run runs agents again. Resume on a
// pipeline that is not disabled is a safe no-op.
func (p *Pipeline) Resume() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.manualDisable = false
	p.consecutiveErrors = 0
	p.disabledUntil = time.Time{}
}

// Disabled reports whether the next Run would short-circuit to
// OutcomeDisabled. Useful for sidecar metrics and tests.
func (p *Pipeline) Disabled() bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isDisabledLocked(p.timeNow())
}

func (p *Pipeline) isDisabledLocked(now time.Time) bool {
	if p.manualDisable {
		return true
	}
	if p.disabledUntil.IsZero() {
		return false
	}
	return now.Before(p.disabledUntil)
}

// Run executes one cycle of the pipeline. The order is:
//
//  1. context cancellation              → OutcomeError, role planner
//  2. operator/error-budget disable     → OutcomeDisabled
//  3. planner.Plan                      → OutcomeNoProposal | OutcomeError
//                                          | proceed with proposal
//  4. for each reviewer in order:
//     reviewer.Review                   → OutcomeVeto | OutcomeError
//                                          | proceed
//  5. all reviewers passed              → OutcomeEmit
//
// Run never panics on a nil Pipeline; it returns OutcomeDisabled.
func (p *Pipeline) Run(ctx context.Context) PipelineOutcome {
	if p == nil {
		return PipelineOutcome{Kind: OutcomeDisabled, Reason: "nil_pipeline"}
	}
	now := p.timeNow()

	if err := ctx.Err(); err != nil {
		return p.recordError(PipelineOutcome{
			Kind:      OutcomeError,
			Role:      RolePlanner,
			AgentName: p.planner.Name(),
			Reason:    "context_cancelled",
			Detail:    err.Error(),
			Err:       err,
			DecidedAt: now,
		}, now)
	}

	p.mu.Lock()
	disabled := p.isDisabledLocked(now)
	disabledUntil := p.disabledUntil
	manual := p.manualDisable
	p.mu.Unlock()
	if disabled {
		reason := "error_budget_exhausted"
		if manual {
			reason = "operator_disabled"
		}
		return PipelineOutcome{
			Kind:          OutcomeDisabled,
			Reason:        reason,
			DisabledUntil: disabledUntil,
			DecidedAt:     now,
		}
	}

	plan, err := p.planner.Plan(ctx)
	if err != nil {
		return p.recordError(PipelineOutcome{
			Kind:      OutcomeError,
			Role:      RolePlanner,
			AgentName: p.planner.Name(),
			Reason:    "planner_error",
			Detail:    err.Error(),
			Err:       err,
			DecidedAt: now,
		}, now)
	}
	if !plan.Emit {
		p.recordSuccess()
		return PipelineOutcome{
			Kind:      OutcomeNoProposal,
			Role:      RolePlanner,
			AgentName: p.planner.Name(),
			Reason:    plan.Reason,
			DecidedAt: now,
		}
	}

	proposal := plan.Proposal
	for _, r := range p.reviewers {
		if err := ctx.Err(); err != nil {
			return p.recordError(PipelineOutcome{
				Kind:      OutcomeError,
				Role:      r.Role(),
				AgentName: r.Name(),
				Reason:    "context_cancelled",
				Detail:    err.Error(),
				Err:       err,
				DecidedAt: p.timeNow(),
			}, p.timeNow())
		}
		result, err := r.Review(ctx, proposal)
		if err != nil {
			return p.recordError(PipelineOutcome{
				Kind:      OutcomeError,
				Role:      r.Role(),
				AgentName: r.Name(),
				Reason:    "reviewer_error",
				Detail:    err.Error(),
				Err:       err,
				DecidedAt: p.timeNow(),
			}, p.timeNow())
		}
		if result.Veto {
			p.recordSuccess()
			return PipelineOutcome{
				Kind:      OutcomeVeto,
				Role:      r.Role(),
				AgentName: r.Name(),
				Reason:    result.Reason,
				Detail:    result.Detail,
				DecidedAt: p.timeNow(),
			}
		}
	}

	p.recordSuccess()
	return PipelineOutcome{
		Kind:      OutcomeEmit,
		Proposal:  proposal,
		Role:      RolePlanner,
		AgentName: p.planner.Name(),
		DecidedAt: p.timeNow(),
	}
}

// recordError accumulates an error against the budget and, if the
// budget is exhausted, sets DisabledUntil on the returned outcome.
// The returned outcome is the one to return from Run; recordError
// rewrites only DisabledUntil when relevant.
func (p *Pipeline) recordError(out PipelineOutcome, now time.Time) PipelineOutcome {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.consecutiveErrors++
	if p.budget.MaxConsecutiveErrors > 0 && p.consecutiveErrors >= p.budget.MaxConsecutiveErrors {
		if p.budget.Cooldown > 0 {
			p.disabledUntil = now.Add(p.budget.Cooldown)
		} else {
			// Zero cooldown means stay disabled until Resume.
			// time.Time{}.IsZero is reserved to mean "not auto-disabled",
			// so we use the far-future sentinel time.Unix(1<<62, 0).
			p.disabledUntil = farFuture
		}
		out.DisabledUntil = p.disabledUntil
	}
	return out
}

// recordSuccess resets the consecutive-error counter after a run
// that did not produce an OutcomeError. Veto, NoProposal, and Emit
// all count as successful runs from the budget's perspective —
// the agents disagreed but did not fail.
func (p *Pipeline) recordSuccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.consecutiveErrors = 0
}

func (p *Pipeline) timeNow() time.Time {
	if p == nil || p.now == nil {
		return time.Now()
	}
	return p.now()
}

// farFuture is the disabledUntil sentinel for "stay disabled until
// the operator calls Resume". It is far enough in the future that
// time.Now().Before(farFuture) is true for any plausible runtime.
var farFuture = time.Unix(1<<62, 0)

// PlannerFunc is a function-typed adapter for Planner. Useful for
// tests and for trivial planners that have no per-instance state.
// Name and Role are fields on the value, not the function.
type PlannerFunc struct {
	NameValue string
	Fn        func(ctx context.Context) (PlanResult, error)
}

// Role implements Agent.
func (PlannerFunc) Role() Role { return RolePlanner }

// Name implements Agent.
func (p PlannerFunc) Name() string {
	if p.NameValue == "" {
		return "anonymous_planner"
	}
	return p.NameValue
}

// Plan implements Planner.
func (p PlannerFunc) Plan(ctx context.Context) (PlanResult, error) {
	if p.Fn == nil {
		return PlanResult{Emit: false, Reason: "no_planner_fn"}, nil
	}
	return p.Fn(ctx)
}

// ReviewerFunc is a function-typed adapter for Reviewer. The
// RoleValue field carries the reviewer's role; misconfiguring it
// (e.g., RolePlanner) is a programmer error caught by NewPipeline.
type ReviewerFunc struct {
	NameValue string
	RoleValue Role
	Fn        func(ctx context.Context, p Proposal) (ReviewResult, error)
}

// Role implements Agent.
func (r ReviewerFunc) Role() Role { return r.RoleValue }

// Name implements Agent.
func (r ReviewerFunc) Name() string {
	if r.NameValue == "" {
		return "anonymous_reviewer"
	}
	return r.NameValue
}

// Review implements Reviewer.
func (r ReviewerFunc) Review(ctx context.Context, p Proposal) (ReviewResult, error) {
	if r.Fn == nil {
		return ReviewResult{}, nil
	}
	return r.Fn(ctx, p)
}
