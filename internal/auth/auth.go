// Package auth provides JWT/OAuth2 Bearer token validation middleware
// for the API gateway.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dskow/go-api-gateway/internal/config"
	"github.com/dskow/go-api-gateway/internal/metrics"
	"github.com/golang-jwt/jwt/v5"
)

type contextKey string

// ClaimsKey is the context key used to store validated JWT claims.
const ClaimsKey contextKey = "jwt_claims"

// Claims represents the validated JWT claims injected into the request context.
type Claims struct {
	Subject  string   `json:"sub"`
	Issuer   string   `json:"iss"`
	Audience string   `json:"aud"`
	Scopes   []string `json:"scopes"`
}

// Middleware returns an HTTP middleware that validates JWT Bearer tokens.
// Routes that do not require authentication are passed through.
func Middleware(cfg config.AuthConfig, routeRequiresAuth func(path string) bool, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.Enabled || !routeRequiresAuth(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			tokenStr, ok := extractBearerToken(r)
			if !ok {
				metrics.AuthFailures.WithLabelValues("missing_token").Inc()
				writeAuthError(w, http.StatusUnauthorized, "missing or malformed Authorization header")
				return
			}

			claims, err := validateToken(tokenStr, cfg)
			if err != nil {
				logger.Warn("auth failure", "error", err, "path", r.URL.Path)
				if isScopeError(err) {
					metrics.AuthFailures.WithLabelValues("insufficient_scope").Inc()
					writeAuthError(w, http.StatusForbidden, err.Error())
				} else {
					metrics.AuthFailures.WithLabelValues("invalid_token").Inc()
					writeAuthError(w, http.StatusUnauthorized, err.Error())
				}
				return
			}

			ctx := context.WithValue(r.Context(), ClaimsKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractBearerToken(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", false
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}

func validateToken(tokenStr string, cfg config.AuthConfig) (*Claims, error) {
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(cfg.JWTSecret), nil
	},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithAudience(cfg.Audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	mapClaims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	claims := &Claims{}

	if sub, ok := mapClaims["sub"].(string); ok {
		claims.Subject = sub
	}
	if iss, ok := mapClaims["iss"].(string); ok {
		claims.Issuer = iss
	}

	// Handle audience — can be string or []interface{}
	switch aud := mapClaims["aud"].(type) {
	case string:
		claims.Audience = aud
	case []interface{}:
		if len(aud) > 0 {
			if s, ok := aud[0].(string); ok {
				claims.Audience = s
			}
		}
	}

	// Parse scopes — space-separated string per OAuth2 spec
	if scopeStr, ok := mapClaims["scope"].(string); ok {
		claims.Scopes = strings.Fields(scopeStr)
	}

	// Validate required scopes
	if len(cfg.Scopes) > 0 {
		scopeSet := make(map[string]bool, len(claims.Scopes))
		for _, s := range claims.Scopes {
			scopeSet[s] = true
		}
		for _, required := range cfg.Scopes {
			if !scopeSet[required] {
				return nil, &ScopeError{MissingScope: required}
			}
		}
	}

	return claims, nil
}

// ScopeError indicates the token is valid but lacks required scopes.
type ScopeError struct {
	MissingScope string
}

func (e *ScopeError) Error() string {
	return fmt.Sprintf("missing required scope: %s", e.MissingScope)
}

func isScopeError(err error) bool {
	var se *ScopeError
	return errors.As(err, &se)
}

// Pre-serialized auth error body for the most common rejection (missing token).
var errBodyMissingAuth = []byte(`{"error":"Unauthorized","message":"missing or malformed Authorization header"}` + "\n")

func writeAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if status == http.StatusUnauthorized && message == "missing or malformed Authorization header" {
		w.Write(errBodyMissingAuth)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error":   http.StatusText(status),
		"message": message,
	})
}
