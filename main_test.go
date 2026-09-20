package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyFile is the cross-device fallback that the integration tests never hit
// (temp dir and media dir share a filesystem there), so its partial-file
// cleanup is checked on its own.
func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	payload := []byte(strings.Repeat("video", 10_000))
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("destination content differs from source")
	}

	// A failed copy must not leave a partial destination behind.
	if err := copyFile(filepath.Join(dir, "missing.bin"), filepath.Join(dir, "partial.bin")); err == nil {
		t.Fatal("copyFile from missing source should fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "partial.bin")); !os.IsNotExist(err) {
		t.Errorf("partial destination should not exist, stat err = %v", err)
	}
}
