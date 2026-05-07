package envelope

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestShadowRegistry_NilIsSafe(t *testing.T) {
	t.Parallel()

	var r *ShadowRegistry
	if o := r.Evaluate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}); o != nil {
		t.Fatalf("nil registry must not stop a proposal, got %+v", o)
	}
	// Mutators on a nil receiver must be safe no-ops.
	r.Enable(KindRateLimit)
	r.Disable(KindRateLimit)
	r.SetScorer(DefaultSLOScorer(time.Second, 0.01))
	if r.Enabled(KindRateLimit) {
		t.Fatal("nil registry must report disabled for every kind")
	}
	if r.Timeout() != 0 {
		t.Fatal("nil registry timeout must be zero")
	}
}

func TestShadowRegistry_DisabledKindPasses(t *testing.T) {
	t.Parallel()

	called := false
	sim := SimulatorFunc(func(context.Context, Proposal) (*ShadowVerdict, error) {
		called = true
		return &ShadowVerdict{Regressions: []string{"latency"}}, nil
	})
	r := NewShadowRegistry(sim, time.Second)

	// Kind is not enabled; registry must skip the simulator entirely.
	if o := r.Evaluate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}); o != nil {
		t.Fatalf("disabled kind must pass, got %+v", o)
	}
	if called {
		t.Fatal("simulator must not run for a disabled kind")
	}
}

func TestShadowRegistry_EnabledKindRunsSimulator(t *testing.T) {
	t.Parallel()

	var calls int32
	sim := SimulatorFunc(func(context.Context, Proposal) (*ShadowVerdict, error) {
		atomic.AddInt32(&calls, 1)
		return &ShadowVerdict{SamplesReplayed: 5}, nil
	})
	r := NewShadowRegistry(sim, time.Second)
	r.Enable(KindRateLimit)

	if o := r.Evaluate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}); o != nil {
		t.Fatalf("zero-regression verdict must pass, got %+v", o)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 simulator call, got %d", got)
	}
}

func TestShadowRegistry_RegressionVerdictRejects(t *testing.T) {
	t.Parallel()

	sim := SimulatorFunc(func(context.Context, Proposal) (*ShadowVerdict, error) {
		return &ShadowVerdict{
			SamplesReplayed: 1000,
			Score:           0.42,
			Regressions:     []string{"error_rate", "latency_p99"},
		}, nil
	})
	r := NewShadowRegistry(sim, time.Second)
	r.Enable(KindRateLimit)

	o := r.Evaluate(context.Background(), Proposal{
		Kind: KindRateLimit, Target: "/api/users", Agent: "x", Value: 100,
	})
	if o == nil {
		t.Fatal("regression verdict must produce an outcome")
	}
	if o.Reason != "regression" {
		t.Fatalf("expected reason=regression, got %+v", o)
	}
	if o.Kind != KindRateLimit || o.Target != "/api/users" {
		t.Fatalf("outcome must echo Kind and Target, got %+v", o)
	}
	if !strings.Contains(o.Detail, "error_rate") || !strings.Contains(o.Detail, "latency_p99") {
		t.Fatalf("detail must list every regressed dimension, got %q", o.Detail)
	}
	if o.Verdict == nil || o.Verdict.SamplesReplayed != 1000 {
		t.Fatalf("verdict must be attached to the outcome, got %+v", o.Verdict)
	}
	if o.RetryAfter != 0 {
		t.Fatalf("regression must not set RetryAfter, got %s", o.RetryAfter)
	}
}

func TestShadowRegistry_ScorerProducesRegression(t *testing.T) {
	t.Parallel()

	sim := SimulatorFunc(func(context.Context, Proposal) (*ShadowVerdict, error) {
		return &ShadowVerdict{
			Latency:   LatencyStats{P99: 80 * time.Millisecond},
			ErrorRate: 0.005,
			Baseline: ShadowBaseline{
				Latency:   LatencyStats{P99: 30 * time.Millisecond},
				ErrorRate: 0.001,
			},
		}, nil
	})
	r := NewShadowRegistry(sim, time.Second)
	r.Enable(KindRateLimit)
	// 20ms is the maximum tolerated P99 increase; the verdict shows +50ms.
	r.SetScorer(DefaultSLOScorer(20*time.Millisecond, 0.01))

	o := r.Evaluate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	if o == nil {
		t.Fatal("scorer-detected regression must produce an outcome")
	}
	if o.Reason != "regression" {
		t.Fatalf("expected reason=regression, got %+v", o)
	}
	if !strings.Contains(o.Detail, "latency_p99") {
		t.Fatalf("scorer must name the regressed dimension, got %q", o.Detail)
	}
}

func TestShadowRegistry_ScorerWithNoBaselineSkipsChecks(t *testing.T) {
	t.Parallel()

	sim := SimulatorFunc(func(context.Context, Proposal) (*ShadowVerdict, error) {
		return &ShadowVerdict{
			Latency:   LatencyStats{P99: 5 * time.Second}, // would regress if there were a baseline
			ErrorRate: 0.5,
		}, nil
	})
	r := NewShadowRegistry(sim, time.Second)
	r.Enable(KindRateLimit)
	r.SetScorer(DefaultSLOScorer(20*time.Millisecond, 0.01))

	if o := r.Evaluate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}); o != nil {
		t.Fatalf("zero-baseline must skip scorer checks, got %+v", o)
	}
}

func TestShadowRegistry_TimeoutDefers(t *testing.T) {
	t.Parallel()

	sim := SimulatorFunc(func(ctx context.Context, _ Proposal) (*ShadowVerdict, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	timeout := 25 * time.Millisecond
	r := NewShadowRegistry(sim, timeout)
	r.Enable(KindRateLimit)

	start := time.Now()
	o := r.Evaluate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	elapsed := time.Since(start)

	if o == nil {
		t.Fatal("timeout must produce an outcome")
	}
	if o.Reason != "timeout" {
		t.Fatalf("expected reason=timeout, got %+v", o)
	}
	if o.RetryAfter != timeout {
		t.Fatalf("RetryAfter = %s, want %s", o.RetryAfter, timeout)
	}
	if elapsed < timeout {
		t.Fatalf("Evaluate returned before the configured timeout: elapsed=%s timeout=%s", elapsed, timeout)
	}
}

func TestShadowRegistry_SimulatorErrorIsInternalError(t *testing.T) {
	t.Parallel()

	boom := errors.New("replay engine exploded")
	sim := SimulatorFunc(func(context.Context, Proposal) (*ShadowVerdict, error) {
		return nil, boom
	})
	r := NewShadowRegistry(sim, time.Second)
	r.Enable(KindRateLimit)

	o := r.Evaluate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	if o == nil {
		t.Fatal("simulator error must produce an outcome")
	}
	if o.Reason != "simulator_internal_error" {
		t.Fatalf("expected reason=simulator_internal_error, got %+v", o)
	}
	if !strings.Contains(o.Detail, "replay engine exploded") {
		t.Fatalf("detail must include the simulator error, got %q", o.Detail)
	}
}

func TestShadowRegistry_NilVerdictPasses(t *testing.T) {
	t.Parallel()

	sim := SimulatorFunc(func(context.Context, Proposal) (*ShadowVerdict, error) {
		return nil, nil
	})
	r := NewShadowRegistry(sim, time.Second)
	r.Enable(KindRateLimit)

	if o := r.Evaluate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}); o != nil {
		t.Fatalf("nil verdict with nil error must pass, got %+v", o)
	}
}

func TestShadowRegistry_NilSimulatorReplacedWithNoop(t *testing.T) {
	t.Parallel()

	r := NewShadowRegistry(nil, time.Second)
	r.Enable(KindRateLimit)

	if o := r.Evaluate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}); o != nil {
		t.Fatalf("nil simulator must be replaced with NoopSimulator, got %+v", o)
	}
}

func TestShadowRegistry_DisableUnEnables(t *testing.T) {
	t.Parallel()

	called := false
	sim := SimulatorFunc(func(context.Context, Proposal) (*ShadowVerdict, error) {
		called = true
		return &ShadowVerdict{Regressions: []string{"x"}}, nil
	})
	r := NewShadowRegistry(sim, time.Second)
	r.Enable(KindRateLimit)
	r.Disable(KindRateLimit)

	if r.Enabled(KindRateLimit) {
		t.Fatal("Disable must clear Enable")
	}
	if o := r.Evaluate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100}); o != nil {
		t.Fatalf("disabled kind must pass, got %+v", o)
	}
	if called {
		t.Fatal("simulator must not run after Disable")
	}
}

func TestShadowOutcome_ErrorFormat(t *testing.T) {
	t.Parallel()

	with := &ShadowOutcome{Kind: KindRateLimit, Reason: "regression", Detail: "latency_p99(+30ms)"}
	if got := with.Error(); got != "shadow(rate_limit): regression (latency_p99(+30ms))" {
		t.Fatalf("unexpected formatted error: %q", got)
	}
	without := &ShadowOutcome{Kind: KindCacheTTL, Reason: "timeout"}
	if got := without.Error(); got != "shadow(cache_ttl): timeout" {
		t.Fatalf("unexpected formatted error: %q", got)
	}
	var nilo *ShadowOutcome
	if got := nilo.Error(); got == "" {
		t.Fatal("nil outcome.Error() must not be empty")
	}
}

func TestNoopSimulator_AlwaysPasses(t *testing.T) {
	t.Parallel()

	v, err := (NoopSimulator{}).Simulate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	if err != nil {
		t.Fatalf("NoopSimulator must not error, got %v", err)
	}
	if v == nil {
		t.Fatal("NoopSimulator must return a non-nil verdict")
	}
	if v.SamplesReplayed != 0 || v.Score != 0 || len(v.Regressions) != 0 {
		t.Fatalf("NoopSimulator verdict must be the zero value, got %+v", v)
	}
}

func TestSimulatorFunc_NilSafe(t *testing.T) {
	t.Parallel()

	var f SimulatorFunc
	v, err := f.Simulate(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	if err != nil || v != nil {
		t.Fatalf("nil SimulatorFunc must return (nil, nil); got (%+v, %v)", v, err)
	}
}

func TestSLOScorerFunc_NilSafe(t *testing.T) {
	t.Parallel()

	var f SLOScorerFunc
	if rs := f.Score(&ShadowVerdict{}); rs != nil {
		t.Fatalf("nil SLOScorerFunc must return nil, got %v", rs)
	}
}

func TestDefaultSLOScorer_ZeroThresholdsDisable(t *testing.T) {
	t.Parallel()

	scorer := DefaultSLOScorer(0, 0)
	v := &ShadowVerdict{
		Latency:   LatencyStats{P99: 5 * time.Second},
		ErrorRate: 0.99,
		Baseline:  ShadowBaseline{Latency: LatencyStats{P99: time.Millisecond}, ErrorRate: 0},
	}
	if rs := scorer.Score(v); len(rs) != 0 {
		t.Fatalf("zero thresholds must disable every check; got %v", rs)
	}
}

func TestDefaultSLOScorer_ErrorRateRegression(t *testing.T) {
	t.Parallel()

	scorer := DefaultSLOScorer(0, 0.01)
	v := &ShadowVerdict{
		ErrorRate: 0.05,
		Baseline:  ShadowBaseline{ErrorRate: 0.01},
	}
	rs := scorer.Score(v)
	if len(rs) != 1 || !strings.Contains(rs[0], "error_rate") {
		t.Fatalf("expected an error_rate regression, got %v", rs)
	}
}

func TestShadowRegistry_PreCancelledContextIsTimeout(t *testing.T) {
	t.Parallel()

	sim := SimulatorFunc(func(ctx context.Context, _ Proposal) (*ShadowVerdict, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	r := NewShadowRegistry(sim, 100*time.Millisecond)
	r.Enable(KindRateLimit)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	o := r.Evaluate(ctx, Proposal{Kind: KindRateLimit, Agent: "x", Value: 100})
	if o == nil {
		t.Fatal("cancelled ctx must produce an outcome from the simulator path")
	}
	if o.Reason != "timeout" {
		t.Fatalf("expected reason=timeout for cancellation, got %+v", o)
	}
}
