// Package config provides YAML configuration loading with validation and
// environment variable substitution for the API gateway.
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level gateway configuration.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	Auth      AuthConfig      `yaml:"auth"`
	Routes    []RouteConfig   `yaml:"routes"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port            int           `yaml:"port"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	TrustedProxies  []string      `yaml:"trusted_proxies"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes"`
}

// RateLimitConfig holds the global rate limiter settings.
type RateLimitConfig struct {
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	BurstSize         int     `yaml:"burst_size"`
}

// AuthConfig holds JWT/OAuth2 authentication settings.
type AuthConfig struct {
	Enabled   bool     `yaml:"enabled"`
	JWTSecret string   `yaml:"jwt_secret"`
	Issuer    string   `yaml:"issuer"`
	Audience  string   `yaml:"audience"`
	Scopes    []string `yaml:"scopes"`
}

// RouteConfig defines a single proxy route.
type RouteConfig struct {
	PathPrefix    string            `yaml:"path_prefix"`
	Backend       string            `yaml:"backend"`
	StripPrefix   bool              `yaml:"strip_prefix"`
	Methods       []string          `yaml:"methods"`
	AuthRequired  bool              `yaml:"auth_required"`
	TimeoutMs     int               `yaml:"timeout_ms"`
	RetryAttempts int               `yaml:"retry_attempts"`
	Headers       map[string]string `yaml:"headers"`
	RateOverride  *RateLimitConfig  `yaml:"rate_override"`
}

// Timeout returns the route timeout as a time.Duration.
func (r RouteConfig) Timeout() time.Duration {
	if r.TimeoutMs <= 0 {
		return 30 * time.Second
	}
	return time.Duration(r.TimeoutMs) * time.Millisecond
}

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// expandEnvVars replaces ${VAR_NAME} patterns in s with the corresponding
// environment variable value.
func expandEnvVars(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(match string) string {
		key := match[2 : len(match)-1]
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return match
	})
}

// Warnings collects non-fatal config issues.
var Warnings []string

// Load reads and parses a YAML configuration file, applies environment
// variable substitution, sets defaults, and validates the result.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	expanded := expandEnvVars(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	Warnings = collectWarnings(&cfg)

	return &cfg, nil
}

// LoadFromBytes parses configuration from raw YAML bytes. Useful for testing.
func LoadFromBytes(data []byte) (*Config, error) {
	expanded := expandEnvVars(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	Warnings = collectWarnings(&cfg)

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Server.ReadTimeout == 0 {
		cfg.Server.ReadTimeout = 15 * time.Second
	}
	if cfg.Server.WriteTimeout == 0 {
		cfg.Server.WriteTimeout = 15 * time.Second
	}
	if cfg.Server.ShutdownTimeout == 0 {
		cfg.Server.ShutdownTimeout = 10 * time.Second
	}
	if cfg.Server.MaxBodyBytes == 0 {
		cfg.Server.MaxBodyBytes = 1048576 // 1 MB
	}
	if cfg.RateLimit.RequestsPerSecond == 0 {
		cfg.RateLimit.RequestsPerSecond = 100
	}
	if cfg.RateLimit.BurstSize == 0 {
		cfg.RateLimit.BurstSize = 50
	}
	for i := range cfg.Routes {
		if cfg.Routes[i].TimeoutMs == 0 {
			cfg.Routes[i].TimeoutMs = 30000
		}
	}
}

func validate(cfg *Config) error {
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", cfg.Server.Port)
	}
	if cfg.Server.MaxBodyBytes < 0 {
		return fmt.Errorf("server.max_body_bytes must be positive")
	}
	if cfg.RateLimit.RequestsPerSecond <= 0 {
		return fmt.Errorf("rate_limit.requests_per_second must be positive")
	}
	if cfg.RateLimit.BurstSize <= 0 {
		return fmt.Errorf("rate_limit.burst_size must be positive")
	}
	if cfg.Auth.Enabled {
		if cfg.Auth.JWTSecret == "" {
			return fmt.Errorf("auth.jwt_secret is required when auth is enabled")
		}
		if cfg.Auth.Issuer == "" {
			return fmt.Errorf("auth.issuer is required when auth is enabled")
		}
		if cfg.Auth.Audience == "" {
			return fmt.Errorf("auth.audience is required when auth is enabled")
		}
	}

	if len(cfg.Routes) == 0 {
		return fmt.Errorf("at least one route must be configured")
	}

	seen := make(map[string]bool)
	for i, r := range cfg.Routes {
		if r.PathPrefix == "" {
			return fmt.Errorf("routes[%d].path_prefix is required", i)
		}
		if !strings.HasPrefix(r.PathPrefix, "/") {
			return fmt.Errorf("routes[%d].path_prefix must start with /", i)
		}
		if r.Backend == "" {
			return fmt.Errorf("routes[%d].backend is required", i)
		}
		u, err := url.Parse(r.Backend)
		if err != nil {
			return fmt.Errorf("routes[%d].backend: invalid URL: %w", i, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("routes[%d].backend: scheme must be http or https, got %q", i, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("routes[%d].backend: host is required", i)
		}
		if seen[r.PathPrefix] {
			return fmt.Errorf("duplicate route path_prefix: %s", r.PathPrefix)
		}
		seen[r.PathPrefix] = true
	}

	return nil
}

func collectWarnings(cfg *Config) []string {
	var warnings []string
	if cfg.Auth.Enabled && strings.Contains(cfg.Auth.JWTSecret, "${") {
		warnings = append(warnings, "auth.jwt_secret contains unresolved environment variable")
	}
	return warnings
}
