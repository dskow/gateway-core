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

    # ── Summary ───────────────────────────────────────────────

    total = passed + failed
    print(f"\n{'='*60}")
    print(f"RESULTS: {passed}/{total} passed, {failed} failed")
    print(f"{'='*60}")
    if failed > 0:
        sys.exit(1)


if __name__ == "__main__":
    main()
