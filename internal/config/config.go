// Package config provides YAML configuration loading with validation and
// environment variable substitution for the API gateway.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level gateway configuration.
type Config struct {
	Server         ServerConfig         `yaml:"server" json:"server"`
	Metrics        MetricsConfig        `yaml:"metrics" json:"metrics"`
	Logging        LoggingConfig        `yaml:"logging" json:"logging"`
	RateLimit      RateLimitConfig      `yaml:"rate_limit" json:"rate_limit"`
	Auth           AuthConfig           `yaml:"auth" json:"auth"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker" json:"circuit_breaker"`
	Admin          AdminConfig          `yaml:"admin" json:"admin"`
	Routes         []RouteConfig        `yaml:"routes" json:"routes"`

	// Warnings holds non-fatal config issues detected during loading.
	// Stored on the Config itself (not a package-level var) so it is
	// safe to call Load concurrently from the hot-reload goroutine.
	Warnings []string `yaml:"-" json:"-"`
}

// MetricsConfig holds Prometheus metrics endpoint settings.
// Enabled defaults to true; set to false to disable metrics.
type MetricsConfig struct {
	Enabled *bool  `yaml:"enabled" json:"enabled"`
	Path    string `yaml:"path" json:"path"`
}

// IsEnabled returns whether metrics are enabled (defaults to true).
func (m MetricsConfig) IsEnabled() bool {
	if m.Enabled == nil {
		return true
	}
	return *m.Enabled
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port            int           `yaml:"port" json:"port"`
	ReadTimeout     time.Duration `yaml:"read_timeout" json:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout" json:"write_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" json:"shutdown_timeout"`
	TrustedProxies  []string      `yaml:"trusted_proxies" json:"trusted_proxies"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes" json:"max_body_bytes"`
	GlobalTimeoutMs int           `yaml:"global_timeout_ms" json:"global_timeout_ms"`
	TLS             TLSConfig     `yaml:"tls" json:"tls"`
}

// TLSConfig holds TLS termination settings.
type TLSConfig struct {
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	CertFile   string `yaml:"cert_file" json:"cert_file"`
	KeyFile    string `yaml:"key_file" json:"key_file"`
	MinVersion string `yaml:"min_version" json:"min_version"` // "1.2" or "1.3"; default: "1.2"
}

// LoggingConfig holds access log output and debug settings.
type LoggingConfig struct {
	Output          string `yaml:"output" json:"output"`                          // "stdout", "stderr", or file path; default: "stdout"
	MaxSizeMB       int    `yaml:"max_size_mb" json:"max_size_mb"`               // max log file size before rotation; default: 100
	MaxBackups      int    `yaml:"max_backups" json:"max_backups"`               // number of rotated files to keep; default: 3
	MaxAgeDays      int    `yaml:"max_age_days" json:"max_age_days"`             // max days to retain rotated files; default: 30
	BodyLogging     bool   `yaml:"body_logging" json:"body_logging"`             // log request/response bodies; default: false
	MaxBodyLogBytes int    `yaml:"max_body_log_bytes" json:"max_body_log_bytes"` // max bytes of body to log; default: 4096
}

// AdminConfig holds admin API settings.
type AdminConfig struct {
	Enabled     bool     `yaml:"enabled" json:"enabled"`           // default: false
	IPAllowlist []string `yaml:"ip_allowlist" json:"ip_allowlist"` // CIDR notation
}

// GlobalTimeout returns the global request deadline as a time.Duration.
// Returns 0 (disabled) when GlobalTimeoutMs is not set.
func (s ServerConfig) GlobalTimeout() time.Duration {
	if s.GlobalTimeoutMs <= 0 {
		return 0
	}
	return time.Duration(s.GlobalTimeoutMs) * time.Millisecond
}

// RateLimitConfig holds the global rate limiter settings.
type RateLimitConfig struct {
	RequestsPerSecond float64 `yaml:"requests_per_second" json:"requests_per_second"`
	BurstSize         int     `yaml:"burst_size" json:"burst_size"`
}

// AuthConfig holds JWT/OAuth2 authentication settings.
type AuthConfig struct {
	Enabled   bool     `yaml:"enabled" json:"enabled"`
	JWTSecret string   `yaml:"jwt_secret" json:"jwt_secret"`
	Issuer    string   `yaml:"issuer" json:"issuer"`
	Audience  string   `yaml:"audience" json:"audience"`
	Scopes    []string `yaml:"scopes" json:"scopes"`
}

// RouteConfig defines a single proxy route.
type RouteConfig struct {
	PathPrefix     string               `yaml:"path_prefix" json:"path_prefix"`
	Backend        string               `yaml:"backend" json:"backend"`
	StripPrefix    bool                 `yaml:"strip_prefix" json:"strip_prefix"`
	Methods        []string             `yaml:"methods" json:"methods"`
	AuthRequired   bool                 `yaml:"auth_required" json:"auth_required"`
	TimeoutMs      int                  `yaml:"timeout_ms" json:"timeout_ms"`
	RetryAttempts  int                  `yaml:"retry_attempts" json:"retry_attempts"`
	Headers        map[string]string    `yaml:"headers" json:"headers,omitempty"`
	RateOverride   *RateLimitConfig     `yaml:"rate_override" json:"rate_override,omitempty"`
	ConnectionPool *ConnectionPoolConfig `yaml:"connection_pool" json:"connection_pool,omitempty"`
	FallbackStatus int                  `yaml:"fallback_status" json:"fallback_status"`
	FallbackBody   string               `yaml:"fallback_body" json:"fallback_body"`
	LogLevel       string               `yaml:"log_level" json:"log_level"` // "debug", "info", "warn", "error", "none"; default: "info"
}

// ValidLogLevels are the accepted log level strings for routes.
var ValidLogLevels = map[string]bool{
	"":      true, // empty means default ("info")
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
	"none":  true,
}

// CircuitBreakerConfig holds circuit breaker settings applied to all backends.
type CircuitBreakerConfig struct {
	WindowSize       int           `yaml:"window_size" json:"window_size"`
	FailureThreshold float64       `yaml:"failure_threshold" json:"failure_threshold"`
	ResetTimeout     time.Duration `yaml:"reset_timeout" json:"reset_timeout"`
	HalfOpenMax      int           `yaml:"half_open_max" json:"half_open_max"`
	SlowThreshold    time.Duration `yaml:"slow_threshold" json:"slow_threshold"`
	MaxConcurrent    int           `yaml:"max_concurrent" json:"max_concurrent"`
	Adaptive         bool          `yaml:"adaptive" json:"adaptive"`
	LatencyCeiling   time.Duration `yaml:"latency_ceiling" json:"latency_ceiling"`
	MinThreshold     float64       `yaml:"min_threshold" json:"min_threshold"`
}

// ConnectionPoolConfig holds per-backend HTTP transport pool settings.
type ConnectionPoolConfig struct {
	MaxIdleConns   int           `yaml:"max_idle_conns" json:"max_idle_conns"`
	MaxIdlePerHost int           `yaml:"max_idle_per_host" json:"max_idle_per_host"`
	IdleTimeout    time.Duration `yaml:"idle_timeout" json:"idle_timeout"`
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

// Load reads and parses a YAML configuration file, applies environment
// variable substitution, sets defaults, and validates the result.
// Warnings are stored on cfg.Warnings (goroutine-safe, no package-level state).
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

	cfg.Warnings = collectWarnings(&cfg)

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

	cfg.Warnings = collectWarnings(&cfg)

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Metrics.Path == "" {
		cfg.Metrics.Path = "/metrics"
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}

	// Logging defaults
	if cfg.Logging.Output == "" {
		cfg.Logging.Output = "stdout"
	}
	if cfg.Logging.MaxSizeMB == 0 {
		cfg.Logging.MaxSizeMB = 100
	}
	if cfg.Logging.MaxBackups == 0 {
		cfg.Logging.MaxBackups = 3
	}
	if cfg.Logging.MaxAgeDays == 0 {
		cfg.Logging.MaxAgeDays = 30
	}
	if cfg.Logging.MaxBodyLogBytes == 0 {
		cfg.Logging.MaxBodyLogBytes = 4096
	}

	// TLS defaults
	if cfg.Server.TLS.Enabled && cfg.Server.TLS.MinVersion == "" {
		cfg.Server.TLS.MinVersion = "1.2"
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

	// Circuit breaker defaults
	cb := &cfg.CircuitBreaker
	if cb.WindowSize == 0 {
		cb.WindowSize = 10
	}
	if cb.FailureThreshold == 0 {
		cb.FailureThreshold = 0.5
	}
	if cb.ResetTimeout == 0 {
		cb.ResetTimeout = 30 * time.Second
	}
	if cb.HalfOpenMax == 0 {
		cb.HalfOpenMax = 2
	}
	if cb.Adaptive && cb.LatencyCeiling == 0 {
		cb.LatencyCeiling = 2 * time.Second
	}
	if cb.Adaptive && cb.MinThreshold == 0 {
		cb.MinThreshold = 0.2
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

	// Circuit breaker validation
	cb := cfg.CircuitBreaker
	if cb.WindowSize < 1 {
		return fmt.Errorf("circuit_breaker.window_size must be positive")
	}
	if cb.FailureThreshold <= 0 || cb.FailureThreshold > 1 {
		return fmt.Errorf("circuit_breaker.failure_threshold must be between 0 (exclusive) and 1 (inclusive)")
	}
	if cb.ResetTimeout <= 0 {
		return fmt.Errorf("circuit_breaker.reset_timeout must be positive")
	}
	if cb.HalfOpenMax < 1 {
		return fmt.Errorf("circuit_breaker.half_open_max must be positive")
	}
	if cb.SlowThreshold < 0 {
		return fmt.Errorf("circuit_breaker.slow_threshold must be non-negative")
	}
	if cb.MaxConcurrent < 0 {
		return fmt.Errorf("circuit_breaker.max_concurrent must be non-negative")
	}
	if cb.Adaptive {
		if cb.MinThreshold <= 0 || cb.MinThreshold >= cb.FailureThreshold {
			return fmt.Errorf("circuit_breaker.min_threshold must be between 0 and failure_threshold")
		}
		if cb.LatencyCeiling <= 0 {
			return fmt.Errorf("circuit_breaker.latency_ceiling must be positive when adaptive is enabled")
		}
	}

	if cfg.Server.GlobalTimeoutMs < 0 {
		return fmt.Errorf("server.global_timeout_ms must be non-negative")
	}

	// TLS validation
	if cfg.Server.TLS.Enabled {
		if cfg.Server.TLS.CertFile == "" {
			return fmt.Errorf("server.tls.cert_file is required when TLS is enabled")
		}
		if cfg.Server.TLS.KeyFile == "" {
			return fmt.Errorf("server.tls.key_file is required when TLS is enabled")
		}
		if cfg.Server.TLS.MinVersion != "1.2" && cfg.Server.TLS.MinVersion != "1.3" {
			return fmt.Errorf("server.tls.min_version must be \"1.2\" or \"1.3\", got %q", cfg.Server.TLS.MinVersion)
		}
	}

	// Logging validation
	if cfg.Logging.Output != "stdout" && cfg.Logging.Output != "stderr" {
		if cfg.Logging.MaxSizeMB < 1 {
			return fmt.Errorf("logging.max_size_mb must be positive when output is a file path")
		}
	}
	if cfg.Logging.BodyLogging && cfg.Logging.MaxBodyLogBytes < 1 {
		return fmt.Errorf("logging.max_body_log_bytes must be positive when body_logging is enabled")
	}

	// Admin validation
	if cfg.Admin.Enabled {
		if len(cfg.Admin.IPAllowlist) == 0 {
			return fmt.Errorf("admin.ip_allowlist is required when admin is enabled")
		}
		for i, cidr := range cfg.Admin.IPAllowlist {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("admin.ip_allowlist[%d]: invalid CIDR %q: %w", i, cidr, err)
			}
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

		if !ValidLogLevels[r.LogLevel] {
			return fmt.Errorf("routes[%d].log_level must be one of debug, info, warn, error, none; got %q", i, r.LogLevel)
		}
		if r.FallbackStatus != 0 && (r.FallbackStatus < 200 || r.FallbackStatus > 599) {
			return fmt.Errorf("routes[%d].fallback_status must be between 200 and 599", i)
		}
		if r.ConnectionPool != nil {
			cp := r.ConnectionPool
			if cp.MaxIdleConns < 0 {
				return fmt.Errorf("routes[%d].connection_pool.max_idle_conns must be non-negative", i)
			}
			if cp.MaxIdlePerHost < 0 {
				return fmt.Errorf("routes[%d].connection_pool.max_idle_per_host must be non-negative", i)
			}
			if cp.IdleTimeout < 0 {
				return fmt.Errorf("routes[%d].connection_pool.idle_timeout must be non-negative", i)
			}
		}
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
