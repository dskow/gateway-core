# Mission-Critical Roadmap

A phased plan to raise gateway-core from a functional prototype to production-grade, mission-critical infrastructure.

## Phase 1 — Security Hardening (COMPLETE)

Addressed the most exploitable security gaps without adding new dependencies.

### 1. Trusted Proxy — X-Forwarded-For Spoofing Fix

**Problem:** `clientIP()` blindly trusted `X-Forwarded-For` from any client, allowing rate limit bypass by spoofing the header.

**Solution:** Added `trusted_proxies` CIDR list to config. XFF is only trusted when `RemoteAddr` is in a trusted CIDR. Walks XFF right-to-left and returns the first non-trusted IP. Default: empty list (ignore XFF entirely — safe default).

**Files:** `internal/config/config.go`, `internal/ratelimit/ratelimit.go`

### 2. Request Body Size Limit

**Problem:** No cap on request body size — clients could POST unlimited data causing OOM.

**Solution:** New `BodyLimit` middleware wraps `r.Body` with `http.MaxBytesReader`. Default limit: 1 MB. Returns 413 when exceeded.

**Files:** `internal/config/config.go`, `internal/middleware/bodylimit.go`

### 3. Backend URL Scheme Validation

**Problem:** Config accepted `file:///etc/passwd` as a backend URL — SSRF vector.

**Solution:** Validation rejects any backend URL that isn't `http://` or `https://` with a non-empty host.

**Files:** `internal/config/config.go`

### 4. Path Matching Boundary Enforcement

**Problem:** Route prefix `/api` matched `/api.evil.com/steal` because `strings.HasPrefix` has no boundary check.

**Solution:** After prefix match, require the path to either equal the prefix exactly, the prefix to end with `/`, or the next character in path to be `/`. Applied in both proxy router and rate limiter.

**Files:** `internal/proxy/proxy.go`, `internal/ratelimit/ratelimit.go`

### 5. Security Response Headers

**Problem:** No HSTS, X-Content-Type-Options, or X-Frame-Options headers.

**Solution:** New `SecurityHeaders` middleware sets `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `X-XSS-Protection: 0`, and conditional `Strict-Transport-Security` (only on TLS or `X-Forwarded-Proto: https`).

**Files:** `internal/middleware/security.go`

### 6. Config Validation Improvements

**Solution:** `issuer` and `audience` are now required when auth is enabled. Warnings emitted for unresolved `${ENV_VAR}` in `jwt_secret`. `max_body_bytes` validated as non-negative.

**Files:** `internal/config/config.go`

### Middleware Chain (post-Phase 1)

```
Recovery → SecurityHeaders → Logging → CORS → BodyLimit → RateLimit → Auth → Proxy
```

### Config Additions (backward-compatible)

```yaml
server:
  trusted_proxies: ["10.0.0.0/8"]  # optional, default: [] (ignore XFF)
  max_body_bytes: 1048576           # optional, default: 1MB
```

---

## Phase 2 — Observability & Metrics (COMPLETE)

Added Prometheus metrics, per-route labels, request ID propagation, and config hot-reload.

### 1. Prometheus Metrics Endpoint

**Solution:** Added `github.com/prometheus/client_golang` dependency. Centralized metrics registry in `internal/metrics/metrics.go` with 7 collectors: `gateway_requests_total` (counter by route/method/status), `gateway_request_duration_seconds` (histogram by route/method), `gateway_active_connections` (gauge), `gateway_rate_limit_hits_total` (counter by route), `gateway_auth_failures_total` (counter by reason), `gateway_backend_errors_total` (counter by route/backend/status), `gateway_retries_total` (counter by route/backend). `/metrics` endpoint bypasses auth and rate limiting like health endpoints.

**Files:** `internal/metrics/metrics.go`, `cmd/gateway/main.go`

### 2. Per-Route Metrics Labels

**Solution:** All metrics are labeled with route path prefix and backend URL where applicable. Request count, latency, retries, and backend errors are tracked per-route for per-service dashboards.

**Files:** `internal/proxy/proxy.go`, `internal/ratelimit/ratelimit.go`, `internal/auth/auth.go`

### 3. Request ID Propagation

**Solution:** Extracted request ID generation from proxy into a dedicated `RequestID` middleware that runs early in the chain. The ID is stored in the request context via `middleware.GetRequestID(ctx)` so all downstream middleware (logging, recovery) can access it. `X-Request-ID` is set on both response and request headers for backend propagation.

**Files:** `internal/middleware/requestid.go`, `internal/middleware/logging.go`, `internal/middleware/recovery.go`, `internal/proxy/proxy.go`

### 4. Config Hot-Reload

**Solution:** Added `config.Reloader` with dual reload triggers: fsnotify file watcher (cross-platform, with 300ms debounce) and SIGHUP signal handler (Unix only, no-op on Windows via build tags). Validates new config before swapping — invalid configs are rejected and the old config is preserved. Logs a diff summary of changed fields. Components register callbacks to receive new config (rate limiter `UpdateConfig` clears existing limiters so new rates apply immediately).

**Files:** `internal/config/reload.go`, `internal/config/reload_unix.go`, `internal/config/reload_windows.go`, `internal/ratelimit/ratelimit.go`

### Middleware Chain (post-Phase 2)

```
Recovery → RequestID → SecurityHeaders → Logging → CORS → BodyLimit → RateLimit → Auth → Proxy
```

### Config Additions (backward-compatible)

```yaml
metrics:
  enabled: true      # optional, default: true
  path: "/metrics"   # optional, default: "/metrics"
```

### Dependencies Added

- `github.com/prometheus/client_golang` v1.19.1 — Prometheus instrumentation
- `github.com/fsnotify/fsnotify` v1.7.0 — Cross-platform file system notifications

---

## Phase 3 — Resilience & Reliability (COMPLETE)

Hardened the gateway against backend failures and load spikes with four composable circuit breaker types, global request deadlines, graceful degradation, and per-backend connection pooling.

### 1. Circuit Breaker Package

Four composable circuit breaker types in `internal/circuitbreaker/`, composed via `CompositeBreaker`:

**a) Failure-Rate Breaker** (`failure_rate.go`) — Sliding-window failure-rate tracking. Opens when failure ratio exceeds configurable threshold over the window. Three-state machine: Closed → Open → Half-Open → Closed. Ring buffer for O(1) window management.

**b) Timeout Breaker** (`timeout.go`) — Wraps inner breaker and treats slow responses (above `slow_threshold`) as failures. No goroutine leaks — latency measured by caller.

**c) Bulkhead Breaker** (`bulkhead.go`) — Limits concurrent in-flight requests per backend via buffered channel semaphore. Non-blocking rejection at limit. Prevents goroutine pileups and resource starvation.

**d) Adaptive Breaker** (`adaptive.go`) — Dynamically adjusts failure-rate threshold based on EWMA latency. Tightens threshold when latency rises above ceiling, relaxes when it drops. No extra goroutines.

**Composition** (`composite.go`): Factory builds layered stack based on config: FailureRate → Adaptive → Timeout → Bulkhead. Proxy interacts only with `CompositeBreaker`.

**Files:** `internal/circuitbreaker/breaker.go`, `failure_rate.go`, `timeout.go`, `bulkhead.go`, `adaptive.go`, `composite.go`

### 2. Request Timeout Improvements

**Solution:** Added `Deadline` middleware (`internal/middleware/deadline.go`) that wraps the entire request context with `context.WithTimeout`. Returns 504 when deadline fires. Configurable via `server.global_timeout_ms` (0 = disabled). Context cancellation checked between retry attempts in proxy for clean propagation.

**Files:** `internal/middleware/deadline.go`, `internal/proxy/proxy.go`, `internal/config/config.go`

### 3. Graceful Degradation

**Solution:** Health endpoint uses circuit breaker state as fast path — skips TCP dial when breaker state is known. Reports per-backend status with new strings: `"ok"`, `"circuit-open"`, `"circuit-half-open"`, `"unreachable"`. Optional per-route fallback responses (configurable `fallback_status` and `fallback_body`) served when circuit is open.

**Files:** `internal/health/health.go`, `internal/proxy/proxy.go`, `internal/config/config.go`

### 4. Connection Pooling

**Solution:** Per-backend `http.Transport` with configurable `MaxIdleConns`, `MaxIdleConnsPerHost`, `IdleConnTimeout` via route-level `connection_pool` config. Sensible defaults (100, 10, 90s). Custom dialer with 10s connect timeout and 30s keepalive.

**Files:** `internal/proxy/proxy.go`, `internal/config/config.go`

### Middleware Chain (post-Phase 3)

```
Recovery → RequestID → Deadline → SecurityHeaders → Logging → CORS → BodyLimit → RateLimit → Auth → Proxy
```

### Metrics Added

- `gateway_circuit_breaker_state_changes_total` (counter: backend/from/to)
- `gateway_circuit_breaker_state` (gauge: backend, 0=closed/1=open/2=half-open)
- `gateway_bulkhead_rejections_total` (counter: backend)
- `gateway_bulkhead_in_flight` (gauge: backend)

### Config Additions (backward-compatible)

```yaml
server:
  global_timeout_ms: 60000    # optional, default: 0 (disabled)

circuit_breaker:
  window_size: 10             # optional, default: 10
  failure_threshold: 0.5      # optional, default: 0.5
  reset_timeout: 30s          # optional, default: 30s
  half_open_max: 2            # optional, default: 2
  slow_threshold: 5s          # optional, default: 0 (disabled)
  max_concurrent: 100         # optional, default: 0 (disabled)
  adaptive: false             # optional, default: false
  latency_ceiling: 2s         # optional, default: 2s (when adaptive)
  min_threshold: 0.2          # optional, default: 0.2 (when adaptive)

routes:
  - path_prefix: "/api/users"
    connection_pool:           # optional per-route
      max_idle_conns: 100
      max_idle_per_host: 10
      idle_timeout: 90s
    fallback_status: 200       # optional, served when circuit open
    fallback_body: '{"status":"degraded"}'
```

---

## Phase 4 — Operational Hardening (COMPLETE)

Production deployment and operational tooling. All changes are backward-compatible — existing configs work without modification.

### 1. Structured Error Taxonomy

**Solution:** Centralized error response package (`internal/apierror/`) with typed `ErrorCode`, standardized `ErrorResponse` struct, and a single `WriteJSON` function used by all error-producing components. 12 machine-readable error codes form a stable API contract (`GATEWAY_*` prefix). Pre-serialized JSON bodies for the 6 most common errors avoid allocation in the hot path. Internal details (stack traces, upstream errors) are never exposed to clients. Request ID is included when available.

**Files:** `internal/apierror/apierror.go`

**Migrated:** `internal/proxy/proxy.go`, `internal/auth/auth.go`, `internal/ratelimit/ratelimit.go`, `internal/middleware/recovery.go`, `internal/middleware/deadline.go`, `internal/middleware/bodylimit.go`

### 2. Access Logging Improvements

**a) Per-Route Log Levels** — Each route accepts a `log_level` field: `"debug"`, `"info"`, `"warn"`, `"error"`, or `"none"`. Default: `"info"`. The `"none"` level completely suppresses access log entries for that route (ideal for health checks). The Logging middleware accepts a `routeLogLevel` callback (same pattern as `routeRequiresAuth`) and uses `logger.Log(ctx, level, ...)`.

**b) Request/Response Body Logging** — Opt-in via `logging.body_logging: true`. Captures request body via TeeReader and response body via a wrapping ResponseWriter. Bodies truncated to `max_body_log_bytes` (default 4096). Only text-based content types (JSON, text/*, XML, form-urlencoded) are captured. Sensitive fields (`password`, `secret`, `token`, `key`, `authorization`) are redacted to `"***"` before logging.

**c) Log Output to File with Rotation** — New `internal/logging/writer.go` implements `io.Writer` + `io.Closer` with size-based rotation. Pure stdlib, no external dependencies. Rotated files are named `<base>-<timestamp><ext>`. Old files are cleaned up based on `max_backups` and `max_age_days`.

**Files:** `internal/middleware/logging.go`, `internal/logging/writer.go`, `internal/config/config.go`, `cmd/gateway/main.go`

### 3. TLS Termination

**Solution:** Native TLS support via `server.tls` config. When enabled, the server uses `ListenAndServeTLS` with a `tls.Config.GetCertificate` callback for runtime certificate rotation. `CertLoader` (`internal/tlsutil/certloader.go`) loads the cert/key pair at startup and watches both files via fsnotify with 300ms debounce. Certificate swaps happen under `sync.RWMutex` — active connections are not dropped. Minimum TLS version is configurable (`"1.2"` or `"1.3"`), default `1.2`.

**Files:** `internal/tlsutil/certloader.go`, `internal/config/config.go`, `cmd/gateway/main.go`

### 4. Admin API

**Solution:** Read-only admin API (`internal/admin/admin.go`) with three inspection endpoints, all protected by IP allowlist (CIDR-based). Registered on a separate mux that bypasses the public middleware stack (same pattern as health/metrics). No mutating operations.

- **`GET /admin/routes`** — Returns routes with backend URL, methods, auth_required, timeout, and current circuit breaker state (`"closed"`, `"open"`, `"half-open"`)
- **`GET /admin/config`** — Returns running config from `Reloader.Current()` with `jwt_secret` redacted to `"***"`
- **`GET /admin/limiters`** — Returns active rate limiter entries (IP, rate, burst, last_seen) with pagination (`?page=1&page_size=100`)

Rate limiter inspection is supported by a new `Snapshot()` method on `ratelimit.Limiter` that reads under `RLock`.

**Files:** `internal/admin/admin.go`, `internal/ratelimit/ratelimit.go`, `internal/config/config.go`, `cmd/gateway/main.go`

### Config Additions (backward-compatible)

```yaml
logging:
  output: "stdout"           # "stdout", "stderr", or file path
  max_size_mb: 100           # rotation threshold (file output only)
  max_backups: 3             # rotated files to keep
  max_age_days: 30           # max age of rotated files
  body_logging: false        # opt-in request/response body logging
  max_body_log_bytes: 4096   # max body bytes per request

server:
  tls:
    enabled: false
    cert_file: ""
    key_file: ""
    min_version: "1.2"       # "1.2" or "1.3"

admin:
  enabled: false
  ip_allowlist: []           # required when enabled, CIDR notation

routes:
  - path_prefix: "/health"
    log_level: "none"        # per-route: "debug", "info", "warn", "error", "none"
```

### Documentation

- [`docs/ERROR_CODES.md`](ERROR_CODES.md) — Full error code reference with response format, catalog, and client usage examples

---

## Phase 5 — Testing & CI/CD (COMPLETE)

Raised test quality and automated the release pipeline with integration tests, fuzz tests, load tests, and GitHub Actions CI/CD.

### 1. Integration Test Suite

**Solution:** Docker Compose-based integration tests in `tests/integration/` using `//go:build integration` tag. `TestMain` manages the full lifecycle: writes `.env`, runs `docker compose up --build`, polls `/health` until ready, executes tests, then tears down. Tests exercise the real gateway binary over HTTP against echoserver backends. A dedicated `configs/integration-gateway.yaml` provides deterministic settings (lower rate limits, small circuit breaker window) for reliable test outcomes.

**Test Scenarios (~20 tests):** Health/readiness endpoints, JWT auth flows (valid/expired/missing/wrong-scope/garbage), routing (404/405/path boundary/prefix stripping/header injection), rate limiting burst exhaustion, retry behavior (502 retries), circuit breaker trips (admin state inspection + 503 on open), metrics endpoint, admin API (routes/config/limiters), security headers, request ID generation/preservation/uniqueness, and error response format consistency.

**Files:** `tests/integration/integration_test.go`, `tests/integration/helpers_test.go`, `tests/integration/docker-compose.integration.yaml`, `configs/integration-gateway.yaml`

### 2. Load Testing

**Solution:** k6-based load test scripts in `tests/load/`. Baseline test runs two scenarios concurrently: public traffic (200 rps) and authenticated traffic (100 rps) for 30 seconds. CI-enforced thresholds: p99 < 500ms, error rate < 1%. Stress test ramps from 50 to 500 VUs to find breaking points. A Go token generator (`gen-token.go`) creates JWTs for load testing without external dependencies.

**Files:** `tests/load/k6-baseline.js`, `tests/load/k6-stress.js`, `tests/load/gen-token.go`, `tests/load/results/.gitkeep`

### 3. Fuzz Testing

**Solution:** Native Go `testing.F` fuzz tests placed alongside the packages they test. Three fuzz targets:

- **`FuzzLoadFromBytes`** (`internal/config/fuzz_test.go`) — Feeds random YAML to config parser, verifies no panics, checks post-parse invariants (port range, positive RPS).
- **`FuzzMatchesPrefix`** (`internal/routing/fuzz_test.go`) — Feeds random (path, prefix) pairs, verifies boundary enforcement invariant holds on all matches.
- **`FuzzAuthMiddleware`** (`internal/auth/fuzz_test.go`) — Feeds random Authorization headers through real middleware, verifies no panics and only valid HTTP status codes (200/401/403).

**Files:** `internal/config/fuzz_test.go`, `internal/routing/fuzz_test.go`, `internal/auth/fuzz_test.go`

### 4. CI Pipeline

**Solution:** GitHub Actions CI pipeline (`.github/workflows/ci.yml`) with six jobs in dependency order: lint (`go vet` + `staticcheck`) → build (compile binaries) + unit-test (`go test -race -cover`, parallel) → fuzz (matrix of 3 targets, 30s each) → integration-test (docker compose stack) → docker-build (verify image builds). Release pipeline (`.github/workflows/release.yml`) triggers on `v*` tags: builds and pushes to GitHub Container Registry (`ghcr.io`), generates SBOM via `anchore/sbom-action` (SPDX format). Dependabot (`.github/dependabot.yml`) configured for weekly updates of Go modules, Docker base images, and GitHub Actions.

**Files:** `.github/workflows/ci.yml`, `.github/workflows/release.yml`, `.github/dependabot.yml`

### Makefile Additions

```
make test-integration  # Start stack, run integration tests, teardown
make test-fuzz         # Run all 3 fuzz targets for 30s each
make test-load         # Generate JWT, run k6 baseline
```

---

## Priority Order

| Phase | Focus | Status | Dependencies |
|-------|-------|--------|-------------|
| 1 | Security Hardening | **Complete** | None |
| 2 | Observability & Metrics | **Complete** | `prometheus/client_golang`, `fsnotify/fsnotify` |
| 3 | Resilience & Reliability | **Complete** | Phase 2 (metrics for circuit breaker) |
| 4 | Operational Hardening | **Complete** | Phase 1 (TLS needs security headers) |
| 5 | Testing & CI/CD | **Complete** | Phase 2-3 (integration tests need metrics + circuit breaker) |

Phases 2 and 4 can run in parallel. Phase 3 benefits from Phase 2 metrics. Phase 5 should come last since it tests features from all prior phases.
