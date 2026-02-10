package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromBytes_Defaults(t *testing.T) {
	yaml := []byte(`
auth:
  enabled: false
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
`)
	cfg, err := LoadFromBytes(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("expected default port 8080, got %d", cfg.Server.Port)
	}
	if cfg.RateLimit.RequestsPerSecond != 100 {
		t.Errorf("expected default rps 100, got %f", cfg.RateLimit.RequestsPerSecond)
	}
	if cfg.RateLimit.BurstSize != 50 {
		t.Errorf("expected default burst 50, got %d", cfg.RateLimit.BurstSize)
	}
	if cfg.Routes[0].TimeoutMs != 30000 {
		t.Errorf("expected default timeout 30000, got %d", cfg.Routes[0].TimeoutMs)
	}
	if cfg.Server.MaxBodyBytes != 1048576 {
		t.Errorf("expected default max_body_bytes 1048576, got %d", cfg.Server.MaxBodyBytes)
	}
}

func TestLoadFromBytes_FullConfig(t *testing.T) {
	yaml := []byte(`
server:
  port: 9090
  read_timeout: 10s
  write_timeout: 20s
  shutdown_timeout: 5s
  trusted_proxies: ["10.0.0.0/8"]
  max_body_bytes: 2097152
rate_limit:
  requests_per_second: 200
  burst_size: 100
auth:
  enabled: true
  jwt_secret: "test-secret"
  issuer: "test-issuer"
  audience: "test-audience"
  scopes: ["read"]
routes:
  - path_prefix: "/api/v1"
    backend: "http://backend:3000"
    strip_prefix: true
    methods: ["GET", "POST"]
    auth_required: true
    timeout_ms: 5000
    retry_attempts: 3
    headers:
      X-Custom: "value"
`)
	cfg, err := LoadFromBytes(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Server.Port)
	}
	if cfg.Auth.JWTSecret != "test-secret" {
		t.Errorf("expected jwt_secret 'test-secret', got %q", cfg.Auth.JWTSecret)
	}
	if len(cfg.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(cfg.Routes))
	}
	r := cfg.Routes[0]
	if r.PathPrefix != "/api/v1" {
		t.Errorf("expected path_prefix /api/v1, got %q", r.PathPrefix)
	}
	if !r.StripPrefix {
		t.Error("expected strip_prefix true")
	}
	if r.RetryAttempts != 3 {
		t.Errorf("expected retry_attempts 3, got %d", r.RetryAttempts)
	}
	if r.Headers["X-Custom"] != "value" {
		t.Errorf("expected header X-Custom=value, got %q", r.Headers["X-Custom"])
	}
	if len(cfg.Server.TrustedProxies) != 1 || cfg.Server.TrustedProxies[0] != "10.0.0.0/8" {
		t.Errorf("expected trusted_proxies [10.0.0.0/8], got %v", cfg.Server.TrustedProxies)
	}
	if cfg.Server.MaxBodyBytes != 2097152 {
		t.Errorf("expected max_body_bytes 2097152, got %d", cfg.Server.MaxBodyBytes)
	}
}

func TestLoadFromBytes_EnvVarSubstitution(t *testing.T) {
	os.Setenv("TEST_JWT_SECRET", "env-secret-value")
	defer os.Unsetenv("TEST_JWT_SECRET")

	yaml := []byte(`
auth:
  enabled: true
  jwt_secret: "${TEST_JWT_SECRET}"
  issuer: "iss"
  audience: "aud"
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
`)
	cfg, err := LoadFromBytes(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Auth.JWTSecret != "env-secret-value" {
		t.Errorf("expected env var expansion, got %q", cfg.Auth.JWTSecret)
	}
}

func TestLoadFromBytes_UnresolvedEnvVarWarning(t *testing.T) {
	os.Unsetenv("NONEXISTENT_SECRET")

	yaml := []byte(`
auth:
  enabled: true
  jwt_secret: "${NONEXISTENT_SECRET}"
  issuer: "iss"
  audience: "aud"
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
`)
	cfg, err := LoadFromBytes(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "unresolved environment variable") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected warning about unresolved environment variable")
	}
}

func TestLoadFromBytes_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "missing routes",
			yaml: `
auth:
  enabled: false
routes: []
`,
		},
		{
			name: "invalid port",
			yaml: `
server:
  port: 99999
auth:
  enabled: false
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
`,
		},
		{
			name: "missing path_prefix",
			yaml: `
auth:
  enabled: false
routes:
  - backend: "http://localhost:3000"
`,
		},
		{
			name: "missing backend",
			yaml: `
auth:
  enabled: false
routes:
  - path_prefix: "/api"
`,
		},
		{
			name: "path_prefix without leading slash",
			yaml: `
auth:
  enabled: false
routes:
  - path_prefix: "api"
    backend: "http://localhost:3000"
`,
		},
		{
			name: "duplicate path_prefix",
			yaml: `
auth:
  enabled: false
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
  - path_prefix: "/api"
    backend: "http://localhost:3001"
`,
		},
		{
			name: "auth enabled without secret",
			yaml: `
auth:
  enabled: true
  issuer: "iss"
  audience: "aud"
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
`,
		},
		{
			name: "auth enabled without issuer",
			yaml: `
auth:
  enabled: true
  jwt_secret: "secret"
  audience: "aud"
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
`,
		},
		{
			name: "auth enabled without audience",
			yaml: `
auth:
  enabled: true
  jwt_secret: "secret"
  issuer: "iss"
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
`,
		},
		{
			name: "backend with file scheme",
			yaml: `
auth:
  enabled: false
routes:
  - path_prefix: "/api"
    backend: "file:///etc/passwd"
`,
		},
		{
			name: "backend with ftp scheme",
			yaml: `
auth:
  enabled: false
routes:
  - path_prefix: "/api"
    backend: "ftp://evil.com/data"
`,
		},
		{
			name: "negative max_body_bytes",
			yaml: `
server:
  max_body_bytes: -1
auth:
  enabled: false
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFromBytes([]byte(tt.yaml))
			if err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}

func TestLoadFromBytes_BackendSchemeAccepted(t *testing.T) {
	tests := []struct {
		name    string
		backend string
	}{
		{"http", "http://localhost:3000"},
		{"https", "https://api.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := []byte(`
auth:
  enabled: false
routes:
  - path_prefix: "/api"
    backend: "` + tt.backend + `"
`)
			_, err := LoadFromBytes(yaml)
			if err != nil {
				t.Errorf("expected %s backend to be accepted, got: %v", tt.name, err)
			}
		})
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoad_FromFile(t *testing.T) {
	content := `
auth:
  enabled: false
routes:
  - path_prefix: "/test"
    backend: "http://localhost:4000"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Routes[0].PathPrefix != "/test" {
		t.Errorf("expected /test, got %q", cfg.Routes[0].PathPrefix)
	}
}

func TestRouteConfig_Timeout(t *testing.T) {
	r := RouteConfig{TimeoutMs: 5000}
	if r.Timeout().Milliseconds() != 5000 {
		t.Errorf("expected 5000ms, got %dms", r.Timeout().Milliseconds())
	}

	r2 := RouteConfig{TimeoutMs: 0}
	if r2.Timeout().Seconds() != 30 {
		t.Errorf("expected 30s default, got %v", r2.Timeout())
	}
}
