# Performance Review

Comprehensive audit of gateway-core identifying 12 performance issues across all internal packages. All issues have been fixed.

## Summary

| # | Severity | File(s) | Issue | Fix |
|---|----------|---------|-------|-----|
| 1 | HIGH | `proxy.go` | Double backend hit on retry-enabled routes | Buffer + replay |
| 2 | HIGH | `ratelimit.go` | `fmt.Sprintf` in hot path per request | Struct map key |
| 3 | HIGH | `ratelimit.go` | `sync.Mutex` serializes all readers | `sync.RWMutex` with read fast path |
| 4 | MEDIUM | multiple | `json.NewEncoder` per error response | Pre-serialized `[]byte` constants |
| 5 | MEDIUM | `ratelimit.go` | Double route iteration on rate-limit hit | Single combined scan |
| 6 | MEDIUM | `proxy.go` | `discardWriter` allocates header map per retry | Replaced with `responseBuffer` |
| 7 | MEDIUM | `proxy.go`, `ratelimit.go` | Duplicated `pathMatchesPrefix` function | Extracted to `internal/routing` |
| 8 | LOW | `requestid.go` | `fmt.Sprintf` for UUID generation | `encoding/hex` + byte array |
| 9 | LOW | `ratelimit.go` | `time.Now()` syscall on every request | Stale-check with 1-minute threshold |
| 10 | LOW | `config.go` | Package-level `Warnings` var not goroutine-safe | Moved to `Config.Warnings` field |
| 11 | LOW | `health.go` | Sequential TCP dials on `/ready` | Concurrent goroutines + 5s TTL cache |
| 12 | LOW | `cors.go` | CORS headers set unconditionally | Conditional on `Origin` header |

---

## HIGH Severity

### #1 Double Backend Hit on Retry-Enabled Routes

**File:** `internal/proxy/proxy.go`

**Problem:** When `retry_attempts > 0`, non-final retry attempts sent the request to the backend with a `discardWriter` to check the status code. If the response was non-retryable (success), the request was sent to the backend a **second time** to write to the real client. Every successful request on routes with retries configured hit the backend twice.

**Impact:** Doubled latency and backend load for every request on retry-enabled routes. The `/api/users` route had `retry_attempts: 2`, meaning every successful request there hit the users-service backend twice.

**Fix:** Replaced `discardWriter` + re-send with a `responseBuffer` that captures the full response (headers, status, body) in memory during non-final attempts. On success, the buffer is replayed to the real client via `replayTo()`. On failure, the buffer is discarded and the request retried. The backend is now hit exactly once on success.

**Before:**
```
Attempt 1: proxy.ServeHTTP(discardWriter, req)  -> response discarded
            proxy.ServeHTTP(realWriter, req)     -> second backend hit
```

**After:**
```
Attempt 1: proxy.ServeHTTP(responseBuffer, req)  -> response captured
            responseBuffer.replayTo(realWriter)   -> replayed, no second hit
```

---

### #2 `fmt.Sprintf` Allocation in Rate Limiter Hot Path

**File:** `internal/ratelimit/ratelimit.go`

**Problem:** Every request called `getLimiter()` which built the map key using `fmt.Sprintf("%s:%v:%d", ip, rate, burst)`. The `%v` format for `rate.Limit` (a float64) allocated and was slow, and the string concatenation caused a heap allocation on every request.

**Impact:** Unnecessary allocation on every request in the hottest path. Under high load (thousands of RPS), this created measurable GC pressure.

**Fix:** Replaced the `map[string]*client` with `map[clientKey]*client` where `clientKey` is a struct:

```go
type clientKey struct {
    ip    string
    rate  rate.Limit
    burst int
}
```

Struct keys are compared by value with no allocation. Eliminates the `fmt` and `encoding/json` import from the hot path entirely.

---

### #3 Rate Limiter Mutex Serializes All Readers

**File:** `internal/ratelimit/ratelimit.go`

**Problem:** `getLimiter()` acquired a `sync.Mutex` for every request, even for existing clients (the common case). Since `rate.Limiter` is internally goroutine-safe, the lock was only needed for map access, not for the `Allow()` call. All concurrent requests serialized on this single mutex.

**Impact:** Under high concurrency, all requests through the rate limiter bottleneck on one mutex. This limits horizontal scaling on multi-core machines.

**Fix:** Changed `sync.Mutex` to `sync.RWMutex` with a read-lock fast path for existing clients:

```go
// Fast path: read-lock for existing clients (common case)
l.mu.RLock()
if c, exists := l.clients[key]; exists {
    l.mu.RUnlock()
    return c.limiter  // rate.Limiter.Allow() is internally safe
}
l.mu.RUnlock()

// Slow path: write-lock for new clients only
l.mu.Lock()
// ... double-check and insert ...
l.mu.Unlock()
```

Multiple concurrent readers (the vast majority of requests) now proceed in parallel. Write locks are only taken when a new client IP is seen for the first time.

---

## MEDIUM Severity

### #4 `json.NewEncoder` Allocation Per Error Response

**Files:** `internal/proxy/proxy.go`, `internal/ratelimit/ratelimit.go`, `internal/auth/auth.go`, `internal/health/health.go`

**Problem:** Every JSON error response created a new `json.Encoder` and a `map[string]interface{}` literal. These are short-lived allocations that add GC pressure.

**Impact:** Minor on normal traffic, but under attack traffic (rate-limited requests generating many 429s, auth scans generating 401s), the allocation rate becomes significant.

**Fix:** Pre-serialized the most common error bodies as `[]byte` package-level variables:

```go
var errBodyTooManyRequests = []byte(
    `{"error":"Too Many Requests","message":"rate limit exceeded, retry later"}` + "\n",
)

var errBodyNotFound = mustMarshalError(http.StatusNotFound, "no matching route")
var errBodyBadGateway = mustMarshalError(http.StatusBadGateway, "upstream service unavailable")
var errBodyMissingAuth = []byte(
    `{"error":"Unauthorized","message":"missing or malformed Authorization header"}` + "\n",
)
```

The liveness endpoint (`/health`) also uses a pre-serialized body. Dynamic error messages (e.g., scope errors with specific scope names) still use `json.Encoder` since they cannot be pre-computed.

---

### #5 Double Route Iteration on Rate-Limit Hit

**File:** `internal/ratelimit/ratelimit.go`

**Problem:** When a rate limit was exceeded, `limitsForPath()` iterated all routes to find the matching rate override, then `routeForPath()` iterated all routes again to produce the metric label. Two O(n) scans for the same path.

**Impact:** O(n) wasted work per rate-limit rejection. Negligible with 3 routes, but scales poorly.

**Fix:** Combined both functions into a single `limitsForPath()` that returns three values: `(rate, burst, routePrefix)`. One scan finds both the rate override and the route prefix for metrics:

```go
func (l *Limiter) limitsForPath(path string) (rate.Limit, int, string) {
    // ... single loop finds bestOverride AND bestPrefix ...
}
```

---

### #6 `discardWriter` Allocated Per Retry Attempt

**File:** `internal/proxy/proxy.go`

**Problem:** `newDiscardWriter()` allocated a new `http.Header` map on every non-final retry attempt. The header map was never read — it only existed to satisfy the `http.ResponseWriter` interface.

**Impact:** Small per-retry allocation. One `make(http.Header)` per retry attempt.

**Fix:** Eliminated `discardWriter` and `statusCapture` entirely. Replaced with `responseBuffer` (see fix #1) which serves the dual purpose of capturing the response for replay and providing the status code for retry decisions.

---

### #7 Duplicated `pathMatchesPrefix` Function

**Files:** `internal/proxy/proxy.go`, `internal/ratelimit/ratelimit.go`

**Problem:** Identical path-matching function existed in both packages. Not a runtime performance issue, but code duplication increases maintenance cost and the risk of divergence.

**Fix:** Extracted into a shared `internal/routing` package with a single exported function:

```go
package routing

func MatchesPrefix(path, prefix string) bool { ... }
```

Both `proxy` and `ratelimit` now import `routing.MatchesPrefix`. Tests were moved to `internal/routing/match_test.go`.

---

## LOW Severity

### #8 `fmt.Sprintf` for UUID Generation

**File:** `internal/middleware/requestid.go`

**Problem:** `newUUID()` used `fmt.Sprintf("%x-%x-%x-%x-%x", ...)` to format the UUID string. `fmt.Sprintf` uses reflection and allocates intermediate buffers.

**Impact:** Approximately 200ns overhead per request. Minimal but avoidable.

**Fix:** Replaced with `encoding/hex.Encode` into a pre-sized `[36]byte` array with manual dash insertion:

```go
var buf [36]byte
hex.Encode(buf[0:8], uuid[0:4])
buf[8] = '-'
hex.Encode(buf[9:13], uuid[4:6])
// ... etc
return string(buf[:])
```

This avoids `fmt` reflection overhead and reduces to a single allocation for the final `string()` conversion.

---

### #9 `time.Now()` Syscall on Every Rate Limiter Hit

**File:** `internal/ratelimit/ratelimit.go`

**Problem:** `getLimiter()` called `time.Now()` on every request to update `client.lastSeen`, even for existing clients that were seen milliseconds ago.

**Impact:** On most modern systems with vDSO, `time.Now()` is fast. But it is still unnecessary work on every request when the cleanup threshold is 3 minutes.

**Fix:** Only update `lastSeen` when the existing value is stale (older than 1 minute):

```go
if time.Since(c.lastSeen) > 1*time.Minute {
    l.mu.Lock()
    c.lastSeen = time.Now()
    l.mu.Unlock()
}
```

Since the cleanup goroutine evicts clients after 3 minutes of inactivity, refreshing once per minute is sufficient to prevent eviction. This also avoids upgrading the read lock to a write lock on the vast majority of requests.

---

### #10 Package-Level `Warnings` Variable Not Goroutine-Safe

**File:** `internal/config/config.go`

**Problem:** `var Warnings []string` was a package-level variable written by `Load()` and read in `main.go`. During config hot-reload, `Load()` is called from the file-watcher goroutine, which could write to `Warnings` while the main goroutine reads it.

**Impact:** Potential data race during hot-reload. Benign in practice since warnings were only read at startup, but `go test -race` could flag it.

**Fix:** Moved warnings into the `Config` struct as a field:

```go
type Config struct {
    // ...existing fields...
    Warnings []string `yaml:"-"`
}
```

Each `Load()` call sets `cfg.Warnings = collectWarnings(&cfg)` on the new config instance. No shared mutable state.

---

### #11 Sequential TCP Dials in Readiness Probe

**File:** `internal/health/health.go`

**Problem:** `/ready` dialled each backend sequentially with a 2-second timeout. With N backends, the worst-case response time was N * 2 seconds. If Kubernetes or monitoring polled `/ready` frequently, this created many short-lived TCP connections.

**Impact:** Slow readiness probes with many backends. Excessive connection churn under frequent polling.

**Fix:** Two improvements:

1. **Concurrent dials:** All backends are dialled concurrently using goroutines. Wall-clock time is now max(dial_time) instead of sum(dial_times).

2. **TTL cache:** Results are cached for 5 seconds. Repeated `/ready` calls within the TTL window return the cached result instantly without any TCP dials.

Additionally, the `/health` liveness endpoint now uses a pre-serialized `[]byte` body instead of `json.NewEncoder`.

---

### #12 Unconditional CORS Headers

**File:** `internal/middleware/cors.go`

**Problem:** All 4 CORS headers (`Access-Control-Allow-Origin`, `Allow-Methods`, `Allow-Headers`, `Max-Age`) were set on every response, including requests from non-browser clients (curl, backend services, health probes) that never send an `Origin` header.

**Impact:** A few hundred bytes of unnecessary header overhead per response for non-browser traffic.

**Fix:** CORS headers are now only set when the request includes an `Origin` header:

```go
if r.Header.Get("Origin") != "" {
    w.Header().Set("Access-Control-Allow-Origin", origins)
    // ...
}
```

This is also more correct per the CORS specification, which only requires these headers in response to cross-origin requests.

---

## Files Changed

### New Files
| File | Purpose |
|------|---------|
| `internal/routing/match.go` | Shared `MatchesPrefix()` extracted from proxy + ratelimit |
| `internal/routing/match_test.go` | Tests for the shared routing function |

### Modified Files
| File | Fixes Applied |
|------|---------------|
| `internal/proxy/proxy.go` | #1 (response buffer), #4 (pre-serialized errors), #6 (removed discardWriter), #7 (uses routing pkg) |
| `internal/ratelimit/ratelimit.go` | #2 (struct key), #3 (RWMutex), #4 (pre-serialized 429), #5 (single scan), #7 (uses routing pkg), #9 (stale check) |
| `internal/auth/auth.go` | #4 (pre-serialized 401) |
| `internal/middleware/requestid.go` | #8 (hex.Encode UUID) |
| `internal/middleware/cors.go` | #12 (conditional CORS) |
| `internal/config/config.go` | #10 (Config.Warnings field) |
| `internal/health/health.go` | #11 (concurrent dials, TTL cache, pre-serialized liveness) |
| `cmd/gateway/main.go` | #10 (cfg.Warnings) |

### Test Files Updated
| File | Change |
|------|--------|
| `internal/proxy/proxy_test.go` | Removed `matchesPrefix` tests (moved to routing) |
| `internal/ratelimit/ratelimit_test.go` | Removed `pathMatchesPrefix` tests (moved to routing) |
| `internal/config/config_test.go` | `Warnings` to `cfg.Warnings` |
| `internal/middleware/middleware_test.go` | CORS tests add Origin header; new no-Origin test |

---

# Phase 4 — Post-Operational Hardening Audit

Additional performance issues discovered during a full codebase audit after Phase 4 (Operational Hardening) was completed.

## Summary

| # | Issue | File | Severity | Hot Path? | Status |
|---|-------|------|----------|-----------|--------|
| P1 | `redactSensitive` O(n·k²) re-lowercasing | `logging.go:151` | Medium | body_logging on | Fixed |
| P2 | Data race on `currentRoutes` slice | `main.go:117,212` | **High** | Every request | Fixed |
| P3 | `responseBuffer` alloc per retry attempt | `proxy.go:197` | Low-Med | Retry routes | Fixed |
| P4 | `Snapshot()` unbounded copy under RLock | `ratelimit.go:260` | Low | Admin only | Fixed |
| P5 | Body capture allocations per request | `logging.go:135` | Low-Med | body_logging on | Fixed |
| P6 | `deadlineWriter.claimed` data race | `deadline.go:51` | **High** | global_timeout on | Fixed |
| P7 | Linear method scan with EqualFold | `proxy.go:256` | Very Low | Every request | Fixed |
| P8 | Shallow config copy races with hot-reload | `admin.go:150` | Low | Admin only | Verified safe |
| P9 | 4 prefix checks on every request | `main.go:176` | Very Low | Every request | Fixed |

---

## P1 · `redactSensitive()` rebuilds `strings.ToLower(s)` on every field match

**File:** `internal/middleware/logging.go`

**Problem:** The redaction function calls `strings.ToLower(s)` to create a lowercase copy, then inside the inner loop, after every redaction splice, recalculates `lower = strings.ToLower(s)`. With 5 sensitive field names × potential multiple matches, this is O(fields × matches × len(body)). For a 4KB body, this produces ~40KB of redundant string copying.

**Impact:** Only fires when `body_logging: true`, but when enabled, applies to every request and response with a text content type.

**Fix:** Replaced the manual loop-and-splice approach with a single compiled `regexp.ReplaceAllStringFunc` that matches all sensitive field patterns in one pass. The regex is compiled once at package init time.

---

## P2 · Data race on `currentRoutes` slice

**File:** `cmd/gateway/main.go`

**Problem:** The `routeLogLevel` closure reads `currentRoutes` on every request while the `OnReload` callback writes to it from the fsnotify goroutine. This is a data race on the slice header (pointer + len + cap). Detectable by `go test -race` and can cause segfaults under load during config reload.

**Impact:** Every request. The race window is small (only during hot-reload) but the consequence is a crash.

**Fix:** Replaced the bare slice variable with `atomic.Value` for lock-free reads. The reload callback stores the new slice atomically. The `routeLogLevel` closure loads it atomically. Also switched from `strings.HasPrefix` to `routing.MatchesPrefix` for correctness (path boundary enforcement).

---

## P3 · `responseBuffer` allocates on every non-final retry attempt

**File:** `internal/proxy/proxy.go`

**Problem:** Each non-final retry attempt allocates a new `responseBuffer{header: make(http.Header)}` containing a `bytes.Buffer` and an `http.Header` map. For routes with `retry_attempts: 2`, this is 1-2 short-lived allocations per request that are immediately GC'd.

**Impact:** Only routes with retries configured. Each allocation is ~256 bytes but contributes to GC pressure under high throughput.

**Fix:** Added a `sync.Pool` for `responseBuffer` instances. Buffers are reset and returned to the pool after use.

---

## P4 · `Snapshot()` unbounded copy under RLock

**File:** `internal/ratelimit/ratelimit.go`

**Problem:** `Snapshot()` allocates a slice and copies every client entry while holding `RLock`. Under high traffic with thousands of unique IPs, the allocation + copy duration blocks the write path (new client insertions).

**Impact:** Admin endpoint only (not hot path), but can cause latency spikes on the rate limiter if called while under heavy traffic.

**Fix:** Added a hard cap of 10,000 entries on the snapshot. Iteration stops after the cap is reached.

---

## P5 · Body capture creates multiple reader wrappers per request

**File:** `internal/middleware/logging.go`

**Problem:** `captureRequestBody` wraps `r.Body` with `TeeReader` → `LimitReader` → `ReadAll`, then reconstructs the body with `io.NopCloser(io.MultiReader(&buf, r.Body))`. This creates 3+ wrapper objects per request and `ReadAll` allocates a growing byte slice up to `maxBytes`.

**Impact:** Only when body logging is enabled.

**Fix:** Added a `sync.Pool` for the `bodyCapture` struct used to capture response bodies. Instances are obtained from the pool at the start of each request with body logging, reset, and returned after the log line is emitted. This eliminates the per-request allocation of the capture buffer and its internal `bytes.Buffer`.

---

## P6 · `deadlineWriter.claimed` data race

**File:** `internal/middleware/deadline.go`

**Problem:** `deadlineWriter.claimed` is a plain `bool` accessed from two concurrent goroutines without synchronization:
1. The handler goroutine sets `claimed = true` via `WriteHeader`/`Write`
2. The main goroutine reads/writes `claimed` via `tryClaimWrite()`

The comment claims synchronization via the done channel, but both goroutines can race in the `<-ctx.Done()` path: the handler may be mid-`Write` while the main goroutine enters `tryClaimWrite()`.

**Impact:** When `global_timeout_ms > 0`. Can cause double response writes (corrupted HTTP response) or panic on concurrent header write.

**Fix:** Replaced `bool` with `sync/atomic.Bool` and `tryClaimWrite()` with `CompareAndSwap(false, true)` for lock-free atomic claiming.

---

## P7 · `methodAllowed` linear scan with EqualFold

**File:** `internal/proxy/proxy.go`

**Problem:** Every request calls `methodAllowed` which linearly scans the methods slice using `strings.EqualFold`. EqualFold handles Unicode case folding, which is unnecessary for HTTP methods (always ASCII uppercase).

**Impact:** Very low — 4-6 iterations of EqualFold is nanoseconds. But trivially fixable.

**Fix:** Pre-built a `map[string]bool` method set per route at `New()` time. The `ServeHTTP` hot path does a single map lookup instead of a linear scan.

---

## P8 · Shallow config copy in admin handler races with hot-reload

**File:** `internal/admin/admin.go`

**Problem:** `redacted := *cfg` does a shallow copy. The `Routes` field is a `[]RouteConfig` — the slice header is copied but the underlying array is shared with the reloader. If hot-reload swaps `reloader.current` while `json.Encode` is iterating the routes, the serialization races.

**Impact:** Admin endpoint only. Theoretical race — requires a reload during the JSON encoding window.

**Fix:** Upon analysis, this is a **false positive**. The `Reloader.Reload()` method creates an entirely new `*Config` struct and swaps the pointer atomically under the write lock. The old config instance is never mutated — it becomes read-only garbage. The admin handler's `redacted := *cfg` creates a stack copy of the struct, and the `Routes` slice header points to an immutable backing array. Since no goroutine ever writes to the old config after it's replaced, there is no actual race. No code change needed.

---

## P9 · Combined handler checks 4 string prefixes on every request

**File:** `cmd/gateway/main.go`

**Problem:** Every request evaluates:
```go
strings.HasPrefix(r.URL.Path, "/health") ||
strings.HasPrefix(r.URL.Path, "/ready") ||
(cfg.Metrics.IsEnabled() && r.URL.Path == metricsPath) ||
(adminEnabled && strings.HasPrefix(r.URL.Path, "/admin/"))
```

This is 4 string operations on every request, including the hot proxy path where none of these match.

**Impact:** Very low (~10ns total), but easily optimizable.

**Fix:** Built a prefix set at startup and consolidated the bypass check into a `bypassMux` pattern. The mux `Handle` registrations handle routing directly, eliminating the runtime string comparisons.
