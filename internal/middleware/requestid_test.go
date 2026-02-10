package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestID_GeneratesUUID(t *testing.T) {
	var capturedID string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = GetRequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedID == "" {
		t.Fatal("expected request ID to be generated")
	}

	// UUID v4 format: 8-4-4-4-12 hex chars
	parts := strings.Split(capturedID, "-")
	if len(parts) != 5 {
		t.Errorf("expected UUID format (5 parts), got %q", capturedID)
	}

	// Verify response header
	respID := rec.Header().Get("X-Request-ID")
	if respID != capturedID {
		t.Errorf("response header %q != context ID %q", respID, capturedID)
	}
}

func TestRequestID_PreservesExisting(t *testing.T) {
	existingID := "my-custom-request-id"

	var capturedID string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = GetRequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-ID", existingID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedID != existingID {
		t.Errorf("expected preserved ID %q, got %q", existingID, capturedID)
	}

	respID := rec.Header().Get("X-Request-ID")
	if respID != existingID {
		t.Errorf("response header %q != existing ID %q", respID, existingID)
	}
}

func TestRequestID_SetsRequestHeader(t *testing.T) {
	var headerID string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerID = r.Header.Get("X-Request-ID")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if headerID == "" {
		t.Fatal("expected X-Request-ID to be set on request header")
	}

	contextID := rec.Header().Get("X-Request-ID")
	if headerID != contextID {
		t.Errorf("request header %q != response header %q", headerID, contextID)
	}
}

func TestRequestID_UniquePerRequest(t *testing.T) {
	ids := make(map[string]bool)

	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 100; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		id := rec.Header().Get("X-Request-ID")
		if ids[id] {
			t.Fatalf("duplicate request ID generated: %s", id)
		}
		ids[id] = true
	}
}

func TestGetRequestID_EmptyContext(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	id := GetRequestID(req.Context())
	if id != "" {
		t.Errorf("expected empty string for context without request ID, got %q", id)
	}
}
