package envelope

import "time"

// ProposalKind classifies what aspect of gateway configuration a proposal
// would change. The list is intentionally small and explicit; new kinds are
// added only when a corresponding handler exists in the deterministic core.
type ProposalKind string

const (
	KindRateLimit      ProposalKind = "rate_limit"
	KindCircuitBreaker ProposalKind = "circuit_breaker"
	KindRouteWeight    ProposalKind = "route_weight"
	KindCacheTTL       ProposalKind = "cache_ttl"
)

// Proposal is an immutable description of a desired configuration change
// emitted by an agent or agent pipeline. Proposals never mutate the gateway
// directly; they enter the Envelope through Envelope.Submit and reach the
// data path only after a Decision of DecisionApply.
//
// Proposals are value types and must be safe to log, hash, and compare.
type Proposal struct {
	// Kind identifies which configuration parameter this proposal targets.
	Kind ProposalKind

	// Target is a stable identifier for the specific resource being changed
	// (for example, the route prefix "/api/users" or the backend URL of a
	// circuit breaker). Empty Target means the change is global.
	Target string

	// Value is the proposed new value. Interpretation depends on Kind. The
	// caller is responsible for ensuring the type matches what the deterministic
	// core expects for that Kind; the Envelope only inspects it after
	// constraints and bounds have been checked.
	Value any

	// Agent is a stable identifier for the agent (or pipeline) that produced
	// the proposal. Used for audit lineage and per-agent rate limiting.
	Agent string

	// Reason is a human-readable justification. The Envelope does not
	// interpret this; it is logged with the Decision.
	Reason string

	// SubmittedAt is the time the proposal entered the Envelope. Set by
	// Envelope.Submit; callers should leave it zero.
	SubmittedAt time.Time
}

// DecisionKind is the terminal verdict the Envelope returns for a Proposal.
type DecisionKind int

const (
	// DecisionReject means the proposal was rejected. It must not be
	// resubmitted unchanged. The Reason field on the Decision explains why.
	DecisionReject DecisionKind = iota

	// DecisionDefer means the proposal is currently inadmissible (typically
	// because of a cooldown or shadow timeout) but may be resubmitted after
	// the RetryAfter duration has elapsed.
	DecisionDefer

	// DecisionApply means the proposal passed every stage of the Envelope
	// and has been (or will be) applied to the deterministic control plane.
	DecisionApply
)

// String returns a human-readable decision name.
func (d DecisionKind) String() string {
	switch d {
	case DecisionReject:
		return "reject"
	case DecisionDefer:
		return "defer"
	case DecisionApply:
		return "apply"
	default:
		return "unknown"
	}
}

// Decision is the Envelope's response to a Proposal. It is structured rather
// than a bare boolean so that audit logs and agent feedback have full
// lineage: which stage rejected, why, and when the proposal may be retried.
type Decision struct {
	Kind DecisionKind

	// Stage names which Envelope stage produced this decision (for example,
	// "constraints", "bounds", "dampener", "shadow"). Empty for DecisionApply.
	Stage string

	// Reason is a short, machine-stable code or human-readable explanation
	// for a Reject or Defer. Empty for DecisionApply.
	Reason string

	// RetryAfter is meaningful only for DecisionDefer; it is the minimum
	// duration the agent must wait before resubmitting.
	RetryAfter time.Duration

	// DecidedAt is the time the Envelope produced this decision.
	DecidedAt time.Time
}
