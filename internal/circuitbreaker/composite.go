package circuitbreaker

import (
	"log/slog"
	"time"

	"github.com/dskow/gateway-core/internal/metrics"
)

// Config holds all circuit breaker configuration. The failure-rate breaker is
// always active. Timeout, bulkhead, and adaptive breakers are enabled only
// when their respective settings are non-zero/true.
type Config struct {
	// Failure-rate breaker (always active)
	WindowSize       int
	FailureThreshold float64
	ResetTimeout     time.Duration
	HalfOpenMax      int

	// Timeout breaker (active when SlowThreshold > 0)
	SlowThreshold time.Duration

	// Bulkhead breaker (active when MaxConcurrent > 0)
	MaxConcurrent int

	// Adaptive breaker (active when Adaptive is true)
	Adaptive       bool
	LatencyCeiling time.Duration
	MinThreshold   float64
}

// CompositeBreaker composes multiple breaker layers into a single unit.
// The proxy interacts only with CompositeBreaker; internal layering is
// transparent.
type CompositeBreaker struct {
	failureRate *FailureRateBreaker
	bulkhead    *BulkheadBreaker // nil if bulkhead disabled
	effective   Breaker          // outermost layer — what Allow/Record call
}

// NewComposite builds a composed breaker stack for the given backend.
// Composition order (inside → out): FailureRate → Adaptive → Timeout → Bulkhead.
// m may be nil for tests that do not exercise the metrics path.
func NewComposite(backend string, cfg Config, logger *slog.Logger, m *metrics.Metrics) *CompositeBreaker {
	fr := NewFailureRateBreaker(backend, cfg.WindowSize, cfg.FailureThreshold, cfg.ResetTimeout, cfg.HalfOpenMax, logger, m)

	var current Breaker = fr

	// Wrap with adaptive if enabled (modifies the failure-rate breaker's threshold).
	if cfg.Adaptive {
		alpha := 0.3 // sensible default
		current = NewAdaptiveBreaker(fr, cfg.FailureThreshold, cfg.MinThreshold, cfg.LatencyCeiling, alpha)
	}

	// Wrap with timeout breaker if slow threshold is configured.
	if cfg.SlowThreshold > 0 {
		current = NewTimeoutBreaker(current, cfg.SlowThreshold)
	}

	cb := &CompositeBreaker{
		failureRate: fr,
		effective:   current,
	}

	// Wrap with bulkhead if max concurrent is configured.
	if cfg.MaxConcurrent > 0 {
		bh := NewBulkheadBreaker(current, cfg.MaxConcurrent, backend, m)
		cb.bulkhead = bh
		cb.effective = bh
	}

	return cb
}

func (c *CompositeBreaker) Allow() bool {
	return c.effective.Allow()
}

func (c *CompositeBreaker) RecordSuccess(latency time.Duration) {
	c.effective.RecordSuccess(latency)
}

func (c *CompositeBreaker) RecordFailure(latency time.Duration) {
	c.effective.RecordFailure(latency)
}

// InnerState returns the core failure-rate breaker's state, ignoring any
// outer decorators (bulkhead, timeout, adaptive).
func (c *CompositeBreaker) InnerState() State {
	return c.failureRate.State()
}

// EffectiveState returns the state the caller actually observes at the
// outermost decorator: StateOpen when an outer layer (today, the bulkhead)
// is rejecting regardless of the inner breaker, otherwise InnerState.
// Health/readiness probes should use EffectiveState so a saturated
// bulkhead does not appear "green" while the gateway is shedding load.
func (c *CompositeBreaker) EffectiveState() State {
	if c.bulkhead != nil && c.bulkhead.AtCapacity() {
		return StateOpen
	}
	return c.InnerState()
}

// State is an alias for InnerState preserved for backward compatibility.
// Prefer InnerState (explicit) or EffectiveState (outermost) at new call sites.
func (c *CompositeBreaker) State() State {
	return c.InnerState()
}

func (c *CompositeBreaker) Reset() {
	c.effective.Reset()
}

// Release frees a bulkhead concurrency slot. Must be called after every
// Allow() that returned true. Safe to call when bulkhead is disabled (no-op).
func (c *CompositeBreaker) Release() {
	if c.bulkhead != nil {
		c.bulkhead.Release()
	}
}

// UpdateConfig updates the failure-rate breaker's core parameters at runtime
// (e.g., on config hot-reload). Thread-safe.
func (c *CompositeBreaker) UpdateConfig(cfg Config) {
	c.failureRate.mu.Lock()
	defer c.failureRate.mu.Unlock()

	c.failureRate.failureThreshold = cfg.FailureThreshold
	c.failureRate.resetTimeout = cfg.ResetTimeout
	c.failureRate.halfOpenMax = cfg.HalfOpenMax

	// Resize the window if needed.
	if cfg.WindowSize != c.failureRate.windowSize {
		c.failureRate.window = make([]outcome, cfg.WindowSize)
		c.failureRate.windowSize = cfg.WindowSize
		c.failureRate.head = 0
		c.failureRate.count = 0
		c.failureRate.failures = 0
	}
}
