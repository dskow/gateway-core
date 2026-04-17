package config

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ConfigObserver is the DP-001 reload contract: given the old and new
// configs, apply the change and return an error if the change cannot be
// accepted. A non-nil return (or a panic) forces the Reloader to roll
// back r.current to old, logged and counted as a rollback.
//
// Contract for implementers: OnReload must be *effectively transactional*
// — either the observer fully adopts `new` or leaves itself in a state
// equivalent to `old`. Because rollback only restores the pointer, not
// earlier observers' side effects, every observer must also be idempotent
// so a subsequent successful reload can bring the component back in line.
// See docs/DESIGN_PATTERNS_PLAN.md §7 Risk Register for rationale.
type ConfigObserver interface {
	OnReload(old, new *Config) error
}

// ConfigObserverFunc adapts an ordinary function to the ConfigObserver
// interface — useful for Gateway wiring and tests.
type ConfigObserverFunc func(old, new *Config) error

func (f ConfigObserverFunc) OnReload(old, new *Config) error { return f(old, new) }

// RollbackRecorder is the subset of *metrics.Metrics used by Reloader.
// Defined here as an interface so the config package does not import the
// metrics package (avoids an import cycle once Gateway grows).
type RollbackRecorder interface {
	IncRollback(reason string)
}

// Reloader watches the config file and reloads on changes.
// It supports fsnotify file watching (cross-platform) and SIGHUP
// (Unix only, registered in reload_unix.go).
type Reloader struct {
	mu sync.RWMutex
	current *Config
	path    string
	logger  *slog.Logger
	// legacyCallbacks preserves the pre-DP-001 fire-and-forget hooks that
	// cannot fail; they are invoked after all observers have accepted.
	legacyCallbacks []func(*Config)
	observers       []ConfigObserver
	rollbacks       RollbackRecorder
	watcher         *fsnotify.Watcher
	stopCh          chan struct{}
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

// SetRollbackRecorder wires the metrics sink used to count rollbacks.
// Safe to call at most once, before Start.
func (r *Reloader) SetRollbackRecorder(rec RollbackRecorder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rollbacks = rec
}

// Current returns the active configuration (thread-safe).
func (r *Reloader) Current() *Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// SetPath updates the watched config file path. Intended for callers that
// construct a Reloader before the final path is known (e.g. Gateway wiring
// that accepts an in-memory Config in tests). Must be called before Start.
func (r *Reloader) SetPath(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path = path
}

// OnReload registers a legacy callback that is invoked with the new
// config after every observer has accepted. Preserved for callers that
// cannot fail the reload; new code should implement ConfigObserver
// directly via RegisterObserver.
func (r *Reloader) OnReload(fn func(*Config)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.legacyCallbacks = append(r.legacyCallbacks, fn)
}

// RegisterObserver adds an observer to the rollback-capable reload
// pipeline. Observers run in registration order; the first one to
// return a non-nil error (or panic) triggers a rollback.
func (r *Reloader) RegisterObserver(obs ConfigObserver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observers = append(r.observers, obs)
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
// in and runs every observer. If any observer returns an error or panics,
// the swap is reverted, the rollbacks counter is incremented, and the
// method returns false. Legacy OnReload callbacks (which cannot fail) run
// only after every observer has accepted. Exported so signal handlers and
// tests can call it.
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
	observers := make([]ConfigObserver, len(r.observers))
	copy(observers, r.observers)
	legacy := make([]func(*Config), len(r.legacyCallbacks))
	copy(legacy, r.legacyCallbacks)
	rollbacks := r.rollbacks
	r.mu.Unlock()

	r.logChanges(old, newCfg)

	for i, obs := range observers {
		reason, detail, ok := invokeObserver(obs, old, newCfg)
		if !ok {
			r.logger.Error("config reload rolled back",
				"observer_index", i, "reason", reason, "detail", detail)
			r.mu.Lock()
			r.current = old
			r.mu.Unlock()
			if rollbacks != nil {
				rollbacks.IncRollback(reason)
			}
			return false
		}
	}

	for _, cb := range legacy {
		cb(newCfg)
	}

	r.logger.Info("configuration reloaded successfully")
	return true
}

// invokeObserver calls obs.OnReload with panic recovery. Returns a stable
// low-cardinality reason label (for Prometheus), a free-form detail string
// (for logs), and false when the observer rejected the reload.
func invokeObserver(obs ConfigObserver, old, newCfg *Config) (reason, detail string, ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			reason = "observer_panic"
			detail = fmt.Sprintf("%v", rec)
			ok = false
		}
	}()
	if err := obs.OnReload(old, newCfg); err != nil {
		return "observer_error", err.Error(), false
	}
	return "", "", true
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
