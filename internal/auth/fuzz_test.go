package auth

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dskow/gateway-core/internal/config"
)

func FuzzAuthMiddleware(f *testing.F) {
	// Seed with various Authorization header formats
	f.Add("Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U")
	f.Add("Bearer ")
	f.Add("Bearer not.a.jwt")
	f.Add("")
	f.Add("Basic dXNlcjpwYXNz")
	f.Add("Bearer eyJ.eyJ.abc")
	f.Add("bearer token")
	f.Add("BEARER token")

	cfg := config.AuthConfig{
		Enabled:   true,
		JWTSecret: "test-secret-for-fuzz-testing-32ch",
		Issuer:    "test-issuer",
		Audience:  "test-audience",
		Scopes:    []string{"read"},
	}
	logger := slog.New(slog.NewTextHandler(discard{}, nil))

	handler := Middleware(cfg, func(string) bool { return true }, logger)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	f.Fuzz(func(t *testing.T, authHeader string) {
		req := httptest.NewRequest("GET", "/api/test", nil)
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		rec := httptest.NewRecorder()

		// Must never panic.
		handler.ServeHTTP(rec, req)

		// Status must be one of: 200 (valid), 401 (invalid/missing), 403 (scope).
		switch rec.Code {
		case http.StatusOK, http.StatusUnauthorized, http.StatusForbidden:
			// expected
		default:
			t.Errorf("unexpected status %d for Authorization header %q", rec.Code, authHeader)
		}
	})
}

// discard is an io.Writer that discards all writes (avoids noisy fuzz output).
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
