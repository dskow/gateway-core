package apierror

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteJSON_BasicFields(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/test", nil)

	WriteJSON(w, r, http.StatusNotFound, RouteNotFound, "no matching route")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != "Not Found" {
		t.Errorf("error = %q, want %q", resp.Error, "Not Found")
	}
	if resp.ErrorCode != "GATEWAY_ROUTE_NOT_FOUND" {
		t.Errorf("error_code = %q, want %q", resp.ErrorCode, "GATEWAY_ROUTE_NOT_FOUND")
	}
	if resp.Message != "no matching route" {
		t.Errorf("message = %q, want %q", resp.Message, "no matching route")
	}
}

func TestWriteJSON_IncludesRequestID(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	r.Header.Set("X-Request-ID", "test-req-123")

	WriteJSON(w, r, http.StatusUnauthorized, AuthMissingToken, "missing or malformed Authorization header")

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.RequestID != "test-req-123" {
		t.Errorf("request_id = %q, want %q", resp.RequestID, "test-req-123")
	}
	if resp.ErrorCode != "GATEWAY_AUTH_MISSING_TOKEN" {
		t.Errorf("error_code = %q, want %q", resp.ErrorCode, "GATEWAY_AUTH_MISSING_TOKEN")
	}
}

func TestWriteJSON_OmitsEmptyRequestID(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	// No X-Request-ID header set

	WriteJSON(w, r, http.StatusTooManyRequests, RateLimitExceeded, "rate limit exceeded, retry later")

	// The pre-serialized path should not include request_id at all.
	var raw map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, exists := raw["request_id"]; exists {
		t.Error("request_id should be omitted when empty")
	}
}

func TestWriteJSON_NilRequest(t *testing.T) {
	w := httptest.NewRecorder()

	WriteJSON(w, nil, http.StatusInternalServerError, InternalError, "an unexpected error occurred")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ErrorCode != "GATEWAY_INTERNAL_ERROR" {
		t.Errorf("error_code = %q, want %q", resp.ErrorCode, "GATEWAY_INTERNAL_ERROR")
	}
}

func TestWriteJSON_NonPreserializedPath(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	r.Header.Set("X-Request-ID", "custom-id")

	// Custom message won't match any pre-serialized body.
	WriteJSON(w, r, http.StatusForbidden, AuthInsufficientScope, "missing required scope: admin")

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != "Forbidden" {
		t.Errorf("error = %q, want %q", resp.Error, "Forbidden")
	}
	if resp.ErrorCode != "GATEWAY_AUTH_INSUFFICIENT_SCOPE" {
		t.Errorf("error_code = %q, want %q", resp.ErrorCode, "GATEWAY_AUTH_INSUFFICIENT_SCOPE")
	}
	if resp.Message != "missing required scope: admin" {
		t.Errorf("message = %q, want %q", resp.Message, "missing required scope: admin")
	}
	if resp.RequestID != "custom-id" {
		t.Errorf("request_id = %q, want %q", resp.RequestID, "custom-id")
	}
}

func TestAllErrorCodes(t *testing.T) {
	// Verify all error codes have the GATEWAY_ prefix.
	codes := []ErrorCode{
		RouteNotFound, MethodNotAllowed, UpstreamUnavailable,
		CircuitOpen, RequestCancelled, AuthMissingToken,
		AuthInvalidToken, AuthInsufficientScope, RateLimitExceeded,
		InternalError, BodyTooLarge, DeadlineExceeded,
	}
	for _, code := range codes {
		if len(code) < 8 || code[:8] != "GATEWAY_" {
			t.Errorf("code %q does not have GATEWAY_ prefix", code)
		}
	}
	if len(codes) != 12 {
		t.Errorf("expected 12 error codes, got %d", len(codes))
	}
}
