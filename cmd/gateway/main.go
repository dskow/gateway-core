// Package main is the entry point for the API gateway. Construction and
// lifecycle belong to internal/gateway; main() only handles flag parsing,
// logger bootstrapping, and signal-driven context cancellation (DP-003).
package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dskow/gateway-core/internal/config"
	"github.com/dskow/gateway-core/internal/gateway"
	"github.com/dskow/gateway-core/internal/logging"
)

func main() {
	configPath := flag.String("config", "configs/gateway.yaml", "path to configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	gw, err := gateway.NewGateway(ctx, cfg, logger, gateway.Options{})
	if err != nil {
		logger.Error("failed to build gateway", "error", err)
		os.Exit(1)
	}
	gw.SetReloadPath(*configPath)

	if err := gw.Run(ctx); err != nil {
		logger.Error("gateway exited with error", "error", err)
		os.Exit(1)
	}
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
			slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("failed to open log file, falling back to stdout",
				"path", cfg.Output, "error", err)
			return os.Stdout, nil
		}
		return rw, rw
	}
}
