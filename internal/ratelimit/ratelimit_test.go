package ratelimit

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dskow/gateway-core/internal/config"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestLimiter_AllowsUpToBurst(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerSecond: 10,
		BurstSize:         5,
	}
	logger := slog.Default()
	limiter := New(cfg, nil, nil, logger, nil)
	defer limiter.Stop()

	handler := limiter.Middleware()(okHandler())

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("request %d: expected 200, got %d", i, rec.Code)
		}
	}
}

func TestLimiter_BlocksAfterBurst(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerSecond: 1,
		BurstSize:         2,
	}
	logger := slog.Default()
	limiter := New(cfg, nil, nil, logger, nil)
	defer limiter.Stop()

	handler := limiter.Middleware()(okHandler())

	// Use up burst
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "10.0.0.2:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	// Next request should be rate limited
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.2:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec.Code)
	}

	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Error("expected Retry-After header")
	}
}

func TestLimiter_PerClientIsolation(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerSecond: 1,
		BurstSize:         1,
	}
	logger := slog.Default()
	limiter := New(cfg, nil, nil, logger, nil)
	defer limiter.Stop()

	handler := limiter.Middleware()(okHandler())

	// Client 1 client uses up its burst
	req1 := httptest.NewRequest("GET", "/test", nil)
	req1.RemoteAddr = "10.0.0.1:12345"
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	// Client 1 is now rate limited
	req1b := httptest.NewRequest("GET", "/test", nil)
	req1b.RemoteAddr = "10.0.0.1:12345"
	rec1b := httptest.NewRecorder()
	handler.ServeHTTP(rec1b, req1b)
	if rec1b.Code != http.StatusTooManyRequests {
		t.Errorf("client 1 should be rate limited, got %d", rec1b.Code)
	}

	// Client 2 should still be allowed
	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.RemoteAddr = "10.0.0.2:12345"
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Errorf("client 2 should be allowed, got %d", rec2.Code)
	}
}

func TestLimiter_XForwardedFor_NoTrustedProxies(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerSecond: 1,
		BurstSize:         1,
	}
	logger := slog.Default()
	// No trusted proxies — XFF should be IGNORED, rate limit by RemoteAddr
	limiter := New(cfg, nil, nil, logger, nil)
	defer limiter.Stop()

	handler := limiter.Middleware()(okHandler())

	// Two requests from different XFF but same RemoteAddr
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.50:8080"
	req.Header.Set("X-Forwarded-For", "192.168.1.100")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// Same RemoteAddr, different XFF — should be rate limited by RemoteAddr
	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.RemoteAddr = "10.0.0.50:8080"
	req2.Header.Set("X-Forwarded-For", "192.168.1.200")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 (XFF ignored without trusted proxies), got %d", rec2.Code)
	}
}

func TestLimiter_XForwardedFor_TrustedProxy(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerSecond: 1,
		BurstSize:         1,
	}
	logger := slog.Default()
	// Trust the 10.0.0.0/8 range
	limiter := New(cfg, nil, []string{"10.0.0.0/8"}, logger, nil)
	defer limiter.Stop()

	handler := limiter.Middleware()(okHandler())

	// Request from trusted proxy with XFF
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.1:8080"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// Same XFF IP, same trusted proxy — should be rate limited by XFF IP
	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.RemoteAddr = "10.0.0.1:8080"
	req2.Header.Set("X-Forwarded-For", "203.0.113.50")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 for same XFF IP via trusted proxy, got %d", rec2.Code)
	}
}

func TestLimiter_XForwardedFor_UntrustedPeer(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerSecond: 1,
		BurstSize:         1,
	}
	logger := slog.Default()
	// Only trust 10.0.0.0/8
	limiter := New(cfg, nil, []string{"10.0.0.0/8"}, logger, nil)
	defer limiter.Stop()

	handler := limiter.Middleware()(okHandler())

	// Request from UNTRUSTED peer trying to spoof XFF
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "203.0.113.99:12345"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// Same untrusted peer — rate limited by RemoteAddr, not spoofed XFF
	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.RemoteAddr = "203.0.113.99:12345"
	req2.Header.Set("X-Forwarded-For", "5.6.7.8")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 (spoofed XFF from untrusted peer ignored), got %d", rec2.Code)
	}
}

func TestLimiter_PerRouteOverride(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerSecond: 100,
		BurstSize:         100,
	}
	routes := []config.RouteConfig{
		{
			PathPrefix: "/limited",
			RateOverride: &config.RateLimitConfig{
				RequestsPerSecond: 1,
				BurstSize:         1,
			},
		},
	}
	logger := slog.Default()
	limiter := New(cfg, routes, nil, logger, nil)
	defer limiter.Stop()

	handler := limiter.Middleware()(okHandler())

	// First request to /limited — should pass
	req1 := httptest.NewRequest("GET", "/limited/test", nil)
	req1.RemoteAddr = "10.0.0.5:12345"
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec1.Code)
	}

	// Second request to /limited — should be rate limited
	req2 := httptest.NewRequest("GET", "/limited/test", nil)
	req2.RemoteAddr = "10.0.0.5:12345"
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec2.Code)
	}
}

func TestLimiter_ResponseBody(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerSecond: 1,
		BurstSize:         1,
	}
	logger := slog.Default()
	limiter := New(cfg, nil, nil, logger, nil)
	defer limiter.Stop()

	handler := limiter.Middleware()(okHandler())

	// Exhaust burst
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.10:12345"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Rate limited request
	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.RemoteAddr = "10.0.0.10:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req2)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
}

// Note: pathMatchesPrefix tests moved to internal/routing/match_test.go
// since the function was extracted into the shared routing package.

// DP-005: the janitor must evict idle client entries so per-IP memory
// growth is bounded over time.
func TestLimiter_JanitorEvictsIdleClients(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerSecond: 10,
		BurstSize:         5,
		IdleTTL:           50 * time.Millisecond,
		CleanupInterval:   10 * time.Millisecond, // unused — we drive evictOnce directly
	}
	limiter := New(cfg, nil, nil, slog.Default(), nil)
	defer limiter.Close()

	handler := limiter.Middleware()(okHandler())

	// Seed 100 distinct client buckets.
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = fmt.Sprintf("10.0.%d.%d:12345", i/256, i%256)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	limiter.mu.RLock()
	before := len(limiter.clients)
	limiter.mu.RUnlock()
	if before != 100 {
		t.Fatalf("expected 100 tracked clients, got %d", before)
	}

	// Simulate all entries becoming idle past the TTL.
	limiter.evictOnce(time.Now().Add(1 * time.Second))

	limiter.mu.RLock()
	after := len(limiter.clients)
	limiter.mu.RUnlock()
	if after != 0 {
		t.Fatalf("janitor failed to evict idle clients: %d remain", after)
	}
}

// DP-005: entries refreshed since the last tick must survive the eviction pass.
func TestLimiter_JanitorSparesActiveClients(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerSecond: 10,
		BurstSize:         5,
		IdleTTL:           time.Second,
		CleanupInterval:   time.Minute,
	}
	limiter := New(cfg, nil, nil, slog.Default(), nil)
	defer limiter.Close()

	handler := limiter.Middleware()(okHandler())

	// One active client — will be refreshed right before the eviction pass.
	active := httptest.NewRequest("GET", "/test", nil)
	active.RemoteAddr = "10.0.0.1:12345"
	handler.ServeHTTP(httptest.NewRecorder(), active)

	// One idle client — seeded then left alone.
	idle := httptest.NewRequest("GET", "/test", nil)
	idle.RemoteAddr = "10.0.0.2:12345"
	handler.ServeHTTP(httptest.NewRecorder(), idle)

	// Age every entry so the scan phase marks both as expired.
	limiter.mu.Lock()
	for _, c := range limiter.clients {
		c.lastSeen = time.Now().Add(-2 * time.Second)
	}
	limiter.mu.Unlock()

	// Refresh the active client right before the eviction pass — this
	// is the race the per-batch re-check under Lock must survive.
	handler.ServeHTTP(httptest.NewRecorder(), active)

	limiter.evictOnce(time.Now())

	limiter.mu.RLock()
	remaining := len(limiter.clients)
	limiter.mu.RUnlock()
	if remaining != 1 {
		t.Fatalf("expected active client to survive eviction, got %d clients", remaining)
	}
}

// DP-005: Close must be idempotent and block until the janitor exits.
func TestLimiter_CloseIsIdempotent(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerSecond: 10,
		BurstSize:         5,
		IdleTTL:           10 * time.Minute,
		CleanupInterval:   10 * time.Millisecond,
	}
	limiter := New(cfg, nil, nil, slog.Default(), nil)
	limiter.Close()
	limiter.Close() // second call must not panic or block
}
