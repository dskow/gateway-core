package circuitbreaker

import (
	"sync"
	"testing"
	"time"
)

func TestBulkhead_AllowsUpToLimit(t *testing.T) {
	inner := newTestBreaker(10, 0.9, 30*time.Second, 2)
	bh := NewBulkheadBreaker(inner, 3, "test-backend")

	// Acquire 3 slots — all should succeed.
	for i := 0; i < 3; i++ {
		if !bh.Allow() {
			t.Fatalf("expected Allow() on slot %d", i)
		}
	}

	// 4th should be rejected.
	if bh.Allow() {
		t.Fatal("expected Allow() to return false at concurrency limit")
	}
}

func TestBulkhead_ReleaseFreesSlot(t *testing.T) {
	inner := newTestBreaker(10, 0.9, 30*time.Second, 2)
	bh := NewBulkheadBreaker(inner, 1, "test-backend")

	if !bh.Allow() {
		t.Fatal("expected first Allow()")
	}

	// At limit.
	if bh.Allow() {
		t.Fatal("expected rejection at limit")
	}

	// Release and re-acquire.
	bh.Release()
	if !bh.Allow() {
		t.Fatal("expected Allow() after Release()")
	}
}

func TestBulkhead_RejectsWhenInnerRejects(t *testing.T) {
	inner := newTestBreaker(2, 0.5, 30*time.Second, 2)
	// Trip the inner breaker.
	inner.RecordFailure(10 * time.Millisecond)
	inner.RecordFailure(10 * time.Millisecond)

	bh := NewBulkheadBreaker(inner, 10, "test-backend")

	// Bulkhead has slots, but inner breaker is open.
	if bh.Allow() {
		t.Fatal("expected rejection when inner breaker is open")
	}
}

func TestBulkhead_ConcurrentAccess(t *testing.T) {
	inner := newTestBreaker(100, 0.9, 30*time.Second, 2)
	bh := NewBulkheadBreaker(inner, 10, "test-backend")

	var wg sync.WaitGroup
	allowed := make(chan struct{}, 100)
	rejected := make(chan struct{}, 100)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if bh.Allow() {
				allowed <- struct{}{}
				time.Sleep(10 * time.Millisecond) // simulate work
				bh.RecordSuccess(10 * time.Millisecond)
				bh.Release()
			} else {
				rejected <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(allowed)
	close(rejected)

	a := len(allowed)
	r := len(rejected)
	if a+r != 50 {
		t.Fatalf("expected 50 total, got %d allowed + %d rejected", a, r)
	}
	if r == 0 {
		t.Fatal("expected some rejections with 50 goroutines and limit of 10")
	}
}

func TestBulkhead_DelegatesRecordAndState(t *testing.T) {
	inner := newTestBreaker(10, 0.5, 30*time.Second, 2)
	bh := NewBulkheadBreaker(inner, 5, "test-backend")

	if bh.State() != StateClosed {
		t.Fatal("expected StateClosed")
	}

	bh.Reset()
	if bh.State() != StateClosed {
		t.Fatal("expected StateClosed after Reset")
	}
}
