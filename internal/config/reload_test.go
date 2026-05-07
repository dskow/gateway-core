package config

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	return logger, &buf
}

func writeTestConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "test-config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
	return path
}

const validConfig = `
server:
  port: 8080
rate_limit:
  requests_per_second: 100
  burst_size: 50
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
`

const validConfigUpdated = `
server:
  port: 8080
rate_limit:
  requests_per_second: 200
  burst_size: 100
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
`

const invalidConfig = `
server:
  port: -1
routes: []
`

func TestReloader_Current(t *testing.T) {
	logger, _ := newTestLogger()
	dir := t.TempDir()
	path := writeTestConfig(t, dir, validConfig)

	initial, err := Load(path)
	if err != nil {
		t.Fatalf("failed to load initial config: %v", err)
	}

	r := NewReloader(path, initial, logger)
	cfg := r.Current()
	if cfg.RateLimit.RequestsPerSecond != 100 {
		t.Errorf("expected 100 rps, got %v", cfg.RateLimit.RequestsPerSecond)
	}
}

func TestReloader_Reload_ValidConfig(t *testing.T) {
	logger, _ := newTestLogger()
	dir := t.TempDir()
	path := writeTestConfig(t, dir, validConfig)

	initial, err := Load(path)
	if err != nil {
		t.Fatalf("failed to load initial config: %v", err)
	}

	r := NewReloader(path, initial, logger)

	// Update the config file
	if err := os.WriteFile(path, []byte(validConfigUpdated), 0644); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	ok := r.Reload()
	if !ok {
		t.Fatal("expected reload to succeed")
	}

	cfg := r.Current()
	if cfg.RateLimit.RequestsPerSecond != 200 {
		t.Errorf("expected 200 rps after reload, got %v", cfg.RateLimit.RequestsPerSecond)
	}
	if cfg.RateLimit.BurstSize != 100 {
		t.Errorf("expected 100 burst after reload, got %v", cfg.RateLimit.BurstSize)
	}
}

func TestReloader_Reload_InvalidConfig(t *testing.T) {
	logger, logBuf := newTestLogger()
	dir := t.TempDir()
	path := writeTestConfig(t, dir, validConfig)

	initial, err := Load(path)
	if err != nil {
		t.Fatalf("failed to load initial config: %v", err)
	}

	r := NewReloader(path, initial, logger)

	// Write invalid config
	if err := os.WriteFile(path, []byte(invalidConfig), 0644); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	ok := r.Reload()
	if ok {
		t.Fatal("expected reload to fail for invalid config")
	}

	// Original config should be preserved
	cfg := r.Current()
	if cfg.RateLimit.RequestsPerSecond != 100 {
		t.Errorf("expected original 100 rps preserved, got %v", cfg.RateLimit.RequestsPerSecond)
	}

	if !strings.Contains(logBuf.String(), "config reload failed") {
		t.Error("expected error to be logged")
	}
}

func TestReloader_OnReload_Callback(t *testing.T) {
	logger, _ := newTestLogger()
	dir := t.TempDir()
	path := writeTestConfig(t, dir, validConfig)

	initial, err := Load(path)
	if err != nil {
		t.Fatalf("failed to load initial config: %v", err)
	}

	r := NewReloader(path, initial, logger)

	var callbackCalled bool
	var callbackRPS float64
	r.OnReload(func(cfg *Config) {
		callbackCalled = true
		callbackRPS = cfg.RateLimit.RequestsPerSecond
	})

	// Update and reload
	if err := os.WriteFile(path, []byte(validConfigUpdated), 0644); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	r.Reload()

	if !callbackCalled {
		t.Fatal("expected callback to be called")
	}
	if callbackRPS != 200 {
		t.Errorf("expected callback to receive 200 rps, got %v", callbackRPS)
	}
}

func TestReloader_OnReload_NotCalledOnFailure(t *testing.T) {
	logger, _ := newTestLogger()
	dir := t.TempDir()
	path := writeTestConfig(t, dir, validConfig)

	initial, err := Load(path)
	if err != nil {
		t.Fatalf("failed to load initial config: %v", err)
	}

	r := NewReloader(path, initial, logger)

	callbackCalled := false
	r.OnReload(func(cfg *Config) {
		callbackCalled = true
	})

	// Write invalid config and attempt reload
	if err := os.WriteFile(path, []byte(invalidConfig), 0644); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	r.Reload()

	if callbackCalled {
		t.Fatal("callback should not be called on failed reload")
	}
}

func TestReloader_FileWatch(t *testing.T) {
	logger, _ := newTestLogger()
	dir := t.TempDir()
	path := writeTestConfig(t, dir, validConfig)

	initial, err := Load(path)
	if err != nil {
		t.Fatalf("failed to load initial config: %v", err)
	}

	r := NewReloader(path, initial, logger)

	reloadDone := make(chan struct{}, 1)
	r.OnReload(func(cfg *Config) {
		select {
		case reloadDone <- struct{}{}:
		default:
		}
	})

	r.Start()
	defer r.Stop()

	// Give the watcher time to initialize
	time.Sleep(100 * time.Millisecond)

	// Write updated config to trigger file watch
	if err := os.WriteFile(path, []byte(validConfigUpdated), 0644); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	// Wait for reload with timeout
	select {
	case <-reloadDone:
		cfg := r.Current()
		if cfg.RateLimit.RequestsPerSecond != 200 {
			t.Errorf("expected 200 rps after file watch reload, got %v", cfg.RateLimit.RequestsPerSecond)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("file watch reload timed out")
	}
}

func TestReloader_LogChanges(t *testing.T) {
	logger, logBuf := newTestLogger()
	dir := t.TempDir()
	path := writeTestConfig(t, dir, validConfig)

	initial, err := Load(path)
	if err != nil {
		t.Fatalf("failed to load initial config: %v", err)
	}

	r := NewReloader(path, initial, logger)

	// Update and reload
	if err := os.WriteFile(path, []byte(validConfigUpdated), 0644); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	r.Reload()

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "rate limit config changed") {
		t.Error("expected rate limit change to be logged")
	}
}

// countingRecorder captures rollback counter increments for assertions.
type countingRecorder struct {
	byReason map[string]int
}

func (c *countingRecorder) IncRollback(reason string) {
	if c.byReason == nil {
		c.byReason = map[string]int{}
	}
	c.byReason[reason]++
}

// DP-001: when an observer returns an error, the Reloader's Current() must
// revert to the pre-reload value and the rollbacks counter must increment.
func TestReloader_ObserverErrorRollsBack(t *testing.T) {
	logger, _ := newTestLogger()
	dir := t.TempDir()
	path := writeTestConfig(t, dir, validConfig)
	initial, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	r := NewReloader(path, initial, logger)
	rec := &countingRecorder{}
	r.SetRollbackRecorder(rec)

	var observed struct {
		sawOld *Config
		sawNew *Config
	}
	r.RegisterObserver(ObserverFunc(func(old, new *Config) error {
		observed.sawOld = old
		observed.sawNew = new
		return errors.New("observer refuses this reload")
	}))

	if err := os.WriteFile(path, []byte(validConfigUpdated), 0644); err != nil {
		t.Fatalf("write updated: %v", err)
	}
	if r.Reload() {
		t.Fatal("Reload must return false when an observer errors")
	}

	cfg := r.Current()
	if cfg.RateLimit.RequestsPerSecond != 100 {
		t.Fatalf("Current() must hold the pre-reload config, got rps=%v",
			cfg.RateLimit.RequestsPerSecond)
	}
	if observed.sawOld == nil || observed.sawNew == nil {
		t.Fatal("observer was never invoked")
	}
	if observed.sawOld.RateLimit.RequestsPerSecond != 100 ||
		observed.sawNew.RateLimit.RequestsPerSecond != 200 {
		t.Fatal("observer received wrong old/new pair")
	}
	if rec.byReason["observer_error"] != 1 {
		t.Fatalf("expected 1 observer_error rollback, counter=%v", rec.byReason)
	}
}

// DP-001: a panicking observer must be recovered, counted as a rollback, and
// leave Current() unchanged — the Reloader may not crash the process.
func TestReloader_ObserverPanicRollsBack(t *testing.T) {
	logger, _ := newTestLogger()
	dir := t.TempDir()
	path := writeTestConfig(t, dir, validConfig)
	initial, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	r := NewReloader(path, initial, logger)
	rec := &countingRecorder{}
	r.SetRollbackRecorder(rec)

	r.RegisterObserver(ObserverFunc(func(old, new *Config) error {
		panic("observer went boom")
	}))

	if err := os.WriteFile(path, []byte(validConfigUpdated), 0644); err != nil {
		t.Fatalf("write updated: %v", err)
	}
	if r.Reload() {
		t.Fatal("Reload must return false when an observer panics")
	}
	if r.Current().RateLimit.RequestsPerSecond != 100 {
		t.Fatal("Current() must hold the pre-reload config after panic")
	}
	if rec.byReason["observer_panic"] != 1 {
		t.Fatalf("expected 1 observer_panic rollback, counter=%v", rec.byReason)
	}
}

// DP-001: observers are invoked in registration order and a later failure
// does NOT revert earlier observers' side effects — only the pointer. This
// test pins the documented "observers must be idempotent" contract.
func TestReloader_ObserverOrderAndPartialApply(t *testing.T) {
	logger, _ := newTestLogger()
	dir := t.TempDir()
	path := writeTestConfig(t, dir, validConfig)
	initial, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	r := NewReloader(path, initial, logger)
	r.SetRollbackRecorder(&countingRecorder{})

	var order []string
	r.RegisterObserver(ObserverFunc(func(old, new *Config) error {
		order = append(order, "a")
		return nil
	}))
	r.RegisterObserver(ObserverFunc(func(old, new *Config) error {
		order = append(order, "b")
		return errors.New("reject at b")
	}))
	r.RegisterObserver(ObserverFunc(func(old, new *Config) error {
		order = append(order, "c")
		return nil
	}))

	if err := os.WriteFile(path, []byte(validConfigUpdated), 0644); err != nil {
		t.Fatalf("write updated: %v", err)
	}
	r.Reload()

	if got := strings.Join(order, ","); got != "a,b" {
		t.Fatalf("observer invocation order = %q, want a,b (c must be skipped)", got)
	}
	if r.Current().RateLimit.RequestsPerSecond != 100 {
		t.Fatal("Current() must revert after partial apply")
	}
}

// DP-001: legacy OnReload callbacks still run, but only after all observers
// accept. If an observer rejects, legacy callbacks must NOT fire.
func TestReloader_LegacyCallbacksSkippedOnRollback(t *testing.T) {
	logger, _ := newTestLogger()
	dir := t.TempDir()
	path := writeTestConfig(t, dir, validConfig)
	initial, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	r := NewReloader(path, initial, logger)
	r.SetRollbackRecorder(&countingRecorder{})

	var legacyCalls int
	r.OnReload(func(*Config) { legacyCalls++ })
	r.RegisterObserver(ObserverFunc(func(old, new *Config) error {
		return errors.New("no")
	}))

	if err := os.WriteFile(path, []byte(validConfigUpdated), 0644); err != nil {
		t.Fatalf("write updated: %v", err)
	}
	r.Reload()

	if legacyCalls != 0 {
		t.Fatalf("legacy callbacks fired on rollback: %d calls", legacyCalls)
	}

	// And a subsequent successful reload must invoke them.
	r = NewReloader(path, initial, logger)
	r.OnReload(func(*Config) { legacyCalls++ })
	r.Reload()
	if legacyCalls != 1 {
		t.Fatalf("legacy callback should have fired once on success, got %d", legacyCalls)
	}
}
