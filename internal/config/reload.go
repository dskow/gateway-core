package config

import (
	"log/slog"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Reloader watches the config file and reloads on changes.
// It supports fsnotify file watching (cross-platform) and SIGHUP
// (Unix only, registered in reload_unix.go).
type Reloader struct {
	mu        sync.RWMutex
	current   *Config
	path      string
	logger    *slog.Logger
	callbacks []func(*Config)
	watcher   *fsnotify.Watcher
	stopCh    chan struct{}
}

// NewReloader creates a Reloader for the given config file path.
func NewReloader(path string, initial *Config, logger *slog.Logger) *Reloader {
	return &Reloader{
		current: initial,
		path:    path,
		logger:  logger,
		stopCh:  make(chan struct{}),
	}
}

// Current returns the active configuration (thread-safe).
func (r *Reloader) Current() *Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// OnReload registers a callback that is invoked with the new config
// after a successful reload.
func (r *Reloader) OnReload(fn func(*Config)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.callbacks = append(r.callbacks, fn)
}

// Start begins watching the config file for changes and listening for
// SIGHUP (on Unix). Must be called once after NewReloader.
func (r *Reloader) Start() {
	// Start fsnotify file watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		r.logger.Error("failed to create file watcher", "error", err)
		return
	}
	r.watcher = watcher

	if err := watcher.Add(r.path); err != nil {
		r.logger.Error("failed to watch config file", "path", r.path, "error", err)
		watcher.Close()
		r.watcher = nil
		return
	}

	r.logger.Info("config file watcher started", "path", r.path)

	go r.watchLoop()

	// Register SIGHUP handler (Unix only — no-op on Windows)
	r.registerSignalHandler()
}

// Stop terminates the file watcher and signal handler.
func (r *Reloader) Stop() {
	close(r.stopCh)
	if r.watcher != nil {
		r.watcher.Close()
	}
}

// Reload loads the config from disk, validates it, and if valid swaps it
// in and notifies all registered callbacks. Returns true if the reload
// succeeded. Exported so signal handlers and tests can call it.
func (r *Reloader) Reload() bool {
	r.logger.Info("reloading configuration", "path", r.path)

	newCfg, err := Load(r.path)
	if err != nil {
		r.logger.Error("config reload failed: invalid config, keeping current",
			"path", r.path, "error", err)
		return false
	}

	r.mu.Lock()
	old := r.current
	r.current = newCfg
	callbacks := make([]func(*Config), len(r.callbacks))
	copy(callbacks, r.callbacks)
	r.mu.Unlock()

	r.logChanges(old, newCfg)

	for _, cb := range callbacks {
		cb(newCfg)
	}

	r.logger.Info("configuration reloaded successfully")
	return true
}

// watchLoop processes fsnotify events with debouncing.
func (r *Reloader) watchLoop() {
	// Debounce timer — editors often write multiple events on save.
	var debounce *time.Timer

	for {
		select {
		case event, ok := <-r.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(300*time.Millisecond, func() {
					r.Reload()
				})
			}
		case err, ok := <-r.watcher.Errors:
			if !ok {
				return
			}
			r.logger.Error("file watcher error", "error", err)
		case <-r.stopCh:
			if debounce != nil {
				debounce.Stop()
			}
			return
		}
	}
}

// logChanges logs a summary of what changed between the old and new config.
func (r *Reloader) logChanges(old, new *Config) {
	if old.RateLimit.RequestsPerSecond != new.RateLimit.RequestsPerSecond ||
		old.RateLimit.BurstSize != new.RateLimit.BurstSize {
		r.logger.Info("rate limit config changed",
			"old_rps", old.RateLimit.RequestsPerSecond,
			"new_rps", new.RateLimit.RequestsPerSecond,
			"old_burst", old.RateLimit.BurstSize,
			"new_burst", new.RateLimit.BurstSize,
		)
	}

	if len(old.Routes) != len(new.Routes) {
		r.logger.Info("route count changed",
			"old", len(old.Routes),
			"new", len(new.Routes),
		)
	}

	if old.Auth.Enabled != new.Auth.Enabled {
		r.logger.Info("auth enabled changed",
			"old", old.Auth.Enabled,
			"new", new.Auth.Enabled,
		)
	}
}
