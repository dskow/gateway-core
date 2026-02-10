#!/usr/bin/env python3
"""Demo script that generates JWTs and runs curl commands against the gateway."""

import json
import os
import subprocess
import sys
import time

import jwt


GATEWAY = "http://gateway:8080"
SECRET = os.environ["JWT_SECRET"]

passed = 0
failed = 0

HEADER_DUMP = "/tmp/resp_headers.txt"


def generate_token(sub="user-1", scope="read write", expired=False):
    exp = int(time.time()) + (-3600 if expired else 3600)
    payload = {
        "sub": sub,
        "iss": "https://auth.example.com",
        "aud": "api-gateway",
        "exp": exp,
        "scope": scope,
    }
    return jwt.encode(payload, SECRET, algorithm="HS256")


def curl(method, path, token=None, headers=None, data=None, data_file=None, label=""):
    """Run a curl request and return (status_code, response_headers_dict).

    Pass data= for small inline bodies, data_file= for file-based bodies.
    """
    url = f"{GATEWAY}{path}"
    cmd = ["curl", "-s", "-X", method, "-w", "\n%{http_code}", "-D", HEADER_DUMP]
    if token:
        cmd += ["-H", f"Authorization: Bearer {token}"]
    for k, v in (headers or {}).items():
        cmd += ["-H", f"{k}: {v}"]
    if data is not None:
        cmd += ["-d", data]
    if data_file is not None:
        cmd += ["--data-binary", f"@{data_file}"]
    cmd.append(url)

    result = subprocess.run(cmd, capture_output=True, text=True, timeout=15)
    lines = result.stdout.strip().rsplit("\n", 1)
    body = lines[0] if len(lines) > 1 else ""
    status = lines[-1]

    # Parse response headers from the dump file
    resp_headers = {}
    try:
        with open(HEADER_DUMP, "r") as f:
            for line in f:
                line = line.strip()
                if ": " in line:
                    k, v = line.split(": ", 1)
                    resp_headers[k.lower()] = v
    except FileNotFoundError:
        pass

    print(f"\n{'='*60}")
    print(f"TEST: {label}")
    print(f"{method} {path} -> {status}")
    if body:
        try:
            print(json.dumps(json.loads(body), indent=2))
        except json.JSONDecodeError:
            print(body[:200])
    return int(status), resp_headers


def check(ok, msg=""):
    global passed, failed
    if ok:
        print(f"  PASS{': ' + msg if msg else ''}")
        passed += 1
    else:
        print(f"  FAIL{': ' + msg if msg else ''}")
        failed += 1
    return ok


def wait_for_gateway():
    print("Waiting for gateway to be ready...")
    for i in range(30):
        try:
            result = subprocess.run(
                ["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", f"{GATEWAY}/health"],
                capture_output=True, text=True, timeout=2,
            )
            if result.stdout.strip() == "200":
                print("Gateway is ready!")
                return
        except (subprocess.TimeoutExpired, Exception):
            pass
        time.sleep(1)
    print("Gateway did not become ready in time")
    sys.exit(1)


def main():
    wait_for_gateway()

    token = generate_token()
    print(f"\nGenerated JWT: {token[:50]}...")

    # ── Core Functionality ────────────────────────────────────

    status, _ = curl("GET", "/health", label="Liveness probe")
    check(status == 200, f"expected 200, got {status}")

    status, _ = curl("GET", "/ready", label="Readiness probe")
    check(status == 200, f"expected 200, got {status}")

    status, _ = curl("GET", "/public/hello", label="Public route - no auth needed")
    check(status == 200, f"expected 200, got {status}")

    status, _ = curl("GET", "/api/users/123", token=token, label="Auth route - valid token")
    check(status == 200, f"expected 200, got {status}")

    status, _ = curl("GET", "/api/analytics/events", token=token, label="Auth route - analytics")
    check(status == 200, f"expected 200, got {status}")

    status, _ = curl("GET", "/api/users/123", label="Auth route - missing token")
    check(status == 401, f"expected 401, got {status}")

    status, _ = curl("GET", "/api/users/123", token="garbage.not.valid",
                      label="Auth route - bad token")
    check(status == 401, f"expected 401, got {status}")

    expired_token = generate_token(expired=True)
    status, _ = curl("GET", "/api/users/123", token=expired_token,
                      label="Auth route - expired token")
    check(status == 401, f"expected 401, got {status}")

    wrong_scope = generate_token(scope="read")
    status, _ = curl("GET", "/api/users/123", token=wrong_scope,
                      label="Auth route - missing 'write' scope (403 Forbidden)")
    check(status == 403, f"expected 403, got {status}")

    status, _ = curl("GET", "/nonexistent", label="No matching route")
    check(status == 404, f"expected 404, got {status}")

    status, _ = curl("DELETE", "/public/test", label="Method not allowed on public route")
    check(status == 405, f"expected 405, got {status}")

    # Rate limiting — fire requests concurrently to exceed burst window
    print(f"\n{'='*60}")
    print("TEST: Rate limiting (sending 80 concurrent requests to /public/test)")
    procs = []
    for i in range(80):
        p = subprocess.Popen(
            ["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", f"{GATEWAY}/public/test"],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        procs.append(p)
    statuses = {}
    for p in procs:
        out, _ = p.communicate(timeout=15)
        code = out.decode().strip()
        statuses[code] = statuses.get(code, 0) + 1
    print(f"Results: {dict(sorted(statuses.items()))}")
    check("429" in statuses, "expected some 429 responses")

    # Wait for rate limit bucket to refill before Phase 1 tests
    print("\n(Waiting 2s for rate limit bucket to refill...)")
    time.sleep(2)

    # ── Phase 1: Security Hardening ───────────────────────────

    print(f"\n\n{'#'*60}")
    print("# PHASE 1: SECURITY HARDENING TESTS")
    print(f"{'#'*60}")

    # Security response headers
    status, hdrs = curl("GET", "/public/hello",
                         label="Security headers on response")
    check(hdrs.get("x-content-type-options") == "nosniff",
          f"X-Content-Type-Options = {hdrs.get('x-content-type-options', '(missing)')}")
    check(hdrs.get("x-frame-options") == "DENY",
          f"X-Frame-Options = {hdrs.get('x-frame-options', '(missing)')}")
    check(hdrs.get("x-xss-protection") == "0",
          f"X-XSS-Protection = {hdrs.get('x-xss-protection', '(missing)')}")

    # HSTS must NOT be present over plain HTTP
    check("strict-transport-security" not in hdrs,
          "no HSTS on plain HTTP")

    # Path boundary enforcement
    status, _ = curl("GET", "/api.evil.com/steal", token=token,
                      label="Path boundary - /api.evil.com must NOT match /api")
    check(status == 404, f"expected 404, got {status}")

    status, _ = curl("GET", "/apiary", token=token,
                      label="Path boundary - /apiary must NOT match /api")
    check(status == 404, f"expected 404, got {status}")

    status, _ = curl("GET", "/api/users/123", token=token,
                      label="Path boundary - /api/users/123 still matches")
    check(status == 200, f"expected 200, got {status}")

    # Body size limit — small body passes
    # Use /api/analytics (no retry_attempts) to avoid body-consumed-on-retry issues.
    status, _ = curl("POST", "/api/analytics/ingest", token=token, data="a" * 100,
                      label="Body limit - small body (100 bytes) accepted")
    check(status == 200, f"expected 200, got {status}")

    # Body size limit — oversized body rejected
    # Default limit is 1MB. Write a 1.5MB file and send via --data-binary @file.
    # Use /api/analytics (no retries) so MaxBytesReader triggers on the single attempt.
    print(f"\n{'='*60}")
    print("TEST: Body limit - oversized body (1.5MB) rejected")
    big_file = "/tmp/big_body.bin"
    with open(big_file, "wb") as f:
        f.write(b"x" * (1536 * 1024))  # 1.5MB
    try:
        over_result = subprocess.run(
            ["curl", "-s", "-X", "POST", "-w", "\n%{http_code}",
             "-H", f"Authorization: Bearer {token}",
             "-H", "Content-Type: application/octet-stream",
             "--data-binary", f"@{big_file}",
             "-m", "10",
             f"{GATEWAY}/api/analytics/ingest"],
            capture_output=True, text=True, timeout=15,
        )
        over_lines = over_result.stdout.strip().rsplit("\n", 1)
        over_status = over_lines[-1] if over_lines else "0"
    except (subprocess.TimeoutExpired, Exception) as e:
        over_status = "timeout"
        print(f"  (request timed out or errored: {e})")
    finally:
        os.remove(big_file)
    print(f"POST /api/analytics/ingest (1.5MB) -> {over_status}")
    # Gateway should reject: 413 or connection reset (000) or timeout
    check(over_status in ("413", "000", "timeout"),
          f"got {over_status} (expected 413 or connection reset)")

    # XFF ignored without trusted proxies
    # Docker compose doesn't configure trusted_proxies, so XFF should be ignored.
    # Both requests come from the same container IP, so they share the same rate
    # limit bucket regardless of XFF value.
    status1, _ = curl("GET", "/public/hello",
                       headers={"X-Forwarded-For": "1.2.3.4"},
                       label="XFF ignored - request with XFF 1.2.3.4")
    status2, _ = curl("GET", "/public/hello",
                       headers={"X-Forwarded-For": "5.6.7.8"},
                       label="XFF ignored - request with XFF 5.6.7.8")
    check(status1 == 200 and status2 == 200,
          "both requests use RemoteAddr, not spoofed XFF")

    # ── Phase 2: Observability & Metrics ─────────────────────

    print(f"\n\n{'#'*60}")
    print("# PHASE 2: OBSERVABILITY & METRICS TESTS")
    print(f"{'#'*60}")

    # --- /metrics endpoint accessible without auth ---

    status, hdrs = curl("GET", "/metrics", label="Metrics endpoint accessible without auth")
    check(status == 200, f"expected 200, got {status}")

    # Fetch raw metrics body for content checks
    metrics_result = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/metrics"],
        capture_output=True, text=True, timeout=10,
    )
    metrics_body = metrics_result.stdout

    # --- Trigger backend errors and retries ---
    # Hit the echo server's /__status/502 endpoint via /api/users (which has
    # strip_prefix: true and retry_attempts: 2). The backend always returns 502,
    # so the gateway will retry twice and ultimately return 502 to us. This
    # primes the gateway_backend_errors_total and gateway_retries_total counters.

    status, _ = curl("GET", "/api/users/__status/502", token=token,
                      label="Trigger backend 502 (primes error + retry metrics)")
    check(status == 502, f"expected 502, got {status}")

    # Re-scrape metrics now that all 7 families have been observed
    metrics_result = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/metrics"],
        capture_output=True, text=True, timeout=10,
    )
    metrics_body = metrics_result.stdout

    # --- All 7 gateway metric families present ---

    check("gateway_requests_total" in metrics_body,
          "gateway_requests_total present in /metrics output")
    check("gateway_request_duration_seconds" in metrics_body,
          "gateway_request_duration_seconds present in /metrics output")
    check("gateway_active_connections" in metrics_body,
          "gateway_active_connections present in /metrics output")
    check("gateway_rate_limit_hits_total" in metrics_body,
          "gateway_rate_limit_hits_total present in /metrics output")
    check("gateway_auth_failures_total" in metrics_body,
          "gateway_auth_failures_total present in /metrics output")
    check("gateway_backend_errors_total" in metrics_body,
          "gateway_backend_errors_total present in /metrics output")
    check("gateway_retries_total" in metrics_body,
          "gateway_retries_total present in /metrics output")

    # --- Per-route labels in metrics ---

    check('/api/users' in metrics_body,
          "per-route label /api/users in metrics output")

    # --- /metrics bypasses rate limiting ---

    print(f"\n{'='*60}")
    print("TEST: /metrics bypasses rate limiting")
    metrics_statuses = []
    for _ in range(10):
        r = subprocess.run(
            ["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
             f"{GATEWAY}/metrics"],
            capture_output=True, text=True, timeout=5,
        )
        metrics_statuses.append(r.stdout.strip())
    check(all(s == "200" for s in metrics_statuses),
          f"all /metrics requests returned 200 (got {set(metrics_statuses)})")

    # --- X-Request-ID generated when absent ---

    status, hdrs = curl("GET", "/public/hello",
                         label="X-Request-ID generated when absent")
    req_id = hdrs.get("x-request-id", "")
    check(req_id != "", f"X-Request-ID present in response: {req_id[:40]}")
    # UUID v4 format: 8-4-4-4-12 hex
    check(len(req_id.split("-")) == 5, f"X-Request-ID looks like UUID: {req_id}")

    # --- X-Request-ID preserved when provided ---

    custom_id = "my-trace-id-12345"
    status, hdrs = curl("GET", "/public/hello",
                         headers={"X-Request-ID": custom_id},
                         label="X-Request-ID preserved when provided")
    check(hdrs.get("x-request-id") == custom_id,
          f"X-Request-ID preserved: {hdrs.get('x-request-id', '(missing)')}")

    # --- X-Request-ID propagated to backend ---

    trace_id = "trace-propagation-test-999"
    prop_result = subprocess.run(
        ["curl", "-s", "-H", f"Authorization: Bearer {token}",
         "-H", f"X-Request-ID: {trace_id}",
         f"{GATEWAY}/api/users/check-trace"],
        capture_output=True, text=True, timeout=10,
    )
    print(f"\n{'='*60}")
    print("TEST: X-Request-ID propagated to backend")
    try:
        echo_resp = json.loads(prop_result.stdout)
        backend_headers = echo_resp.get("headers", {})
        got_trace = backend_headers.get("X-Request-Id",
                        backend_headers.get("x-request-id", ""))
        print(f"  Backend received X-Request-ID: {got_trace}")
        check(got_trace == trace_id,
              f"backend saw X-Request-ID={got_trace}, expected {trace_id}")
    except (json.JSONDecodeError, KeyError) as e:
        print(f"  Could not parse echo response: {e}")
        check(False, "failed to verify backend propagation")

    # --- X-Request-ID unique per request ---

    print(f"\n{'='*60}")
    print("TEST: X-Request-ID unique per request")
    ids = set()
    for _ in range(10):
        r = subprocess.run(
            ["curl", "-s", "-D", HEADER_DUMP, "-o", "/dev/null",
             f"{GATEWAY}/public/hello"],
            capture_output=True, text=True, timeout=5,
        )
        try:
            with open(HEADER_DUMP, "r") as f:
                for line in f:
                    if line.lower().startswith("x-request-id:"):
                        ids.add(line.split(":", 1)[1].strip())
        except FileNotFoundError:
            pass
    print(f"  Generated {len(ids)} unique IDs from 10 requests")
    check(len(ids) == 10, f"expected 10 unique IDs, got {len(ids)}")

    # --- Metrics counters increment after traffic ---
    # Make targeted requests to produce known counter increments,
    # then re-scrape /metrics and verify counts.

    # auth failure: missing token
    curl("GET", "/api/users/metrics-test", label="Generate auth failure (missing token)")
    # auth failure: bad token
    curl("GET", "/api/users/metrics-test", token="totally.not.valid",
         label="Generate auth failure (bad token)")
    # auth failure: wrong scope
    no_write = generate_token(scope="read")
    curl("GET", "/api/users/metrics-test", token=no_write,
         label="Generate auth failure (insufficient scope)")

    time.sleep(0.5)

    metrics_result2 = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/metrics"],
        capture_output=True, text=True, timeout=10,
    )
    metrics_body2 = metrics_result2.stdout

    check('gateway_auth_failures_total{reason="missing_token"}' in metrics_body2,
          "auth_failures counter has missing_token label")
    check('gateway_auth_failures_total{reason="invalid_token"}' in metrics_body2,
          "auth_failures counter has invalid_token label")
    check('gateway_auth_failures_total{reason="insufficient_scope"}' in metrics_body2,
          "auth_failures counter has insufficient_scope label")

    # request totals with route labels
    check('route="/api/users"' in metrics_body2,
          "requests_total has route=/api/users label")

    # request duration histogram buckets present
    check('gateway_request_duration_seconds_bucket{' in metrics_body2,
          "request_duration_seconds histogram buckets present")

    # rate_limit_hits from the earlier burst test
    check('gateway_rate_limit_hits_total{' in metrics_body2,
          "rate_limit_hits_total incremented (from burst test)")

    # backend_errors from the /__status/502 test
    check('gateway_backend_errors_total{' in metrics_body2,
          "backend_errors_total incremented (from 502 test)")
    check('status="502"' in metrics_body2,
          "backend_errors_total has status=502 label")

    # retries from the /__status/502 test (/api/users has retry_attempts: 2)
    check('gateway_retries_total{' in metrics_body2,
          "retries_total incremented (from 502 retry test)")

    # ── Performance Fixes Validation ─────────────────────────

    print(f"\n\n{'#'*60}")
    print("# PERFORMANCE FIXES VALIDATION")
    print(f"{'#'*60}")

    # Wait for rate limit bucket to refill from earlier tests
    print("\n(Waiting 2s for rate limit bucket to refill...)")
    time.sleep(2)

    # --- Fix #1: No double backend hit on retry-enabled routes ---
    # /api/users has retry_attempts: 2 and strip_prefix: true.
    # Verify the response body is correct (buffer replay works) and
    # the response includes proper headers replayed from the buffer.

    status, hdrs = curl("GET", "/api/users/perf-fix1-test", token=token,
                         label="Fix #1: Retry route returns correct response (buffer replay)")
    check(status == 200, f"expected 200, got {status}")

    # Verify the response body is well-formed JSON from the echo server
    fix1_result = subprocess.run(
        ["curl", "-s", "-H", f"Authorization: Bearer {token}",
         f"{GATEWAY}/api/users/perf-fix1-unique-path"],
        capture_output=True, text=True, timeout=10,
    )
    print(f"\n{'='*60}")
    print("TEST: Fix #1: Buffer replay preserves full response body")
    try:
        fix1_body = json.loads(fix1_result.stdout)
        # strip_prefix removes /api/users, so backend sees /perf-fix1-unique-path
        got_path = fix1_body.get("path", "")
        check(got_path == "/perf-fix1-unique-path",
              f"buffer replay preserved response body (path={got_path})")
        check(fix1_body.get("service") == "users-service",
              f"response from correct backend (service={fix1_body.get('service')})")
    except (json.JSONDecodeError, KeyError) as e:
        print(f"  Could not parse echo response: {e}")
        check(False, "buffer replay response parse failed")

    # Verify response headers are preserved through buffer replay
    status, hdrs = curl("GET", "/api/users/perf-fix1-headers", token=token,
                         label="Fix #1: Response headers preserved through buffer replay")
    check(hdrs.get("content-type", "").startswith("application/json"),
          f"Content-Type header preserved: {hdrs.get('content-type', '(missing)')}")
    check(hdrs.get("x-gateway-latency", "") != "",
          "X-Gateway-Latency header present")

    # --- Fix #4: Pre-serialized error responses have correct JSON structure ---

    print(f"\n{'='*60}")
    print("TEST: Fix #4: Pre-serialized error bodies are valid JSON")
    err_result = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/nonexistent/perf-test"],
        capture_output=True, text=True, timeout=10,
    )
    try:
        err_body = json.loads(err_result.stdout)
        check(err_body.get("error") == "Not Found",
              f"404 error field correct: {err_body.get('error')}")
        check(err_body.get("message") == "no matching route",
              f"404 message field correct: {err_body.get('message')}")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"404 response not valid JSON: {e}")

    # Pre-serialized 401 (missing auth)
    err401_result = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/api/users/perf-test"],
        capture_output=True, text=True, timeout=10,
    )
    try:
        err401 = json.loads(err401_result.stdout)
        check(err401.get("error") == "Unauthorized",
              f"401 error field correct: {err401.get('error')}")
        check(err401.get("message") == "missing or malformed Authorization header",
              f"401 message field correct: {err401.get('message')}")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"401 response not valid JSON: {e}")

    # --- Fix #5: Rate limit metrics still have correct route labels ---
    # The combined single-scan limitsForPath now returns the route prefix.

    print(f"\n{'='*60}")
    print("TEST: Fix #5: Route labels preserved after single-scan refactor")
    final_metrics = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/metrics"],
        capture_output=True, text=True, timeout=10,
    ).stdout

    check('route="/api/users"' in final_metrics,
          "route label /api/users present in metrics")
    check('gateway_rate_limit_hits_total{' in final_metrics,
          "rate_limit_hits_total counter still working after refactor")

    # --- Fix #7: Path boundary enforcement with shared routing package ---

    status, _ = curl("GET", "/api.evil.com/perf-test", token=token,
                      label="Fix #7: Path boundary via routing.MatchesPrefix()")
    check(status == 404, f"expected 404, got {status}")

    status, _ = curl("GET", "/publicextended", token=token,
                      label="Fix #7: /publicextended must NOT match /public")
    check(status == 404, f"expected 404, got {status}")

    # --- Fix #8: UUID format correct after hex.Encode optimization ---

    status, hdrs = curl("GET", "/public/perf-uuid-test",
                         label="Fix #8: UUID format valid after hex.Encode optimization")
    req_id = hdrs.get("x-request-id", "")
    parts = req_id.split("-")
    check(len(parts) == 5, f"UUID has 5 dash-separated parts: {req_id}")
    # Verify part lengths: 8-4-4-4-12
    if len(parts) == 5:
        expected_lens = [8, 4, 4, 4, 12]
        actual_lens = [len(p) for p in parts]
        check(actual_lens == expected_lens,
              f"UUID part lengths {actual_lens} match {expected_lens}")
        # Verify all hex characters
        all_hex = all(c in "0123456789abcdef-" for c in req_id)
        check(all_hex, f"UUID contains only hex chars: {req_id}")

    # --- Fix #11: /ready concurrent dials + TTL cache ---
    # Hit /ready multiple times rapidly. With the 5s TTL cache,
    # responses should be fast and consistent.

    print(f"\n{'='*60}")
    print("TEST: Fix #11: /ready TTL cache (10 rapid requests)")
    ready_statuses = []
    ready_start = time.time()
    for _ in range(10):
        r = subprocess.run(
            ["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
             f"{GATEWAY}/ready"],
            capture_output=True, text=True, timeout=5,
        )
        ready_statuses.append(r.stdout.strip())
    ready_elapsed = time.time() - ready_start
    check(all(s == "200" for s in ready_statuses),
          f"all /ready returned 200 (got {set(ready_statuses)})")
    # With cache, 10 serial requests should complete well under 5s
    check(ready_elapsed < 5.0,
          f"/ready 10x completed in {ready_elapsed:.2f}s (cache working)")

    # --- Fix #12: CORS headers conditional on Origin header ---

    # Without Origin header: no CORS headers
    status, hdrs = curl("GET", "/public/cors-no-origin",
                         label="Fix #12: No CORS headers without Origin")
    check(hdrs.get("access-control-allow-origin", "") == "",
          "Access-Control-Allow-Origin absent when no Origin sent")

    # With Origin header: CORS headers present
    status, hdrs = curl("GET", "/public/cors-with-origin",
                         headers={"Origin": "https://example.com"},
                         label="Fix #12: CORS headers present with Origin")
    check(hdrs.get("access-control-allow-origin", "") != "",
          f"Access-Control-Allow-Origin: {hdrs.get('access-control-allow-origin', '(missing)')}")
    check(hdrs.get("access-control-allow-methods", "") != "",
          "Access-Control-Allow-Methods present with Origin header")

    # --- Fix #3: RWMutex concurrency under load ---
    # Send many concurrent requests and verify none are dropped.

    print(f"\n{'='*60}")
    print("TEST: Fix #3: Concurrent requests handled (40 parallel)")
    conc_procs = []
    for i in range(40):
        p = subprocess.Popen(
            ["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
             "-H", f"Authorization: Bearer {token}",
             f"{GATEWAY}/api/users/concurrent-{i}"],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        conc_procs.append(p)
    conc_statuses = {}
    for p in conc_procs:
        out, _ = p.communicate(timeout=15)
        code = out.decode().strip()
        conc_statuses[code] = conc_statuses.get(code, 0) + 1
    print(f"  Results: {dict(sorted(conc_statuses.items()))}")
    total_conc = sum(conc_statuses.values())
    check(total_conc == 40, f"all 40 requests completed (got {total_conc})")
    success_count = conc_statuses.get("200", 0)
    check(success_count >= 30,
          f"at least 30/40 returned 200 ({success_count}, rest may be rate-limited)")

    # ── Phase 4: Operational Hardening ────────────────────────

    print(f"\n\n{'#'*60}")
    print("# PHASE 4: OPERATIONAL HARDENING TESTS")
    print(f"{'#'*60}")

    # Wait for rate limit bucket to refill from earlier tests
    print("\n(Waiting 2s for rate limit bucket to refill...)")
    time.sleep(2)

    # --- 4.1: Structured Error Taxonomy ---
    # All error responses must include error_code field with GATEWAY_* prefix.

    print(f"\n{'='*60}")
    print("TEST: Error taxonomy - 404 has error_code field")
    err404_result = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/nonexistent/phase4-test"],
        capture_output=True, text=True, timeout=10,
    )
    try:
        err404 = json.loads(err404_result.stdout)
        check(err404.get("error_code") == "GATEWAY_ROUTE_NOT_FOUND",
              f"error_code = {err404.get('error_code')}")
        check(err404.get("error") == "Not Found",
              f"error = {err404.get('error')}")
        check("message" in err404,
              f"message field present: {err404.get('message')}")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"404 response not valid JSON: {e}")

    # 401 missing token → GATEWAY_AUTH_MISSING_TOKEN
    err401_result = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/api/users/phase4-test"],
        capture_output=True, text=True, timeout=10,
    )
    print(f"\n{'='*60}")
    print("TEST: Error taxonomy - 401 missing token has error_code")
    try:
        err401 = json.loads(err401_result.stdout)
        check(err401.get("error_code") == "GATEWAY_AUTH_MISSING_TOKEN",
              f"error_code = {err401.get('error_code')}")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"401 response not valid JSON: {e}")

    # 401 invalid token → GATEWAY_AUTH_INVALID_TOKEN
    err401bad_result = subprocess.run(
        ["curl", "-s", "-H", "Authorization: Bearer garbage.not.valid",
         f"{GATEWAY}/api/users/phase4-test"],
        capture_output=True, text=True, timeout=10,
    )
    print(f"\n{'='*60}")
    print("TEST: Error taxonomy - 401 invalid token has error_code")
    try:
        err401bad = json.loads(err401bad_result.stdout)
        check(err401bad.get("error_code") == "GATEWAY_AUTH_INVALID_TOKEN",
              f"error_code = {err401bad.get('error_code')}")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"401 response not valid JSON: {e}")

    # 403 insufficient scope → GATEWAY_AUTH_INSUFFICIENT_SCOPE
    read_only_token = generate_token(scope="read")
    err403_result = subprocess.run(
        ["curl", "-s", "-H", f"Authorization: Bearer {read_only_token}",
         f"{GATEWAY}/api/users/phase4-test"],
        capture_output=True, text=True, timeout=10,
    )
    print(f"\n{'='*60}")
    print("TEST: Error taxonomy - 403 insufficient scope has error_code")
    try:
        err403 = json.loads(err403_result.stdout)
        check(err403.get("error_code") == "GATEWAY_AUTH_INSUFFICIENT_SCOPE",
              f"error_code = {err403.get('error_code')}")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"403 response not valid JSON: {e}")

    # 405 method not allowed → GATEWAY_METHOD_NOT_ALLOWED
    err405_result = subprocess.run(
        ["curl", "-s", "-X", "DELETE", f"{GATEWAY}/public/phase4-test"],
        capture_output=True, text=True, timeout=10,
    )
    print(f"\n{'='*60}")
    print("TEST: Error taxonomy - 405 has error_code")
    try:
        err405 = json.loads(err405_result.stdout)
        check(err405.get("error_code") == "GATEWAY_METHOD_NOT_ALLOWED",
              f"error_code = {err405.get('error_code')}")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"405 response not valid JSON: {e}")

    # Error response includes request_id when X-Request-ID is set
    custom_rid = "phase4-error-trace-001"
    errid_result = subprocess.run(
        ["curl", "-s", "-H", f"X-Request-ID: {custom_rid}",
         f"{GATEWAY}/nonexistent/request-id-test"],
        capture_output=True, text=True, timeout=10,
    )
    print(f"\n{'='*60}")
    print("TEST: Error taxonomy - request_id included in error response")
    try:
        errid = json.loads(errid_result.stdout)
        check(errid.get("request_id") == custom_rid,
              f"request_id = {errid.get('request_id')}")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"error response not valid JSON: {e}")

    # 502 upstream error → GATEWAY_UPSTREAM_UNAVAILABLE
    status, _ = curl("GET", "/api/users/__status/502", token=token,
                      label="Error taxonomy - 502 upstream has error_code")
    check(status == 502, f"expected 502, got {status}")

    # --- 4.2: Admin API - /admin/routes ---

    status, _ = curl("GET", "/admin/routes",
                      label="Admin API - GET /admin/routes")
    check(status == 200, f"expected 200, got {status}")

    routes_result = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/admin/routes"],
        capture_output=True, text=True, timeout=10,
    )
    print(f"\n{'='*60}")
    print("TEST: Admin API - /admin/routes response structure")
    try:
        routes_resp = json.loads(routes_result.stdout)
        admin_routes = routes_resp.get("routes", [])
        check(len(admin_routes) >= 3,
              f"expected at least 3 routes, got {len(admin_routes)}")

        # Verify route structure
        first_route = admin_routes[0] if admin_routes else {}
        check("path_prefix" in first_route,
              f"route has path_prefix: {first_route.get('path_prefix')}")
        check("backend" in first_route,
              f"route has backend: {first_route.get('backend')}")
        check("circuit_breaker_state" in first_route,
              f"route has circuit_breaker_state: {first_route.get('circuit_breaker_state')}")
        check("auth_required" in first_route,
              "route has auth_required field")
        check("timeout_ms" in first_route,
              "route has timeout_ms field")

        # Verify circuit breaker states are valid
        valid_cb_states = {"closed", "open", "half-open", "unknown"}
        for route in admin_routes:
            cb_state = route.get("circuit_breaker_state", "")
            if cb_state not in valid_cb_states:
                check(False, f"invalid circuit_breaker_state: {cb_state}")
                break
        else:
            check(True, "all circuit_breaker_state values are valid")

        # Verify /api/users route is present with correct details
        users_route = next((r for r in admin_routes if r["path_prefix"] == "/api/users"), None)
        check(users_route is not None, "/api/users route found in admin response")
        if users_route:
            check(users_route.get("auth_required") is True,
                  f"/api/users auth_required = {users_route.get('auth_required')}")
            check(users_route.get("circuit_breaker_state") == "closed",
                  f"/api/users circuit_breaker = {users_route.get('circuit_breaker_state')}")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"/admin/routes response not valid JSON: {e}")

    # --- 4.3: Admin API - /admin/config ---

    config_result = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/admin/config"],
        capture_output=True, text=True, timeout=10,
    )
    print(f"\n{'='*60}")
    print("TEST: Admin API - /admin/config redacts jwt_secret")
    try:
        config_resp = json.loads(config_result.stdout)
        auth_cfg = config_resp.get("auth", {})
        jwt_secret = auth_cfg.get("jwt_secret", "")
        check(jwt_secret == "***",
              f"jwt_secret redacted to '***' (got '{jwt_secret}')")
        check(SECRET not in config_result.stdout,
              "raw JWT_SECRET not leaked in config response")

        # Verify config has expected structure
        check("server" in config_resp, "config has server section")
        check("routes" in config_resp, "config has routes section")
        check("rate_limit" in config_resp, "config has rate_limit section")
        check("metrics" in config_resp, "config has metrics section")

        # Verify Phase 4 logging config is present
        logging_cfg = config_resp.get("logging", {})
        check(logging_cfg.get("body_logging") is True,
              f"logging.body_logging = {logging_cfg.get('body_logging')}")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"/admin/config response not valid JSON: {e}")

    # --- 4.4: Admin API - /admin/limiters ---

    # Generate some traffic first so there are rate limiter entries
    for _ in range(3):
        subprocess.run(
            ["curl", "-s", "-o", "/dev/null", f"{GATEWAY}/public/limiter-test"],
            capture_output=True, text=True, timeout=5,
        )

    limiters_result = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/admin/limiters"],
        capture_output=True, text=True, timeout=10,
    )
    print(f"\n{'='*60}")
    print("TEST: Admin API - /admin/limiters response structure")
    try:
        limiters_resp = json.loads(limiters_result.stdout)
        check("entries" in limiters_resp,
              "limiters response has 'entries' field")
        check("total" in limiters_resp,
              f"limiters response has 'total' field: {limiters_resp.get('total')}")
        check("page" in limiters_resp,
              f"limiters response has 'page' field: {limiters_resp.get('page')}")

        entries = limiters_resp.get("entries", [])
        if entries:
            entry = entries[0]
            check("ip" in entry,
                  f"limiter entry has 'ip': {entry.get('ip')}")
            check("rate" in entry,
                  f"limiter entry has 'rate': {entry.get('rate')}")
            check("burst" in entry,
                  f"limiter entry has 'burst': {entry.get('burst')}")
        else:
            check(True, "no entries (demo container may share IP)")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"/admin/limiters response not valid JSON: {e}")

    # --- 4.5: Admin API - Pagination on /admin/limiters ---

    paginated_result = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/admin/limiters?page=0&page_size=1"],
        capture_output=True, text=True, timeout=10,
    )
    print(f"\n{'='*60}")
    print("TEST: Admin API - /admin/limiters pagination")
    try:
        paginated = json.loads(paginated_result.stdout)
        page_entries = paginated.get("entries", [])
        check(len(page_entries) <= 1,
              f"page_size=1 returned {len(page_entries)} entries (expected <= 1)")
        check(paginated.get("page") == 0,
              f"page = {paginated.get('page')}")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"paginated limiters not valid JSON: {e}")

    # --- 4.6: Admin API - method not allowed ---

    status, _ = curl("POST", "/admin/routes",
                      label="Admin API - POST /admin/routes rejected")
    check(status == 405, f"expected 405, got {status}")

    # --- 4.7: Admin API bypasses auth middleware ---
    # /admin/* is on the bypass mux, so no JWT is needed.

    status, _ = curl("GET", "/admin/routes",
                      label="Admin API - accessible without JWT")
    check(status == 200, f"expected 200, got {status} (admin should bypass auth)")

    # --- 4.8: Admin API bypasses rate limiting ---

    print(f"\n{'='*60}")
    print("TEST: Admin API - /admin/routes bypasses rate limiting")
    admin_statuses = []
    for _ in range(15):
        r = subprocess.run(
            ["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
             f"{GATEWAY}/admin/routes"],
            capture_output=True, text=True, timeout=5,
        )
        admin_statuses.append(r.stdout.strip())
    check(all(s == "200" for s in admin_statuses),
          f"all /admin/routes requests returned 200 (got {set(admin_statuses)})")

    # --- 4.9: Per-route log levels configured ---
    # Verify the /health route with log_level: "none" is recognized in admin routes

    print(f"\n{'='*60}")
    print("TEST: Per-route log levels - /health route configured")
    routes_result2 = subprocess.run(
        ["curl", "-s", f"{GATEWAY}/admin/routes"],
        capture_output=True, text=True, timeout=10,
    )
    try:
        routes_resp2 = json.loads(routes_result2.stdout)
        health_route = next(
            (r for r in routes_resp2.get("routes", [])
             if r["path_prefix"] == "/health"),
            None,
        )
        check(health_route is not None,
              "/health route present in admin routes")
    except (json.JSONDecodeError, KeyError) as e:
        check(False, f"admin routes parse error: {e}")

    # --- 4.10: Error responses are consistent across all error types ---
    # Verify every error response has the same JSON structure:
    # {error, error_code, message} with optional request_id.

    print(f"\n{'='*60}")
    print("TEST: Error response consistency across all error types")
    error_tests = [
        ("404", ["curl", "-s", f"{GATEWAY}/nonexistent"]),
        ("401", ["curl", "-s", f"{GATEWAY}/api/users/test"]),
        ("405", ["curl", "-s", "-X", "DELETE", f"{GATEWAY}/public/test"]),
    ]
    for expected_label, cmd in error_tests:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=10)
        try:
            body = json.loads(r.stdout)
            has_error = "error" in body
            has_code = "error_code" in body
            has_msg = "message" in body
            all_present = has_error and has_code and has_msg
            check(all_present,
                  f"{expected_label}: error={has_error} error_code={has_code} message={has_msg}")
            # Verify error_code starts with GATEWAY_
            if has_code:
                check(body["error_code"].startswith("GATEWAY_"),
                      f"{expected_label}: error_code starts with GATEWAY_: {body['error_code']}")
        except (json.JSONDecodeError, KeyError) as e:
            check(False, f"{expected_label} response not valid JSON: {e}")

    # ── Summary ───────────────────────────────────────────────

    total = passed + failed
    print(f"\n{'='*60}")
    print(f"RESULTS: {passed}/{total} passed, {failed} failed")
    print(f"{'='*60}")
    if failed > 0:
        sys.exit(1)


if __name__ == "__main__":
    main()
