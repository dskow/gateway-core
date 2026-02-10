# Mission-Critical Roadmap

A phased plan to raise go-api-gateway from a functional prototype to production-grade, mission-critical infrastructure.

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

## Phase 3 — Resilience & Reliability (PLANNED)

Harden the gateway against backend failures and load spikes.

### 1. Circuit Breaker

- Per-backend circuit breaker (closed → open → half-open state machine)
- Configurable failure threshold, timeout, and half-open probe count
- Return 503 with structured error when circuit is open
- Metrics integration: circuit state changes, trip counts

### 2. Request Timeout Improvements

- Add per-route read/write timeout overrides (currently only connect+response timeout)
- Add global request deadline that covers the entire middleware chain
- Ensure context cancellation propagates cleanly through retries

### 3. Graceful Degradation

- Health endpoint reports per-backend status (not just all-or-nothing)
- Readiness probe fails only when all backends for a route are down
- Optional fallback responses for non-critical routes

### 4. Connection Pooling

- Configure `Transport.MaxIdleConns`, `MaxIdleConnsPerHost`, `IdleConnTimeout` per backend
- Expose connection pool metrics via Prometheus

---

## Phase 4 — Operational Hardening (PLANNED)

Production deployment and operational tooling.

### 1. Structured Error Taxonomy

- Standardize error response format with error codes, not just messages
- Add `error_code` field for machine-readable error classification
- Document all error codes for API consumers

### 2. Access Logging Improvements

- Add configurable log levels per route (reduce noise for health checks)
- Add request/response body logging option for debugging (opt-in, size-limited)
- Support log output to file with rotation

### 3. TLS Termination

- Native TLS support with cert/key config
- Auto-reload certificates on file change (for Let's Encrypt rotation)
- Minimum TLS version enforcement (1.2+)

### 4. Admin API

- `/admin/routes` — list configured routes and their status
- `/admin/config` — view running config (secrets redacted)
- `/admin/limiters` — view active rate limiter entries
- Protected by separate auth or IP allowlist

---

## Phase 5 — Testing & CI/CD (PLANNED)

Raise test quality and automate the release pipeline.

### 1. Integration Test Suite

- Docker Compose-based integration tests that exercise the full stack
- Test scenarios: auth flows, rate limiting under load, retry behavior, circuit breaker trips
- Run in CI on every PR

### 2. Load Testing

- k6 or vegeta load test scripts in `tests/load/`
- Baseline performance benchmarks with documented results
- Regression detection: fail CI if p99 latency exceeds threshold

### 3. Fuzz Testing

- Fuzz config parsing (`go test -fuzz`)
- Fuzz path matching logic
- Fuzz JWT token validation

### 4. CI Pipeline

- GitHub Actions workflow: lint → build → unit test → integration test → Docker build
- Auto-publish Docker image on tag
- Dependabot for dependency updates
- SBOM generation for supply chain security

---

## Priority Order

| Phase | Focus | Status | Dependencies |
|-------|-------|--------|-------------|
| 1 | Security Hardening | **Complete** | None |
| 2 | Observability & Metrics | **Complete** | `prometheus/client_golang`, `fsnotify/fsnotify` |
| 3 | Resilience & Reliability | Planned | Phase 2 (metrics for circuit breaker) |
| 4 | Operational Hardening | Planned | Phase 1 (TLS needs security headers) |
| 5 | Testing & CI/CD | Planned | Phase 2-3 (integration tests need metrics + circuit breaker) |

Phases 2 and 4 can run in parallel. Phase 3 benefits from Phase 2 metrics. Phase 5 should come last since it tests features from all prior phases.
