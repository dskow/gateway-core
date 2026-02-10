// Package tlsutil provides TLS certificate loading with automatic reload
// via filesystem notifications for zero-downtime certificate rotation.
package tlsutil

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// CertLoader loads a TLS certificate from disk and watches the cert and key
// files for changes, automatically reloading on rotation. The GetCertificate
// callback is designed for use with tls.Config.GetCertificate.
type CertLoader struct {
	mu       sync.RWMutex
	cert     *tls.Certificate
	certFile string
	keyFile  string
	logger   *slog.Logger
	watcher  *fsnotify.Watcher
	stopCh   chan struct{}
}

// New loads the initial certificate and starts watching both files for changes.
// Returns an error if the initial load fails.
func New(certFile, keyFile string, logger *slog.Logger) (*CertLoader, error) {
	cl := &CertLoader{
		certFile: certFile,
		keyFile:  keyFile,
		logger:   logger,
		stopCh:   make(chan struct{}),
	}

	if err := cl.loadCert(); err != nil {
		return nil, fmt.Errorf("initial certificate load: %w", err)
	}

	// Start file watcher.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating file watcher: %w", err)
	}

	if err := watcher.Add(certFile); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("watching cert file: %w", err)
	}
	if err := watcher.Add(keyFile); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("watching key file: %w", err)
	}

	cl.watcher = watcher
	go cl.watchLoop()

	logger.Info("TLS certificate loaded, watching for changes",
		"cert_file", certFile, "key_file", keyFile)

	return cl, nil
}

// GetCertificate returns the current certificate. This is the callback for
// tls.Config.GetCertificate — it is called on every TLS handshake.
func (cl *CertLoader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	return cl.cert, nil
}

// Reload reloads the cert/key from disk. Exported for manual reload and testing.
func (cl *CertLoader) Reload() error {
	if err := cl.loadCert(); err != nil {
		cl.logger.Error("TLS certificate reload failed, keeping current",
			"error", err, "cert_file", cl.certFile, "key_file", cl.keyFile)
		return err
	}
	cl.logger.Info("TLS certificate reloaded", "cert_file", cl.certFile, "key_file", cl.keyFile)
	return nil
}

// Stop terminates the file watcher.
func (cl *CertLoader) Stop() {
	close(cl.stopCh)
	if cl.watcher != nil {
		cl.watcher.Close()
	}
}

func (cl *CertLoader) loadCert() error {
	cert, err := tls.LoadX509KeyPair(cl.certFile, cl.keyFile)
	if err != nil {
		return err
	}
	cl.mu.Lock()
	cl.cert = &cert
	cl.mu.Unlock()
	return nil
}

func (cl *CertLoader) watchLoop() {
	var debounce *time.Timer

	for {
		select {
		case event, ok := <-cl.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(300*time.Millisecond, func() {
					cl.Reload() //nolint:errcheck
				})
			}
		case err, ok := <-cl.watcher.Errors:
			if !ok {
				return
			}
			cl.logger.Error("TLS cert file watcher error", "error", err)
		case <-cl.stopCh:
			if debounce != nil {
				debounce.Stop()
			}
			return
		}
	}
}
