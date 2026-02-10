package circuitbreaker

import (
	"log/slog"
	"testing"
	"time"
)

func TestTimeoutBreaker_FastSuccess(t *testing.T) {
	inner := newTestBreaker(4, 0.5, 30*time.Second, 2)
	tb := NewTimeoutBreaker(inner, 100*time.Millisecond)

	tb.RecordSuccess(10 * time.Millisecond) // fast — real success
	tb.RecordSuccess(10 * time.Millisecond)
	tb.RecordSuccess(10 * time.Millisecond)
	tb.RecordSuccess(10 * time.Millisecond)

	if inner.State() != StateClosed {
		t.Fatalf("expected StateClosed, got %v", inner.State())
	}
}

func TestTimeoutBreaker_SlowSuccessBecomesFailure(t *testing.T) {
	inner := newTestBreaker(4, 0.5, 30*time.Second, 2)
	tb := NewTimeoutBreaker(inner, 100*time.Millisecond)

	// 2 fast, 2 slow → 2 converted failures → 2/4 = 0.5 >= threshold → trips.
	tb.RecordSuccess(10 * time.Millisecond)  // fast
	tb.RecordSuccess(10 * time.Millisecond)  // fast
	tb.RecordSuccess(200 * time.Millisecond) // slow → failure
	tb.RecordSuccess(200 * time.Millisecond) // slow → failure

	if inner.State() != StateOpen {
		t.Fatalf("expected StateOpen after slow responses, got %v", inner.State())
	}
}

func TestTimeoutBreaker_ExplicitFailure(t *testing.T) {
	inner := newTestBreaker(2, 0.5, 30*time.Second, 2)
	tb := NewTimeoutBreaker(inner, 100*time.Millisecond)

	tb.RecordFailure(10 * time.Millisecond)
	tb.RecordFailure(10 * time.Millisecond)

	if inner.State() != StateOpen {
		t.Fatalf("expected StateOpen after explicit failures, got %v", inner.State())
	}
}

func TestTimeoutBreaker_DelegatesAllowAndState(t *testing.T) {
	inner := NewFailureRateBreaker("test", 2, 1.0, 30*time.Second, 1, slog.Default())
	tb := NewTimeoutBreaker(inner, 100*time.Millisecond)

	if !tb.Allow() {
		t.Fatal("expected Allow() from closed inner")
	}
	if tb.State() != StateClosed {
		t.Fatal("expected StateClosed from inner")
	}

	tb.Reset()
	if tb.State() != StateClosed {
		t.Fatal("expected StateClosed after Reset")
	}
}
