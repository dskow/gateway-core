// Package logging provides a rotating file writer for structured log output.
// It implements io.WriteCloser and rotates log files by size, keeping a
// configurable number of backups and removing files older than a maximum age.
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RotatingWriter is an io.WriteCloser that rotates log files by size.
type RotatingWriter struct {
	mu         sync.Mutex
	file       *os.File
	filePath   string
	size       int64
	maxBytes   int64
	maxBackups int
	maxAgeDays int
}

// NewRotatingWriter opens the log file (creating it if needed) and returns a
// writer that rotates when the file exceeds maxBytes. Rotated files are named
// <base>-<timestamp>.log. At most maxBackups rotated files are kept, and files
// older than maxAgeDays are removed.
func NewRotatingWriter(filePath string, maxSizeMB, maxBackups, maxAgeDays int) (*RotatingWriter, error) {
	rw := &RotatingWriter{
		filePath:   filePath,
		maxBytes:   int64(maxSizeMB) * 1024 * 1024,
		maxBackups: maxBackups,
		maxAgeDays: maxAgeDays,
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}

	if err := rw.openFile(); err != nil {
		return nil, err
	}

	return rw, nil
}

func (rw *RotatingWriter) openFile() error {
	f, err := os.OpenFile(rw.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat log file: %w", err)
	}

	rw.file = f
	rw.size = info.Size()
	return nil
}

// Write implements io.Writer. It rotates the file if writing would exceed the
// size limit.
func (rw *RotatingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.size+int64(len(p)) > rw.maxBytes {
		if err := rw.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := rw.file.Write(p)
	rw.size += int64(n)
	return n, err
}

// Close closes the underlying file.
func (rw *RotatingWriter) Close() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.file != nil {
		return rw.file.Close()
	}
	return nil
}

func (rw *RotatingWriter) rotate() error {
	if rw.file != nil {
		rw.file.Close()
	}

	// Rename current file to <base>-<timestamp><ext>
	ext := filepath.Ext(rw.filePath)
	base := strings.TrimSuffix(rw.filePath, ext)
	if ext == "" {
		ext = ".log"
	}
	rotatedName := fmt.Sprintf("%s-%s%s", base, time.Now().Format("20060102-150405"), ext)
	os.Rename(rw.filePath, rotatedName) //nolint:errcheck

	// Open a new file
	if err := rw.openFile(); err != nil {
		return err
	}

	// Cleanup old files in background (non-blocking)
	go rw.cleanup()

	return nil
}

func (rw *RotatingWriter) cleanup() {
	ext := filepath.Ext(rw.filePath)
	base := strings.TrimSuffix(filepath.Base(rw.filePath), ext)
	if ext == "" {
		ext = ".log"
	}
	dir := filepath.Dir(rw.filePath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	// Collect rotated files matching the pattern <base>-<timestamp><ext>
	prefix := base + "-"
	var rotated []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ext) && name != filepath.Base(rw.filePath) {
			rotated = append(rotated, name)
		}
	}

	// Sort ascending (oldest first)
	sort.Strings(rotated)

	cutoff := time.Now().AddDate(0, 0, -rw.maxAgeDays)

	// Remove files exceeding max backups (keep the newest maxBackups)
	for len(rotated) > rw.maxBackups {
		os.Remove(filepath.Join(dir, rotated[0])) //nolint:errcheck
		rotated = rotated[1:]
	}

	// Remove files older than maxAgeDays
	for _, name := range rotated {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(path) //nolint:errcheck
		}
	}
}
