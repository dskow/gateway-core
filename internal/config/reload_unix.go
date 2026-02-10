//go:build !windows

package config

import (
	"os"
	"os/signal"
	"syscall"
)

// registerSignalHandler listens for SIGHUP and triggers a config reload.
func (r *Reloader) registerSignalHandler() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)

	go func() {
		for {
			select {
			case <-sigCh:
				r.logger.Info("SIGHUP received, reloading config")
				r.Reload()
			case <-r.stopCh:
				signal.Stop(sigCh)
				return
			}
		}
	}()

	r.logger.Info("SIGHUP config reload handler registered")
}
