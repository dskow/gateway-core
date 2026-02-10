package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"log/slog"

	"github.com/dskow/go-api-gateway/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret-key-for-hmac-256"

func makeToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := token.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"sub":   "user-123",
		"iss":   "test-issuer",
		"aud":   "test-audience",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"scope": "read write",
	}
}

func testAuthConfig() config.AuthConfig {
	return config.AuthConfig{
		Enabled:   true,
		JWTSecret: testSecret,
		Issuer:    "test-issuer",
		Audience:  "test-audience",
		Scopes:    []string{"read", "write"},
	}
}

func TestMiddleware_ValidToken(t *testing.T) {
	cfg := testAuthConfig()
	logger := slog.Default()

	token := makeToken(t, validClaims())

	var capturedClaims *Claims
	handler := Middleware(cfg, func(string) bool { return true }, logger)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedClaims = r.Context().Value(ClaimsKey).(*Claims)
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if capturedClaims == nil {
		t.Fatal("expected claims in context")
	}
	if capturedClaims.Subject != "user-123" {
		t.Errorf("expected sub user-123, got %q", capturedClaims.Subject)
	}
	if len(capturedClaims.Scopes) != 2 {
		t.Errorf("expected 2 scopes, got %d", len(capturedClaims.Scopes))
	}
}

func TestMiddleware_ExpiredToken(t *testing.T) {
	cfg := testAuthConfig()
	logger := slog.Default()

	claims := validClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	token := makeToken(t, claims)

	handler := Middleware(cfg, func(string) bool { return true }, logger)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_WrongAudience(t *testing.T) {
	cfg := testAuthConfig()
	logger := slog.Default()

	claims := validClaims()
	claims["aud"] = "wrong-audience"
	token := makeToken(t, claims)

	handler := Middleware(cfg, func(string) bool { return true }, logger)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_WrongIssuer(t *testing.T) {
	cfg := testAuthConfig()
	logger := slog.Default()

	claims := validClaims()
	claims["iss"] = "wrong-issuer"
	token := makeToken(t, claims)

	handler := Middleware(cfg, func(string) bool { return true }, logger)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_MissingScopes(t *testing.T) {
	cfg := testAuthConfig()
	logger := slog.Default()

	claims := validClaims()
	claims["scope"] = "read" // missing "write"
	token := makeToken(t, claims)

	handler := Middleware(cfg, func(string) bool { return true }, logger)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestMiddleware_MalformedToken(t *testing.T) {
	cfg := testAuthConfig()
	logger := slog.Default()

	handler := Middleware(cfg, func(string) bool { return true }, logger)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	tests := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"no bearer prefix", "Token abc123"},
		{"empty bearer", "Bearer "},
		{"garbage token", "Bearer not.a.valid.jwt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/test", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d", rec.Code)
			}
		})
	}
}

func TestMiddleware_AuthNotRequired(t *testing.T) {
	cfg := testAuthConfig()
	logger := slog.Default()

	handler := Middleware(cfg, func(string) bool { return false }, logger)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/public", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddleware_AuthDisabled(t *testing.T) {
	cfg := testAuthConfig()
	cfg.Enabled = false
	logger := slog.Default()

	handler := Middleware(cfg, func(string) bool { return true }, logger)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddleware_WrongSigningMethod(t *testing.T) {
	cfg := testAuthConfig()
	logger := slog.Default()

	// Create a token signed with HS384 instead of HS256
	claims := validClaims()
	token := jwt.NewWithClaims(jwt.SigningMethodHS384, claims)
	tokenStr, _ := token.SignedString([]byte(testSecret))

	handler := Middleware(cfg, func(string) bool { return true }, logger)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}
