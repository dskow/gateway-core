package circuitbreaker

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/dskow/go-api-gateway/internal/metrics"
)

func init() {
	// Register metrics once for all tests in this package.
	metrics.Init()
}

func newTestBreaker(windowSize int, threshold float64, resetTimeout time.Duration, halfOpenMax int) *FailureRateBreaker {
	return NewFailureRateBreaker("http://test:8080", windowSize, threshold, resetTimeout, halfOpenMax, slog.Default())
}

func TestFailureRate_StartsClosedAndAllows(t *testing.T) {
	b := newTestBreaker(5, 0.5, 30*time.Second, 2)

	if b.State() != StateClosed {
		t.Fatalf("expected StateClosed, got %v", b.State())
	}
	if !b.Allow() {
		t.Fatal("expected Allow() to return true for closed breaker")
	}
}

func TestFailureRate_ClosedToOpen(t *testing.T) {
	// Window of 4, threshold 0.5 → need 2 failures out of 4.
	b := newTestBreaker(4, 0.5, 30*time.Second, 2)

	b.RecordSuccess(10 * time.Millisecond)
	b.RecordFailure(10 * time.Millisecond)
	b.RecordSuccess(10 * time.Millisecond)
	// 1/3 failures — not enough, window not full yet after 3 calls; count < windowSize.
	if b.State() != StateClosed {
		t.Fatalf("expected StateClosed after 3 calls, got %v", b.State())
	}

	b.RecordFailure(10 * time.Millisecond)
	// Window full: [S, F, S, F] → 2/4 = 0.5 >= 0.5 threshold → Open.
	if b.State() != StateOpen {
		t.Fatalf("expected StateOpen after reaching threshold, got %v", b.State())
	}

	if b.Allow() {
		t.Fatal("expected Allow() to return false for open breaker")
	}
}

func TestFailureRate_OpenToHalfOpen(t *testing.T) {
	b := newTestBreaker(2, 0.5, 50*time.Millisecond, 1)

	b.RecordFailure(10 * time.Millisecond)
	b.RecordFailure(10 * time.Millisecond)
	if b.State() != StateOpen {
		t.Fatalf("expected StateOpen, got %v", b.State())
	}

	// Wait for reset timeout to elapse.
	time.Sleep(60 * time.Millisecond)

	// Allow() should transition to HalfOpen.
	if !b.Allow() {
		t.Fatal("expected Allow() to return true after reset timeout")
	}
	if b.State() != StateHalfOpen {
		t.Fatalf("expected StateHalfOpen, got %v", b.State())
	}
}

func TestFailureRate_HalfOpenToClosed(t *testing.T) {
	b := newTestBreaker(2, 0.5, 10*time.Millisecond, 2)

	// Trip to open.
	b.RecordFailure(10 * time.Millisecond)
	b.RecordFailure(10 * time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	b.Allow() // Transition to half-open.

	// Record enough successes to close.
	b.RecordSuccess(10 * time.Millisecond)
	if b.State() != StateHalfOpen {
		t.Fatalf("expected still StateHalfOpen after 1 success, got %v", b.State())
	}
	b.RecordSuccess(10 * time.Millisecond)
	if b.State() != StateClosed {
		t.Fatalf("expected StateClosed after 2 successes, got %v", b.State())
	}
}

func TestFailureRate_HalfOpenToOpen(t *testing.T) {
	b := newTestBreaker(2, 0.5, 10*time.Millisecond, 2)

	b.RecordFailure(10 * time.Millisecond)
	b.RecordFailure(10 * time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	b.Allow()

	// Any failure in half-open should trip back to open.
	b.RecordFailure(10 * time.Millisecond)
	if b.State() != StateOpen {
		t.Fatalf("expected StateOpen after half-open failure, got %v", b.State())
	}
}

func TestFailureRate_Reset(t *testing.T) {
	b := newTestBreaker(2, 0.5, 30*time.Second, 2)

	b.RecordFailure(10 * time.Millisecond)
	b.RecordFailure(10 * time.Millisecond)
	if b.State() != StateOpen {
		t.Fatalf("expected StateOpen, got %v", b.State())
	}

	b.Reset()
	if b.State() != StateClosed {
		t.Fatalf("expected StateClosed after Reset, got %v", b.State())
	}
	if !b.Allow() {
		t.Fatal("expected Allow() after Reset")
	}
}

func TestFailureRate_SlidingWindowEviction(t *testing.T) {
	// Window of 3, threshold 0.5.
	b := newTestBreaker(3, 0.5, 30*time.Second, 2)

	// Fill window: [S, F, F] → 2/3 = 0.67 >= 0.5. The last call is a failure,
	// so the trip check runs and opens the breaker.
	b.RecordSuccess(10 * time.Millisecond)
	b.RecordFailure(10 * time.Millisecond)
	b.RecordFailure(10 * time.Millisecond)
	if b.State() != StateOpen {
		t.Fatalf("expected StateOpen, got %v", b.State())
	}

	// Verify eviction: after reset, record 3 successes to fill window.
	b.Reset()
	b.RecordSuccess(10 * time.Millisecond)
	b.RecordSuccess(10 * time.Millisecond)
	b.RecordSuccess(10 * time.Millisecond)
	// Now the window is [S, S, S]. Adding a failure evicts the oldest S.
	// Window becomes [S, S, F] → 1/3 = 0.33 < 0.5 → stays closed.
	b.RecordFailure(10 * time.Millisecond)
	if b.State() != StateClosed {
		t.Fatalf("expected StateClosed after eviction, got %v", b.State())
	}
}

func TestFailureRate_ConcurrentAccess(t *testing.T) {
	b := newTestBreaker(100, 0.9, 30*time.Second, 2)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Allow()
			b.RecordSuccess(time.Millisecond)
			b.RecordFailure(time.Millisecond)
			_ = b.State()
		}()
	}
	wg.Wait()
	// No panic or race condition = pass.
}

func TestFailureRate_SetFailureThreshold(t *testing.T) {
	b := newTestBreaker(2, 0.9, 30*time.Second, 2)

	// With high threshold, 1/2 failures shouldn't trip.
	b.RecordFailure(10 * time.Millisecond)
	b.RecordSuccess(10 * time.Millisecond)
	if b.State() != StateClosed {
		t.Fatalf("expected StateClosed with high threshold, got %v", b.State())
	}

	b.Reset()

	// Lower the threshold so 1/2 failures trip.
	b.SetFailureThreshold(0.5)
	b.RecordFailure(10 * time.Millisecond)
	b.RecordSuccess(10 * time.Millisecond)
	if b.State() != StateOpen {
		t.Fatalf("expected StateOpen with lowered threshold, got %v", b.State())
	}
}

func TestState_String(t *testing.T) {
	cases := []struct {
		state State
		want  string
	}{
		{StateClosed, "closed"},
		{StateOpen, "open"},
		{StateHalfOpen, "half-open"},
		{State(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}
