// Package gateway wires every subsystem that makes up the running API
// gateway into a single owned object and exposes Run / Shutdown lifecycle
// methods. The package exists so main() can fit on one screen and so a
// whole gateway can be instantiated in-process for end-to-end tests
// without duplicating the wiring logic in a test harness (DP-003).
package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/dskow/gateway-core/internal/admin"
	"github.com/dskow/gateway-core/internal/auth"
	"github.com/dskow/gateway-core/internal/circuitbreaker"
	"github.com/dskow/gateway-core/internal/config"
	"github.com/dskow/gateway-core/internal/health"
	"github.com/dskow/gateway-core/internal/metrics"
	"github.com/dskow/gateway-core/internal/middleware"
	"github.com/dskow/gateway-core/internal/proxy"
	"github.com/dskow/gateway-core/internal/ratelimit"
	"github.com/dskow/gateway-core/internal/routing"
	"github.com/dskow/gateway-core/internal/tlsutil"
	"github.com/prometheus/client_golang/prometheus"
)

// Gateway owns every long-lived component that cooperates on the request
// path. Fields are exported so tests can inspect or substitute them; in
// production code nothing outside of this package should mutate them after
// NewGateway returns.
type Gateway struct {
	Config   *config.Config
	Logger   *slog.Logger
	Metrics  *metrics.Metrics
	Router   *proxy.Router
	Limiter  *ratelimit.Limiter
	Breakers map[string]*circuitbreaker.CompositeBreaker
	Reloader *config.Reloader
	Health   *health.Handler
	Admin    *admin.Handler
	Server   *http.Server

	// handler is the top-level HTTP handler mounted on Server; it
	// composes mux (bypass endpoints) with the request-path handler.
	handler http.Handler

	// routesRef lets request-path callbacks (log-level lookup) read
	// the current route table lock-free, and lets the reload callback
	// swap it atomically.
	routesRef atomic.Value // []config.RouteConfig

	certLoader *tlsutil.CertLoader
}

// Options customize gateway construction. Zero values are fine; pass
// Registerer to share metrics with another process (tests use a fresh
// registry to stay isolated).
type Options struct {
	// Registerer is where the Metrics bundle registers collectors.
	// Defaults to prometheus.DefaultRegisterer when nil and metrics
	// are enabled in cfg.
	Registerer prometheus.Registerer
	// Gatherer is what the /metrics endpoint exports. Defaults to
	// prometheus.DefaultGatherer when nil.
	Gatherer prometheus.Gatherer
}

// NewGateway constructs a Gateway in strict dependency order: Metrics →
// Breakers → Router → Limiter → middleware stack → mux → Reloader →
// Server. Every component that needs to be torn down is owned here, so
// Run+Shutdown is a complete lifecycle.
//
// ctx is reserved for future deadline-bound initialization (e.g. TLS cert
// preloading) and currently only gates the cert loader start. Pass a
// fresh context if construction must respect a parent deadline.
func NewGateway(ctx context.Context, cfg *config.Config, logger *slog.Logger, opts Options) (*Gateway, error) {
	g := &Gateway{
		Config: cfg,
		Logger: logger,
	}

	if cfg.Metrics.IsEnabled() {
		reg := opts.Registerer
		if reg == nil {
			reg = prometheus.DefaultRegisterer
		}
		g.Metrics = metrics.New(reg)
	}

	// Circuit breakers — one per unique backend URL.
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
	g.Breakers = make(map[string]*circuitbreaker.CompositeBreaker)
	for _, route := range cfg.Routes {
		if _, exists := g.Breakers[route.Backend]; !exists {
			g.Breakers[route.Backend] = circuitbreaker.NewComposite(route.Backend, cbCfg, logger, g.Metrics)
			logger.Info("circuit breaker created", "backend", route.Backend)
		}
	}

	router, err := proxy.New(cfg.Routes, g.Breakers, logger, g.Metrics)
	if err != nil {
		return nil, fmt.Errorf("building proxy router: %w", err)
	}
	g.Router = router

	g.Limiter = ratelimit.New(cfg.RateLimit, cfg.Routes, cfg.Server.TrustedProxies, logger, g.Metrics)

	g.routesRef.Store(cfg.Routes)

	routeRequiresAuth := func(path string) bool {
		route, ok := router.MatchRoute(path)
		if !ok {
			return false
		}
		return route.AuthRequired
	}
	routeLogLevel := func(path string) slog.Level {
		routes := g.routesRef.Load().([]config.RouteConfig)
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

	var bodyConfig *middleware.LoggingConfig
	if cfg.Logging.BodyLogging {
		bodyConfig = &middleware.LoggingConfig{
			BodyLogging:     true,
			MaxBodyLogBytes: cfg.Logging.MaxBodyLogBytes,
		}
	}

	// Middleware stack (inside-out assembly matches the original main()):
	// Recovery → RequestID → Deadline → SecurityHeaders → Logging → CORS →
	// BodyLimit → RateLimit → Auth → Proxy. Order is load-bearing — Recovery
	// must wrap everything, Auth must be last before the proxy so claims
	// are on the context the upstream sees.
	var handler http.Handler = router
	handler = auth.Middleware(cfg.Auth, routeRequiresAuth, logger, g.Metrics)(handler)
	handler = g.Limiter.Middleware()(handler)
	handler = middleware.BodyLimit(cfg.Server.MaxBodyBytes)(handler)
	handler = middleware.CORS(middleware.DefaultCORSConfig())(handler)
	handler = middleware.Logging(logger, routeLogLevel, bodyConfig)(handler)
	handler = middleware.SecurityHeaders()(handler)
	handler = middleware.Deadline(cfg.Server.GlobalTimeout())(handler)
	handler = middleware.RequestID(handler)
	handler = middleware.Recovery(logger)(handler)

	// Separate mux for /health, /ready, /metrics, /admin — these bypass
	// the request-path middleware stack entirely.
	mux := http.NewServeMux()
	g.Health = health.New(cfg.Routes, g.Breakers, logger)
	g.Health.RegisterRoutes(mux)

	if cfg.Metrics.IsEnabled() {
		gatherer := opts.Gatherer
		if gatherer == nil {
			gatherer = prometheus.DefaultGatherer
		}
		mux.Handle(cfg.Metrics.Path, metrics.Handler(gatherer))
		logger.Info("metrics endpoint registered", "path", cfg.Metrics.Path)
	}

	// Reloader is constructed before admin so admin can reference it.
	// DP-001 wires SetRollbackRecorder and RegisterObserver on top of this
	// construction; for DP-003 alone we use the legacy fire-and-forget
	// callback below.
	g.Reloader = config.NewReloader("", cfg, logger)

	if cfg.Admin.Enabled {
		g.Admin = admin.New(g.Reloader, g.Limiter, g.Breakers, cfg.Routes, cfg.Admin.IPAllowlist, logger)
		g.Admin.RegisterRoutes(mux)
		logger.Info("admin API enabled", "allowlist", cfg.Admin.IPAllowlist)
	}

	bypassExact := map[string]bool{}
	if cfg.Metrics.IsEnabled() {
		bypassExact[cfg.Metrics.Path] = true
	}
	bypassPrefixes := []string{"/health", "/ready"}
	if cfg.Admin.Enabled {
		bypassPrefixes = append(bypassPrefixes, "/admin/")
	}

	g.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	// Subscribe the limiter and breakers to config reloads. The callback
	// is idempotent — it overwrites limiter rates, breaker thresholds,
	// and the routes atom unconditionally. DP-001 upgrades this to a
	// rollback-capable ConfigObserver once that interface lands.
	g.Reloader.OnReload(func(newCfg *config.Config) {
		g.Limiter.UpdateConfig(newCfg.RateLimit, newCfg.Routes)
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
		for backend, cb := range g.Breakers {
			cb.UpdateConfig(newCbCfg)
			logger.Info("circuit breaker config updated", "backend", backend)
		}
		g.routesRef.Store(newCfg.Routes)
	})

	g.Server = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      g.handler,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	if cfg.Server.TLS.Enabled {
		cl, err := tlsutil.New(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile, logger)
		if err != nil {
			return nil, fmt.Errorf("loading TLS certificate: %w", err)
		}
		g.certLoader = cl

		minVersion := uint16(tls.VersionTLS12)
		if cfg.Server.TLS.MinVersion == "1.3" {
			minVersion = tls.VersionTLS13
		}
		g.Server.TLSConfig = &tls.Config{
			GetCertificate: cl.GetCertificate,
			MinVersion:     minVersion,
		}
	}

	_ = ctx // reserved for future deadline-bound initialization
	return g, nil
}

// SetReloadPath configures the Reloader's watched file path. main() calls
// this after NewGateway so the gateway can be constructed from an in-memory
// Config (e.g. in tests) without a file on disk.
func (g *Gateway) SetReloadPath(path string) {
	g.Reloader.SetPath(path)
}

// Handler returns the composed top-level handler, exported for tests that
// exercise requests in-process without binding a TCP listener.
func (g *Gateway) Handler() http.Handler { return g.handler }

// Run starts the watcher, binds the HTTP server, and blocks until ctx is
// cancelled or the server returns a fatal error. Graceful shutdown happens
// automatically when ctx is cancelled, bounded by cfg.Server.ShutdownTimeout.
func (g *Gateway) Run(ctx context.Context) error {
	g.Reloader.Start()
	defer g.Reloader.Stop()
	defer g.Limiter.Close()
	if g.certLoader != nil {
		defer g.certLoader.Stop()
	}

	serverErr := make(chan error, 1)
	go func() {
		if g.Config.Server.TLS.Enabled {
			g.Logger.Info("starting gateway with TLS",
				"addr", g.Server.Addr,
				"min_tls", g.Config.Server.TLS.MinVersion,
			)
			err := g.Server.ListenAndServeTLS("", "")
			if !errors.Is(err, http.ErrServerClosed) {
				serverErr <- err
			}
		} else {
			g.Logger.Info("starting gateway", "addr", g.Server.Addr)
			err := g.Server.ListenAndServe()
			if !errors.Is(err, http.ErrServerClosed) {
				serverErr <- err
			}
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), g.Config.Server.ShutdownTimeout)
	defer cancel()
	g.Logger.Info("draining in-flight requests", "timeout", g.Config.Server.ShutdownTimeout)
	if err := g.Server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("forced shutdown: %w", err)
	}
	g.Logger.Info("gateway stopped gracefully")
	return nil
}
