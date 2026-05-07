package envelope

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ShadowOutcome is the structured result returned when a proposal is
// stopped by the shadow stage. It mirrors *ConstraintViolation,
// *BoundsViolation, and *DampenerOutcome in shape so audit consumers
// can treat all four stages uniformly. A nil outcome means the
// proposal passed.
//
// The shadow stage emits two distinct outcomes:
//
//   - regression — the simulator replayed traffic against the proposed
//     configuration and the SLO scorer detected a regression
//     (latency, error rate, or cost). The Envelope translates this
//     into a DecisionReject because the agent must observe a different
//     world before resubmitting; resubmitting the same value would
//     fail again.
//   - timeout — the simulator did not produce a verdict within the
//     configured hard timeout. The Envelope translates this into a
//     DecisionDefer with RetryAfter set to the timeout. A timeout is
//     not a rejection; it is "the system was too busy to evaluate
//     you, try later when load drops" — see §8 of AGENTIC_ENVELOPE.md.
//
// A simulator that returns a non-verdict error (genuine programmer
// error, replay panic, etc.) produces a synthetic outcome with reason
// "simulator_internal_error". The Envelope still rejects, but the
// stage and reason make the failure mode legible in audit logs.
type ShadowOutcome struct {
	// Kind is the proposal kind that produced the outcome.
	Kind ProposalKind

	// Target is the proposal target. Empty for global proposals.
	Target string

	// Reason is a stable machine-readable code: "regression",
	// "timeout", or "simulator_internal_error".
	Reason string

	// Detail is an optional human-readable elaboration; safe to log,
	// not part of the stable contract.
	Detail string

	// Verdict carries the simulator's structured score. May be nil for
	// timeouts and internal errors where no verdict was produced.
	Verdict *ShadowVerdict

	// RetryAfter is meaningful only for "timeout"; it is the minimum
	// duration the agent should wait before resubmitting.
	RetryAfter time.Duration
}

// Error implements the error interface. The format is stable and is
// what the Envelope places in Decision.Reason for a shadow outcome:
// "shadow(<kind>): <reason>" optionally followed by detail.
func (o *ShadowOutcome) Error() string {
	if o == nil {
		return "envelope: nil shadow outcome"
	}
	if o.Detail == "" {
		return fmt.Sprintf("shadow(%s): %s", o.Kind, o.Reason)
	}
	return fmt.Sprintf("shadow(%s): %s (%s)", o.Kind, o.Reason, o.Detail)
}

// ShadowVerdict is the structured score a Simulator returns after
// replaying traffic against a proposed configuration. It is recorded
// on the ShadowOutcome (when one is produced) and also surfaced to
// audit logs on DecisionApply so reviewers can see exactly what the
// simulator measured.
//
// The Envelope is intentionally agnostic about how Score is computed;
// the SLOScorer pattern is the recommended composition (see
// DefaultSLOScorer below) but a Simulator may produce a Score by any
// means. The contract is only that Score == 0 means "no measured
// regression" and any positive Score names which SLO regressed.
type ShadowVerdict struct {
	// SamplesReplayed is the number of traffic samples the simulator
	// evaluated. Zero is legal — it means there was no captured
	// traffic to replay against this proposal — and is treated as a
	// pass (the autonomous-safe default still rejects at fallback).
	SamplesReplayed int

	// Score is the SLO regression score. Zero means no regression.
	// Any non-zero value, plus a non-empty Regressions slice, causes
	// the stage to reject with reason "regression".
	Score float64

	// Regressions names the SLO dimensions that regressed. Used in
	// the rejection detail to make the verdict legible.
	Regressions []string

	// Latency is the replay's measured request latency distribution
	// against the proposed config. Optional; populated by replay
	// engines that measure it. Compared to Baseline by SLOScorer.
	Latency LatencyStats

	// ErrorRate is the fraction of replayed samples that produced an
	// error response (5xx, circuit-open, etc.) under the proposed
	// configuration. In [0.0, 1.0]. Optional.
	ErrorRate float64

	// Baseline is the same set of measurements taken against the
	// current (unchanged) configuration. SLOScorer compares the two
	// distributions to detect regressions; if Baseline is the
	// zero-value the scorer treats every measurement as "no
	// baseline" and only fires on absolute thresholds.
	Baseline ShadowBaseline

	// EvaluatedAt is the wall-clock time the simulator finished.
	EvaluatedAt time.Time
}

// LatencyStats is a small fixed-shape summary of a replay's request
// latency distribution. Percentiles, not full histograms, because the
// Envelope's job is regression detection — not latency analysis.
type LatencyStats struct {
	P50 time.Duration
	P95 time.Duration
	P99 time.Duration
}

// ShadowBaseline is the same shape as ShadowVerdict's measurement
// fields, captured from the unchanged configuration. The SLOScorer
// treats a zero-value Baseline as "no baseline available".
type ShadowBaseline struct {
	Latency   LatencyStats
	ErrorRate float64
}

// Simulator replays a window of recent traffic against the proposed
// configuration in a sandbox and returns a structured verdict. It is
// the only place in the Envelope pipeline that performs real work
// (constraints, bounds, and dampener are all O(1) checks against
// in-memory state); it is also the only stage permitted to take
// non-trivial wall-clock time. The Envelope enforces a hard timeout
// around every Simulate call and converts a timeout into a defer action.
//
// Implementations MUST be read-only with respect to the data path:
// they may not mutate gateway configuration, may not emit metrics
// onto the production registry, and may not perform writes to any
// external system. The simulator runs against a captured copy of
// recent traffic and a sandboxed copy of the proposed configuration.
//
// Simulate must honor ctx.Done(): when the context is canceled the
// simulator must stop work and return ctx.Err() promptly. The
// Envelope cancels the context at the configured timeout.
//
// A nil verdict with a nil error is treated as a pass with zero
// samples replayed.
type Simulator interface {
	Simulate(ctx context.Context, p Proposal) (*ShadowVerdict, error)
}

// SimulatorFunc is the function-typed adapter for Simulator. Useful
// for tests and for simple in-process simulators that don't need
// state.
type SimulatorFunc func(ctx context.Context, p Proposal) (*ShadowVerdict, error)

// Simulate implements Simulator.
func (f SimulatorFunc) Simulate(ctx context.Context, p Proposal) (*ShadowVerdict, error) {
	if f == nil {
		return nil, nil
	}
	return f(ctx, p)
}

// NoopSimulator is the autonomous-safe default Simulator. Every call
// returns a zero-sample, zero-score verdict immediately. It is what a
// gateway should run with until a real replay engine is wired up:
// the shadow stage exists in the pipeline, has a stable contract,
// and never blocks proposals on a simulator that does not yet exist.
type NoopSimulator struct{}

// Simulate returns a pass verdict with zero samples replayed.
func (NoopSimulator) Simulate(_ context.Context, _ Proposal) (*ShadowVerdict, error) {
	return &ShadowVerdict{}, nil
}

// SLOScorer turns a raw verdict (latency, error rate) into a
// regression decision. Implementations look at the verdict's measured
// values, compare them against the verdict's Baseline and any
// absolute thresholds the scorer was configured with, and return a
// list of regressed SLO dimensions. An empty slice means no
// regression.
//
// Scorers must be pure: same verdict, same answer. They run inside
// the Envelope's hard timeout, so they should also be cheap.
type SLOScorer interface {
	Score(v *ShadowVerdict) []string
}

// SLOScorerFunc is the function-typed adapter for SLOScorer.
type SLOScorerFunc func(v *ShadowVerdict) []string

// Score implements SLOScorer.
func (f SLOScorerFunc) Score(v *ShadowVerdict) []string {
	if f == nil {
		return nil
	}
	return f(v)
}

// DefaultSLOScorer returns a scorer that compares a verdict's measured
// values against its Baseline using two operator-authored thresholds:
//
//   - maxLatencyP99Increase: the maximum tolerated increase in P99
//     latency over the baseline. A zero value disables the check.
//   - maxErrorRateIncrease: the maximum tolerated absolute increase
//     in error rate over the baseline (e.g., 0.01 = +1pp). A zero
//     value disables the check.
//
// Both checks are skipped when the verdict's Baseline is the
// zero-value, because there is nothing to compare against. A more
// sophisticated scorer (relative thresholds, statistical tests,
// multi-percentile checks) is a future extension; this one is
// deliberately conservative.
func DefaultSLOScorer(maxLatencyP99Increase time.Duration, maxErrorRateIncrease float64) SLOScorer {
	return SLOScorerFunc(func(v *ShadowVerdict) []string {
		if v == nil {
			return nil
		}
		var regressions []string
		baseline := v.Baseline
		hasBaseline := baseline != (ShadowBaseline{})
		if hasBaseline && maxLatencyP99Increase > 0 {
			delta := v.Latency.P99 - baseline.Latency.P99
			if delta > maxLatencyP99Increase {
				regressions = append(regressions, fmt.Sprintf(
					"latency_p99(+%s)", delta,
				))
			}
		}
		if hasBaseline && maxErrorRateIncrease > 0 {
			delta := v.ErrorRate - baseline.ErrorRate
			if delta > maxErrorRateIncrease {
				regressions = append(regressions, fmt.Sprintf(
					"error_rate(+%.4f)", delta,
				))
			}
		}
		return regressions
	})
}

// ShadowRegistry is the fourth Envelope stage. It owns a Simulator,
// an optional SLOScorer that may post-process the verdict, a hard
// timeout, and a per-Kind enable map.
//
// The registry is opt-in per Kind because the simulator is the most
// expensive stage and not every Kind has a replay strategy yet. A
// proposal whose Kind is not enabled passes the stage immediately
// (subject to the autonomous-safe default at the next stage). This
// is the same shape as the bounds and dampener registries — empty or
// not-enabled means "no opinion at this stage".
//
// The registry is safe for concurrent use. Set* and Enable*
// configuration methods are not safe to call concurrently with
// Evaluate; build the registry at startup and then treat it as
// immutable.
type ShadowRegistry struct {
	mu      sync.RWMutex
	sim     Simulator
	scorer  SLOScorer
	timeout time.Duration
	enabled map[ProposalKind]bool
}

// NewShadowRegistry returns a registry that runs sim against every
// enabled Kind, with the given hard per-evaluation timeout. A nil
// simulator is replaced with NoopSimulator so the stage is a safe
// pass-through. A non-positive timeout is treated as "no timeout"
// for tests; production wiring should always pass a positive timeout.
func NewShadowRegistry(sim Simulator, timeout time.Duration) *ShadowRegistry {
	if sim == nil {
		sim = NoopSimulator{}
	}
	return &ShadowRegistry{
		sim:     sim,
		timeout: timeout,
		enabled: make(map[ProposalKind]bool),
	}
}

// SetScorer installs an SLOScorer that may upgrade a simulator's raw
// verdict into a regression. A simulator that already populates
// Verdict.Regressions does not need a scorer; SetScorer composes
// with that — both sources of regressions are merged.
func (r *ShadowRegistry) SetScorer(s SLOScorer) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scorer = s
}

// Enable opts the given Kind into the shadow stage. Calling Enable
// for a Kind that is already enabled is a no-op.
func (r *ShadowRegistry) Enable(kind ProposalKind) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.enabled == nil {
		r.enabled = make(map[ProposalKind]bool)
	}
	r.enabled[kind] = true
}

// Disable opts the given Kind back out of the shadow stage. The
// registry continues to exist; only this Kind is bypassed.
func (r *ShadowRegistry) Disable(kind ProposalKind) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.enabled != nil {
		delete(r.enabled, kind)
	}
}

// Enabled reports whether the registry will run the simulator for
// proposals of the given Kind. Useful for tests and for boot-time
// logging.
func (r *ShadowRegistry) Enabled(kind ProposalKind) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enabled[kind]
}

// Timeout returns the configured per-evaluation hard timeout.
func (r *ShadowRegistry) Timeout() time.Duration {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.timeout
}

// Evaluate runs the simulator against p and returns nil on a pass or
// a *ShadowOutcome on a stop. The Envelope translates a non-nil
// outcome into a DecisionReject (regression, internal error) or
// DecisionDefer (timeout).
//
// ctx is the caller's context; the registry derives a child context
// with the configured timeout. A timeout produces a "timeout"
// outcome with RetryAfter equal to the timeout — the agent should
// resubmit no sooner than that. A pre-canceled ctx is honored as a
// timeout as well.
//
// A simulator that returns a non-nil error other than
// context.DeadlineExceeded or context.Canceled is reported as
// "simulator_internal_error". This keeps the rejection path uniform.
func (r *ShadowRegistry) Evaluate(ctx context.Context, p Proposal) *ShadowOutcome {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	enabled := r.enabled[p.Kind]
	sim := r.sim
	scorer := r.scorer
	timeout := r.timeout
	r.mu.RUnlock()

	if !enabled || sim == nil {
		return nil
	}

	simCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		simCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	verdict, err := sim.Simulate(simCtx, p)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return &ShadowOutcome{
				Kind:       p.Kind,
				Target:     p.Target,
				Reason:     "timeout",
				Detail:     fmt.Sprintf("simulator did not return within %s", timeout),
				RetryAfter: timeout,
			}
		}
		return &ShadowOutcome{
			Kind:   p.Kind,
			Target: p.Target,
			Reason: "simulator_internal_error",
			Detail: err.Error(),
		}
	}
	if verdict == nil {
		return nil
	}

	regressions := append([]string(nil), verdict.Regressions...)
	if scorer != nil {
		regressions = append(regressions, scorer.Score(verdict)...)
	}
	if len(regressions) == 0 {
		return nil
	}
	return &ShadowOutcome{
		Kind:    p.Kind,
		Target:  p.Target,
		Reason:  "regression",
		Detail:  joinRegressions(regressions),
		Verdict: verdict,
	}
}

func joinRegressions(rs []string) string {
	if len(rs) == 0 {
		return ""
	}
	out := rs[0]
	for _, r := range rs[1:] {
		out += ", " + r
	}
	return out
}
