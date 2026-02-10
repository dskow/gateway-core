// Package main is the entry point for the API gateway. It loads configuration,
// assembles the middleware stack, starts the HTTP server, and handles graceful
// shutdown on SIGINT/SIGTERM.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/dskow/gateway-core/internal/admin"
	"github.com/dskow/gateway-core/internal/auth"
	"github.com/dskow/gateway-core/internal/circuitbreaker"
	"github.com/dskow/gateway-core/internal/config"
	"github.com/dskow/gateway-core/internal/health"
	"github.com/dskow/gateway-core/internal/logging"
	"github.com/dskow/gateway-core/internal/metrics"
	"github.com/dskow/gateway-core/internal/middleware"
	"github.com/dskow/gateway-core/internal/proxy"
	"github.com/dskow/gateway-core/internal/ratelimit"
	"github.com/dskow/gateway-core/internal/routing"
	"github.com/dskow/gateway-core/internal/tlsutil"
)

func main() {
	configPath := flag.String("config", "configs/gateway.yaml", "path to configuration file")
	flag.Parse()

	// --- Phase 4: Initialize log writer from config ---
	cfg, err := config.Load(*configPath)
	if err != nil {
		// Fallback logger for startup errors.
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logWriter, logCloser := buildLogWriter(cfg.Logging)
	if logCloser != nil {
		defer logCloser.Close()
	}

	logger := slog.New(slog.NewJSONHandler(logWriter, &slog.HandlerOptions{Level: slog.LevelInfo}))

	for _, w := range cfg.Warnings {
		logger.Warn("config warning", "message", w)
	}

	logger.Info("configuration loaded",
		"port", cfg.Server.Port,
		"routes", len(cfg.Routes),
		"auth_enabled", cfg.Auth.Enabled,
		"metrics_enabled", cfg.Metrics.IsEnabled(),
		"metrics_path", cfg.Metrics.Path,
		"trusted_proxies", len(cfg.Server.TrustedProxies),
		"max_body_bytes", cfg.Server.MaxBodyBytes,
		"global_timeout_ms", cfg.Server.GlobalTimeoutMs,
		"tls_enabled", cfg.Server.TLS.Enabled,
		"admin_enabled", cfg.Admin.Enabled,
		"log_output", cfg.Logging.Output,
	)

	// Initialize Prometheus metrics
	if cfg.Metrics.IsEnabled() {
		metrics.Init()
	}

	// Create circuit breakers (one per unique backend URL).
	cbCfg := circuitbreaker.Config{
		WindowSize:       cfg.CircuitBreaker.WindowSize,
		FailureThreshold: cfg.CircuitBreaker.FailureThreshold,
		ResetTimeout:     cfg.CircuitBreaker.ResetTimeout,
		HalfOpenMax:      cfg.CircuitBreaker.HalfOpenMax,
		SlowThreshold:    cfg.CircuitBreaker.SlowThreshold,
		MaxConcurrent:    cfg.CircuitBreaker.MaxConcurrent,
		Adaptive:         cfg.CircuitBreaker.Adaptive,
		LatencyCeiling:   cfg.CircuitBreaker.LatencyCeiling,
		MinThreshold:     cfg.CircuitBreaker.MinThreshold,
	}
	breakers := make(map[string]*circuitbreaker.CompositeBreaker)
	for _, route := range cfg.Routes {
		if _, exists := breakers[route.Backend]; !exists {
			breakers[route.Backend] = circuitbreaker.NewComposite(route.Backend, cbCfg, logger)
			logger.Info("circuit breaker created", "backend", route.Backend)
		}
	}

	// Build the proxy router
	router, err := proxy.New(cfg.Routes, breakers, logger)
	if err != nil {
		logger.Error("failed to create proxy router", "error", err)
		os.Exit(1)
	}

	// Build the rate limiter
	limiter := ratelimit.New(cfg.RateLimit, cfg.Routes, cfg.Server.TrustedProxies, logger)
	defer limiter.Stop()

	// Route auth checker: looks up whether a matching route requires auth
	routeRequiresAuth := func(path string) bool {
		route, ok := router.MatchRoute(path)
		if !ok {
			return false
		}
		return route.AuthRequired
	}

	// --- Phase 4: Per-route log level callback ---
	// Uses atomic.Value for lock-free reads from the request goroutine.
	// The OnReload callback stores a new slice atomically.
	var routesRef atomic.Value
	routesRef.Store(cfg.Routes)
	routeLogLevel := func(path string) slog.Level {
		routes := routesRef.Load().([]config.RouteConfig)
		bestLen := 0
		bestLevel := slog.LevelInfo
		for _, route := range routes {
			if routing.MatchesPrefix(path, route.PathPrefix) && len(route.PathPrefix) > bestLen {
				bestLen = len(route.PathPrefix)
				bestLevel = middleware.ParseLogLevel(route.LogLevel)
			}
		}
		return bestLevel
	}

	// --- Phase 4: Body logging config ---
	var bodyConfig *middleware.LoggingConfig
	if cfg.Logging.BodyLogging {
		bodyConfig = &middleware.LoggingConfig{
			BodyLogging:     true,
			MaxBodyLogBytes: cfg.Logging.MaxBodyLogBytes,
		}
	}

	// Assemble middleware stack:
	// Recovery → RequestID → Deadline → SecurityHeaders → Logging → CORS → BodyLimit → RateLimit → Auth → Proxy
	var handler http.Handler = router
	handler = auth.Middleware(cfg.Auth, routeRequiresAuth, logger)(handler)
	handler = limiter.Middleware()(handler)
	handler = middleware.BodyLimit(cfg.Server.MaxBodyBytes)(handler)
	handler = middleware.CORS(middleware.DefaultCORSConfig())(handler)
	handler = middleware.Logging(logger, routeLogLevel, bodyConfig)(handler)
	handler = middleware.SecurityHeaders()(handler)
	handler = middleware.Deadline(cfg.Server.GlobalTimeout())(handler)
	handler = middleware.RequestID(handler)
	handler = middleware.Recovery(logger)(handler)

	// Register health check and metrics routes on a separate mux,
	// then combine with the main handler
	mux := http.NewServeMux()
	healthHandler := health.New(cfg.Routes, breakers, logger)
	healthHandler.RegisterRoutes(mux)

	metricsPath := cfg.Metrics.Path
	if cfg.Metrics.IsEnabled() {
		mux.Handle(metricsPath, metrics.Handler())
		logger.Info("metrics endpoint registered", "path", metricsPath)
	}

	// Initialize config reloader (before admin, so admin can reference it).
	reloader := config.NewReloader(*configPath, cfg, logger)

	// --- Phase 4: Admin API ---
	adminEnabled := cfg.Admin.Enabled
	if adminEnabled {
		adminHandler := admin.New(reloader, limiter, breakers, cfg.Routes, cfg.Admin.IPAllowlist, logger)
		adminHandler.RegisterRoutes(mux)
		logger.Info("admin API enabled", "allowlist", cfg.Admin.IPAllowlist)
	}

	// Build a set of exact-match bypass paths and prefix-match bypass paths
	// at startup so the hot-path combined handler avoids repeated conditionals.
	bypassExact := make(map[string]bool)
	if cfg.Metrics.IsEnabled() {
		bypassExact[metricsPath] = true
	}
	bypassPrefixes := []string{"/health", "/ready"}
	if adminEnabled {
		bypassPrefixes = append(bypassPrefixes, "/admin/")
	}

	// Combine: health, metrics, and admin endpoints bypass the middleware stack
	combined := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if bypassExact[path] {
			mux.ServeHTTP(w, r)
			return
		}
		for _, prefix := range bypassPrefixes {
			if strings.HasPrefix(path, prefix) {
				mux.ServeHTTP(w, r)
				return
			}
		}
		handler.ServeHTTP(w, r)
	})

	reloader.Start()
	defer reloader.Stop()

	// Register reload callbacks for components that support hot-reload
	reloader.OnReload(func(newCfg *config.Config) {
		limiter.UpdateConfig(newCfg.RateLimit, newCfg.Routes)

		// Update circuit breaker configs.
		newCbCfg := circuitbreaker.Config{
			WindowSize:       newCfg.CircuitBreaker.WindowSize,
			FailureThreshold: newCfg.CircuitBreaker.FailureThreshold,
			ResetTimeout:     newCfg.CircuitBreaker.ResetTimeout,
			HalfOpenMax:      newCfg.CircuitBreaker.HalfOpenMax,
			SlowThreshold:    newCfg.CircuitBreaker.SlowThreshold,
			MaxConcurrent:    newCfg.CircuitBreaker.MaxConcurrent,
			Adaptive:         newCfg.CircuitBreaker.Adaptive,
			LatencyCeiling:   newCfg.CircuitBreaker.LatencyCeiling,
			MinThreshold:     newCfg.CircuitBreaker.MinThreshold,
		}
		for backend, cb := range breakers {
			cb.UpdateConfig(newCbCfg)
			logger.Info("circuit breaker config updated", "backend", backend)
		}

		// Phase 4: Update route log levels on hot-reload.
		routesRef.Store(newCfg.Routes)
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      combined,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// --- Phase 4: TLS support ---
	var certLoader *tlsutil.CertLoader
	if cfg.Server.TLS.Enabled {
		cl, err := tlsutil.New(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile, logger)
		if err != nil {
			logger.Error("failed to load TLS certificate", "error", err)
			os.Exit(1)
		}
		certLoader = cl
		defer certLoader.Stop()

		minVersion := tls.VersionTLS12
		if cfg.Server.TLS.MinVersion == "1.3" {
			minVersion = tls.VersionTLS13
		}

		srv.TLSConfig = &tls.Config{
			GetCertificate: certLoader.GetCertificate,
			MinVersion:     uint16(minVersion),
		}
	}

	// Start server in a goroutine
	go func() {
		if cfg.Server.TLS.Enabled {
			logger.Info("starting gateway with TLS", "addr", srv.Addr, "min_tls", cfg.Server.TLS.MinVersion)
			// Cert/key are loaded by GetCertificate callback, so pass empty strings.
			if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				logger.Error("server error", "error", err)
				os.Exit(1)
			}
		} else {
			logger.Info("starting gateway", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("server error", "error", err)
				os.Exit(1)
			}
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logger.Info("shutdown signal received", "signal", sig.String())

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	logger.Info("draining in-flight requests", "timeout", cfg.Server.ShutdownTimeout)
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("forced shutdown", "error", err)
		os.Exit(1)
	}

	logger.Info("gateway stopped gracefully")
}

// buildLogWriter returns the io.Writer for the slog handler and an optional
// io.Closer for file-based writers. Returns (os.Stdout, nil) for the default.
func buildLogWriter(cfg config.LoggingConfig) (io.Writer, io.Closer) {
	switch cfg.Output {
	case "stdout", "":
		return os.Stdout, nil
	case "stderr":
		return os.Stderr, nil
	default:
		rw, err := logging.NewRotatingWriter(cfg.Output, cfg.MaxSizeMB, cfg.MaxBackups, cfg.MaxAgeDays)
		if err != nil {
			// Fall back to stdout if file open fails.
			slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("failed to open log file, falling back to stdout",
				"path", cfg.Output, "error", err)
			return os.Stdout, nil
		}
		return rw, rw
	}
}
