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
	cb := NewComposite("http://test:8080", cfg, slog.Default())

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
	cb := NewComposite("http://test:8080", cfg, slog.Default())

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
	cb := NewComposite("http://test:8080", cfg, slog.Default())

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
	cb := NewComposite("http://test:8080", cfg, slog.Default())

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
	cb := NewComposite("http://test:8080", cfg, slog.Default())

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
	cb := NewComposite("http://test:8080", cfg, slog.Default())

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
