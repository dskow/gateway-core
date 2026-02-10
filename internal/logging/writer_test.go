package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriter_CreateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	rw, err := NewRotatingWriter(path, 1, 3, 30)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	n, err := rw.Write([]byte("hello\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 6 {
		t.Fatalf("Write returned %d, want 6", n)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("file content = %q, want %q", string(data), "hello\n")
	}
}

func TestRotatingWriter_RotatesOnSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// 1 KB max size for easy testing
	rw, err := NewRotatingWriter(path, 0, 3, 30)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	// Override maxBytes directly for a small test
	rw.maxBytes = 100
	defer rw.Close()

	// Write enough to trigger rotation
	data := strings.Repeat("x", 60)
	rw.Write([]byte(data))
	rw.Write([]byte(data)) // should trigger rotation

	// Check that a rotated file exists
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	rotatedCount := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "test-") && strings.HasSuffix(e.Name(), ".log") {
			rotatedCount++
		}
	}
	if rotatedCount < 1 {
		t.Errorf("expected at least 1 rotated file, got %d", rotatedCount)
	}
}

func TestRotatingWriter_MaxBackupsEnforced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	rw, err := NewRotatingWriter(path, 0, 2, 30)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	rw.maxBytes = 50
	defer rw.Close()

	// Force multiple rotations
	data := strings.Repeat("y", 40)
	for i := 0; i < 5; i++ {
		rw.Write([]byte(data))
	}

	// Wait briefly for async cleanup
	// Note: cleanup runs in a goroutine, but for test we do a sync cleanup
	rw.cleanup()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	rotatedCount := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "test-") && strings.HasSuffix(e.Name(), ".log") {
			rotatedCount++
		}
	}
	if rotatedCount > 2 {
		t.Errorf("expected at most 2 rotated files (maxBackups=2), got %d", rotatedCount)
	}
}

func TestRotatingWriter_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "nested", "test.log")

	rw, err := NewRotatingWriter(path, 1, 3, 30)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	rw.Write([]byte("test"))

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("log file was not created")
	}
}
