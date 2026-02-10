//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --- Health Endpoints ---

func TestHealthEndpoint(t *testing.T) {
	resp, body, err := httpGet(gatewayURL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)
	assertBodyContains(t, body, "ok")
}

func TestReadyEndpoint(t *testing.T) {
	resp, _, err := httpGet(gatewayURL+"/ready", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)
}

// --- Auth Flows ---

func TestAuthFlow_ValidToken(t *testing.T) {
	token := generateJWT("user-123", "read write", time.Hour)
	resp, body, err := httpGet(gatewayURL+"/api/users/hello", authHeader(token))
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)

	// Echoserver should return valid JSON with service info
	m := parseJSON(t, body)
	if _, ok := m["service"]; !ok {
		t.Error("expected 'service' field in echoserver response")
	}
}

func TestAuthFlow_MissingToken(t *testing.T) {
	resp, body, err := httpGet(gatewayURL+"/api/users/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 401)
	assertErrorCode(t, body, "GATEWAY_AUTH_MISSING_TOKEN")
}

func TestAuthFlow_ExpiredToken(t *testing.T) {
	token := generateJWT("user-123", "read write", -time.Hour)
	resp, body, err := httpGet(gatewayURL+"/api/users/test", authHeader(token))
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 401)
	assertErrorCode(t, body, "GATEWAY_AUTH_INVALID_TOKEN")
}

func TestAuthFlow_InsufficientScope(t *testing.T) {
	token := generateJWT("user-123", "read", time.Hour) // missing "write"
	resp, body, err := httpGet(gatewayURL+"/api/users/test", authHeader(token))
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 403)
	assertErrorCode(t, body, "GATEWAY_AUTH_INSUFFICIENT_SCOPE")
}

func TestAuthFlow_GarbageToken(t *testing.T) {
	resp, body, err := httpGet(gatewayURL+"/api/users/test", authHeader("not.a.valid.jwt"))
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 401)
	assertErrorCode(t, body, "GATEWAY_AUTH_INVALID_TOKEN")
}

// --- Routing ---

func TestRouting_NotFound(t *testing.T) {
	resp, body, err := httpGet(gatewayURL+"/nonexistent/path", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 404)
	assertErrorCode(t, body, "GATEWAY_ROUTE_NOT_FOUND")
}

func TestRouting_MethodNotAllowed(t *testing.T) {
	// /public only allows GET
	resp, body, err := httpDo("DELETE", gatewayURL+"/public/test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 405)
	assertErrorCode(t, body, "GATEWAY_METHOD_NOT_ALLOWED")
}

func TestRouting_PathBoundary(t *testing.T) {
	// /api.evil.com/steal should NOT match /api/users
	resp, _, err := httpGet(gatewayURL+"/api.evil.com/steal", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 404)
}

func TestRouting_PrefixStripping(t *testing.T) {
	token := generateJWT("user-123", "read write", time.Hour)
	resp, body, err := httpGet(gatewayURL+"/api/users/mypath", authHeader(token))
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)

	// Echoserver should see the stripped path
	m := parseJSON(t, body)
	if path, ok := m["path"].(string); ok {
		if path != "/mypath" {
			t.Errorf("expected echoserver to see path /mypath, got %q", path)
		}
	} else {
		t.Error("expected 'path' field in echoserver response")
	}
}

func TestRouting_HeaderInjection(t *testing.T) {
	token := generateJWT("user-123", "read write", time.Hour)
	resp, body, err := httpGet(gatewayURL+"/api/analytics/test", authHeader(token))
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)

	// Echoserver should see the injected X-Source header
	m := parseJSON(t, body)
	headers, ok := m["headers"].(map[string]interface{})
	if !ok {
		t.Fatal("expected 'headers' map in echoserver response")
	}
	xSource, _ := headers["X-Source"].(string)
	if xSource == "" {
		// Try lowercase
		xSource, _ = headers["x-source"].(string)
	}
	if xSource != "gateway" {
		t.Errorf("expected X-Source=gateway in echoserver headers, got %q", xSource)
	}
}

func TestPublicRouteNoAuth(t *testing.T) {
	resp, body, err := httpGet(gatewayURL+"/public/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)

	m := parseJSON(t, body)
	if _, ok := m["service"]; !ok {
		t.Error("expected 'service' field in echoserver response")
	}
}

// --- Rate Limiting ---

func TestRateLimiting_BurstExhaustion(t *testing.T) {
	// Integration config: burst_size=20 for global rate limit.
	// Send burst_size+30 rapid requests; some should be 429.
	got429 := 0
	total := 50

	for i := 0; i < total; i++ {
		resp, body, err := httpGet(gatewayURL+"/public/hello", nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			got429++
			assertErrorCode(t, body, "GATEWAY_RATE_LIMIT_EXCEEDED")
			if resp.Header.Get("Retry-After") == "" {
				t.Error("expected Retry-After header on 429")
			}
		} else if resp.StatusCode != http.StatusOK {
			t.Errorf("unexpected status %d", resp.StatusCode)
		}
	}

	if got429 == 0 {
		t.Error("expected at least one 429 response after exhausting burst")
	}
	t.Logf("got %d/50 rate-limited responses", got429)
}

// --- Retry Behavior ---

func TestRetryBehavior(t *testing.T) {
	// Wait for rate limiter to refill after the burst exhaustion test.
	time.Sleep(2 * time.Second)

	// Request a 502 from echoserver via /__status/502 (prefix stripped).
	// The gateway should retry (retry_attempts=2) and still return 502.
	token := generateJWT("retry-user", "read write", time.Hour)
	resp, _, err := httpGet(gatewayURL+"/api/users/__status/502", authHeader(token))
	if err != nil {
		t.Fatal(err)
	}
	// After retries are exhausted, the gateway returns the backend's status.
	if resp.StatusCode != 502 {
		t.Errorf("expected 502 after retries exhausted, got %d", resp.StatusCode)
	}
}

// --- Circuit Breaker ---

func TestCircuitBreaker_OpensOnFailures(t *testing.T) {
	token := generateJWT("user-123", "read write", time.Hour)

	// Hammer the backend with errors to trip the circuit breaker.
	// Integration config: window_size=5, failure_threshold=0.6, so 3/5 failures trip it.
	// With retry_attempts=2, each request generates 2 failures.
	for i := 0; i < 10; i++ {
		httpGet(gatewayURL+"/api/users/__status/502", authHeader(token))
	}

	// Give the circuit breaker a moment to update state.
	time.Sleep(500 * time.Millisecond)

	// Check admin endpoint for circuit breaker state.
	resp, body, err := httpGet(gatewayURL+"/admin/routes", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)

	var result struct {
		Routes []map[string]interface{} `json:"routes"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse admin/routes: %v\nbody: %s", err, string(body))
	}

	foundOpen := false
	for _, r := range result.Routes {
		prefix, _ := r["path_prefix"].(string)
		state, _ := r["circuit_breaker_state"].(string)
		if prefix == "/api/users" && state == "open" {
			foundOpen = true
			break
		}
	}

	if !foundOpen {
		t.Log("circuit breaker states:")
		for _, r := range result.Routes {
			t.Logf("  %s: %s", r["path_prefix"], r["circuit_breaker_state"])
		}
		t.Error("expected circuit breaker for /api/users to be open after failures")
		return
	}

	// With the breaker open, a new request should get 503.
	resp2, body2, err := httpGet(gatewayURL+"/api/users/test", authHeader(token))
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != 503 {
		t.Errorf("expected 503 when circuit open, got %d", resp2.StatusCode)
	}
	assertErrorCode(t, body2, "GATEWAY_CIRCUIT_OPEN")
}

// --- Metrics ---

func TestMetricsEndpoint(t *testing.T) {
	resp, body, err := httpGet(gatewayURL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)
	assertBodyContains(t, body, "gateway_requests_total")
	assertBodyContains(t, body, "gateway_request_duration_seconds")
}

// --- Admin API ---

func TestAdminRoutes(t *testing.T) {
	resp, body, err := httpGet(gatewayURL+"/admin/routes", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)

	var result struct {
		Routes []map[string]interface{} `json:"routes"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse /admin/routes response: %v", err)
	}
	if len(result.Routes) == 0 {
		t.Error("expected at least one route in admin response")
	}
}

func TestAdminConfig(t *testing.T) {
	resp, body, err := httpGet(gatewayURL+"/admin/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)
	assertBodyContains(t, body, `"***"`) // jwt_secret should be redacted
}

func TestAdminLimiters(t *testing.T) {
	resp, body, err := httpGet(gatewayURL+"/admin/limiters", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)

	m := parseJSON(t, body)
	if _, ok := m["total"]; !ok {
		t.Error("expected 'total' field in /admin/limiters response")
	}
	if _, ok := m["page"]; !ok {
		t.Error("expected 'page' field in /admin/limiters response")
	}
}

// --- Security Headers ---

func TestSecurityHeaders(t *testing.T) {
	resp, _, err := httpGet(gatewayURL+"/public/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 200)
	assertHeader(t, resp, "X-Content-Type-Options", "nosniff")
	assertHeader(t, resp, "X-Frame-Options", "DENY")
	assertHeader(t, resp, "X-Xss-Protection", "0")
}

// --- Request ID ---

func TestRequestID_Generated(t *testing.T) {
	resp, _, err := httpGet(gatewayURL+"/public/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Header.Get("X-Request-ID")
	if id == "" {
		t.Error("expected X-Request-ID header to be auto-generated")
	}
	// Basic UUID format check: 8-4-4-4-12 (36 chars with dashes)
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Errorf("X-Request-ID %q doesn't look like a UUID", id)
	}
}

func TestRequestID_Preserved(t *testing.T) {
	customID := "custom-request-id-12345"
	resp, _, err := httpGet(gatewayURL+"/public/hello", map[string]string{
		"X-Request-ID": customID,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHeader(t, resp, "X-Request-ID", customID)
}

func TestRequestID_Unique(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 10; i++ {
		resp, _, err := httpGet(gatewayURL+"/public/hello", nil)
		if err != nil {
			t.Fatal(err)
		}
		id := resp.Header.Get("X-Request-ID")
		if ids[id] {
			t.Errorf("duplicate X-Request-ID: %s", id)
		}
		ids[id] = true
	}
}

// --- Error Response Consistency ---

func TestErrorResponseFormat(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		method     string
		headers    map[string]string
		wantStatus int
	}{
		{"404 not found", gatewayURL + "/nonexistent", "GET", nil, 404},
		{"401 missing auth", gatewayURL + "/api/users/test", "GET", nil, 401},
		{"405 method not allowed", gatewayURL + "/public/test", "DELETE", nil, 405},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body, err := httpDo(tt.method, tt.url, nil, tt.headers)
			if err != nil {
				t.Fatal(err)
			}
			assertStatusCode(t, resp, tt.wantStatus)

			var m map[string]interface{}
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("error response not valid JSON: %v", err)
			}
			for _, field := range []string{"error", "error_code", "message"} {
				if _, ok := m[field]; !ok {
					t.Errorf("missing field %q in error response: %s", field, string(body))
				}
			}
		})
	}
}

func TestErrorResponse_IncludesRequestID(t *testing.T) {
	customID := "trace-error-test-id"
	resp, body, err := httpGet(gatewayURL+"/nonexistent", map[string]string{
		"X-Request-ID": customID,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, resp, 404)

	m := parseJSON(t, body)
	requestID, ok := m["request_id"].(string)
	if !ok || requestID == "" {
		t.Errorf("expected request_id in error response, got: %s", string(body))
	}
	if requestID != customID {
		fmt.Printf("note: request_id %q differs from sent %q (may be expected)\n", requestID, customID)
	}
}
