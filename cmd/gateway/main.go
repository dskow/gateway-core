// Package main is the entry point for the API gateway. It loads configuration,
// assembles the middleware stack, starts the HTTP server, and handles graceful
// shutdown on SIGINT/SIGTERM.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/dskow/go-api-gateway/internal/auth"
	"github.com/dskow/go-api-gateway/internal/config"
	"github.com/dskow/go-api-gateway/internal/health"
	"github.com/dskow/go-api-gateway/internal/metrics"
	"github.com/dskow/go-api-gateway/internal/middleware"
	"github.com/dskow/go-api-gateway/internal/proxy"
	"github.com/dskow/go-api-gateway/internal/ratelimit"
)

func main() {
	configPath := flag.String("config", "configs/gateway.yaml", "path to configuration file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

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
	)

	// Initialize Prometheus metrics
	if cfg.Metrics.IsEnabled() {
		metrics.Init()
	}

	// Build the proxy router
	router, err := proxy.New(cfg.Routes, logger)
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

	// Assemble middleware stack:
	// Recovery → RequestID → SecurityHeaders → Logging → CORS → BodyLimit → RateLimit → Auth → Proxy
	var handler http.Handler = router
	handler = auth.Middleware(cfg.Auth, routeRequiresAuth, logger)(handler)
	handler = limiter.Middleware()(handler)
	handler = middleware.BodyLimit(cfg.Server.MaxBodyBytes)(handler)
	handler = middleware.CORS(middleware.DefaultCORSConfig())(handler)
	handler = middleware.Logging(logger)(handler)
	handler = middleware.SecurityHeaders()(handler)
	handler = middleware.RequestID(handler)
	handler = middleware.Recovery(logger)(handler)

	// Register health check and metrics routes on a separate mux,
	// then combine with the main handler
	mux := http.NewServeMux()
	healthHandler := health.New(cfg.Routes, logger)
	healthHandler.RegisterRoutes(mux)

	metricsPath := cfg.Metrics.Path
	if cfg.Metrics.IsEnabled() {
		mux.Handle(metricsPath, metrics.Handler())
		logger.Info("metrics endpoint registered", "path", metricsPath)
	}

	// Combine: health and metrics endpoints bypass the middleware stack
	combined := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/health") ||
			strings.HasPrefix(r.URL.Path, "/ready") ||
			(cfg.Metrics.IsEnabled() && r.URL.Path == metricsPath) {
			mux.ServeHTTP(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	})

	// Initialize config reloader
	reloader := config.NewReloader(*configPath, cfg, logger)
	reloader.Start()
	defer reloader.Stop()

	// Register reload callbacks for components that support hot-reload
	reloader.OnReload(func(newCfg *config.Config) {
		limiter.UpdateConfig(newCfg.RateLimit, newCfg.Routes)
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      combined,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// Start server in a goroutine
	go func() {
		logger.Info("starting gateway", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
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
