//go:build windows

package config

// registerSignalHandler is a no-op on Windows since SIGHUP is not available.
// Config reload is still supported via the fsnotify file watcher.
func (r *Reloader) registerSignalHandler() {
	r.logger.Info("SIGHUP not available on Windows, using file watcher only for config reload")
}
