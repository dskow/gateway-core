package circuitbreaker

import (
	"log/slog"
	"testing"
	"time"
)

func TestComposite_BasicFailureRate(t *testing.T) {
	cfg := Config{
		WindowSize:       4,
		FailureThreshold: 0.5,
		ResetTimeout:     10 * time.Millisecond,
		HalfOpenMax:      1,
	}
	cb := NewComposite("http://test:8080", cfg, slog.Default(), nil)

	if cb.State() != StateClosed {
		t.Fatal("expected StateClosed")
	}

	// Trip it.
	cb.RecordFailure(10 * time.Millisecond)
	cb.RecordFailure(10 * time.Millisecond)
	cb.RecordFailure(10 * time.Millisecond)
	cb.RecordFailure(10 * time.Millisecond)

	if cb.State() != StateOpen {
		t.Fatalf("expected StateOpen, got %v", cb.State())
	}

	if cb.Allow() {
		t.Fatal("expected rejection from open breaker")
	}

	// Release is a no-op without bulkhead.
	cb.Release()
}

func TestComposite_WithTimeoutBreaker(t *testing.T) {
	cfg := Config{
		WindowSize:       4,
		FailureThreshold: 0.5,
		ResetTimeout:     30 * time.Second,
		HalfOpenMax:      2,
		SlowThreshold:    50 * time.Millisecond,
	}
	cb := NewComposite("http://test:8080", cfg, slog.Default(), nil)

	// All "successes" but slow — should count as failures.
	cb.RecordSuccess(100 * time.Millisecond)
	cb.RecordSuccess(100 * time.Millisecond)
	cb.RecordSuccess(100 * time.Millisecond)
	cb.RecordSuccess(100 * time.Millisecond)

	if cb.State() != StateOpen {
		t.Fatalf("expected StateOpen from slow successes, got %v", cb.State())
	}
}

func TestComposite_WithBulkhead(t *testing.T) {
	cfg := Config{
		WindowSize:       10,
		FailureThreshold: 0.9,
		ResetTimeout:     30 * time.Second,
		HalfOpenMax:      2,
		MaxConcurrent:    2,
	}
	cb := NewComposite("http://test:8080", cfg, slog.Default(), nil)

	// Acquire 2 slots.
	if !cb.Allow() {
		t.Fatal("expected Allow() for slot 1")
	}
	if !cb.Allow() {
		t.Fatal("expected Allow() for slot 2")
	}

	// 3rd should be rejected.
	if cb.Allow() {
		t.Fatal("expected rejection at concurrency limit")
	}

	// Release one and try again.
	cb.Release()
	if !cb.Allow() {
		t.Fatal("expected Allow() after Release()")
	}

	// Clean up remaining slots.
	cb.Release()
	cb.Release()
}

func TestComposite_WithAdaptive(t *testing.T) {
	cfg := Config{
		WindowSize:       4,
		FailureThreshold: 0.5,
		ResetTimeout:     30 * time.Second,
		HalfOpenMax:      2,
		Adaptive:         true,
		LatencyCeiling:   50 * time.Millisecond,
		MinThreshold:     0.1,
	}
	cb := NewComposite("http://test:8080", cfg, slog.Default(), nil)

	// Send high-latency calls to trigger adaptive tightening.
	cb.RecordSuccess(200 * time.Millisecond)
	cb.RecordSuccess(200 * time.Millisecond)
	cb.RecordSuccess(200 * time.Millisecond)
	cb.RecordSuccess(200 * time.Millisecond)

	// Breaker should still function (no panic, state valid).
	st := cb.State()
	if st != StateClosed && st != StateOpen {
		t.Fatalf("unexpected state: %v", st)
	}
}

func TestComposite_UpdateConfig(t *testing.T) {
	cfg := Config{
		WindowSize:       4,
		FailureThreshold: 0.5,
		ResetTimeout:     30 * time.Second,
		HalfOpenMax:      2,
	}
	cb := NewComposite("http://test:8080", cfg, slog.Default(), nil)

	newCfg := Config{
		WindowSize:       8,
		FailureThreshold: 0.8,
		ResetTimeout:     10 * time.Second,
		HalfOpenMax:      3,
	}
	cb.UpdateConfig(newCfg)

	// Verify the window was resized by filling it.
	for i := 0; i < 8; i++ {
		cb.RecordFailure(10 * time.Millisecond)
	}
	// With threshold 0.8 and 8/8 failures (1.0), should be open.
	if cb.State() != StateOpen {
		t.Fatalf("expected StateOpen after config update, got %v", cb.State())
	}
}

func TestComposite_EffectiveState_NoBulkhead(t *testing.T) {
	cfg := Config{
		WindowSize:       4,
		FailureThreshold: 0.5,
		ResetTimeout:     30 * time.Second,
		HalfOpenMax:      2,
	}
	cb := NewComposite("http://test:8080", cfg, slog.Default(), nil)

	if got := cb.EffectiveState(); got != StateClosed {
		t.Fatalf("EffectiveState closed, got %v", got)
	}
	if got := cb.InnerState(); got != StateClosed {
		t.Fatalf("InnerState closed, got %v", got)
	}

	// Trip the inner breaker — both states should follow.
	for i := 0; i < 4; i++ {
		cb.RecordFailure(10 * time.Millisecond)
	}
	if got := cb.InnerState(); got != StateOpen {
		t.Fatalf("InnerState open after trip, got %v", got)
	}
	if got := cb.EffectiveState(); got != StateOpen {
		t.Fatalf("EffectiveState open after trip, got %v", got)
	}
}

func TestComposite_EffectiveState_BulkheadAtCapacity(t *testing.T) {
	cfg := Config{
		WindowSize:       10,
		FailureThreshold: 0.9,
		ResetTimeout:     30 * time.Second,
		HalfOpenMax:      2,
		MaxConcurrent:    2,
	}
	cb := NewComposite("http://test:8080", cfg, slog.Default(), nil)

	// Inner breaker closed, bulkhead has the slack → both states closed.
	if got := cb.InnerState(); got != StateClosed {
		t.Fatalf("InnerState closed, got %v", got)
	}
	if got := cb.EffectiveState(); got != StateClosed {
		t.Fatalf("EffectiveState closed with slack, got %v", got)
	}

	// Saturate bulkhead.
	if !cb.Allow() {
		t.Fatal("Allow slot 1")
	}
	if !cb.Allow() {
		t.Fatal("Allow slot 2")
	}

	// Inner breaker is still closed — but effective state flips to open
	// because the bulkhead is rejecting.
	if got := cb.InnerState(); got != StateClosed {
		t.Fatalf("InnerState still closed, got %v", got)
	}
	if got := cb.EffectiveState(); got != StateOpen {
		t.Fatalf("EffectiveState open when bulkhead saturated, got %v", got)
	}

	// Release a slot → effective state returns to closed.
	cb.Release()
	if got := cb.EffectiveState(); got != StateClosed {
		t.Fatalf("EffectiveState closed after release, got %v", got)
	}

	cb.Release()
}

func TestComposite_EffectiveState_BulkheadAndInnerOpen(t *testing.T) {
	cfg := Config{
		WindowSize:       2,
		FailureThreshold: 0.5,
		ResetTimeout:     30 * time.Second,
		HalfOpenMax:      1,
		MaxConcurrent:    1,
	}
	cb := NewComposite("http://test:8080", cfg, slog.Default(), nil)

	// Trip the inner breaker.
	cb.RecordFailure(10 * time.Millisecond)
	cb.RecordFailure(10 * time.Millisecond)

	if got := cb.InnerState(); got != StateOpen {
		t.Fatalf("InnerState open, got %v", got)
	}
	if got := cb.EffectiveState(); got != StateOpen {
		t.Fatalf("EffectiveState open (inner tripped), got %v", got)
	}
}

func TestComposite_StateAliasesInnerState(t *testing.T) {
	cfg := Config{
		WindowSize:       4,
		FailureThreshold: 0.5,
		ResetTimeout:     30 * time.Second,
		HalfOpenMax:      2,
		MaxConcurrent:    1,
	}
	cb := NewComposite("http://test:8080", cfg, slog.Default(), nil)

	// Saturate bulkhead so inner and effective would disagree.
	if !cb.Allow() {
		t.Fatal("Allow slot 1")
	}

	if cb.State() != cb.InnerState() {
		t.Fatal("State() must alias InnerState() for backward compat")
	}
	if cb.State() == cb.EffectiveState() {
		t.Fatal("State() must not track EffectiveState() when they legitimately differ")
	}

	cb.Release()
}

func TestComposite_FullStack(t *testing.T) {
	cfg := Config{
		WindowSize:       4,
		FailureThreshold: 0.5,
		ResetTimeout:     30 * time.Second,
		HalfOpenMax:      2,
		SlowThreshold:    50 * time.Millisecond,
		MaxConcurrent:    5,
		Adaptive:         true,
		LatencyCeiling:   100 * time.Millisecond,
		MinThreshold:     0.2,
	}
	cb := NewComposite("http://test:8080", cfg, slog.Default(), nil)

	// Full stack: bulkhead → timeout → adaptive → failure-rate.
	if !cb.Allow() {
		t.Fatal("expected Allow()")
	}
	cb.RecordSuccess(10 * time.Millisecond)
	cb.Release()

	if cb.State() != StateClosed {
		t.Fatalf("expected StateClosed, got %v", cb.State())
	}
}
