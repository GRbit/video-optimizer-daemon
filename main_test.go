package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultHandbrakeConf(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	if got, want := defaultHandbrakeConf(), "/home/tester/.config/ghb/presets.json"; got != want {
		t.Errorf("defaultHandbrakeConf() = %q, want %q", got, want)
	}
	if strings.Contains(defaultHandbrakeConf(), "$") {
		t.Errorf("default must be expanded, got %q", defaultHandbrakeConf())
	}

	t.Setenv("HOME", "")
	if got := defaultHandbrakeConf(); got != "" {
		t.Errorf("defaultHandbrakeConf() with unknown home = %q, want empty", got)
	}
}

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

func resetProcessed(t *testing.T) {
	t.Helper()
	alreadyProcessedFiles = make(map[string]struct{})
	t.Cleanup(func() { alreadyProcessedFiles = make(map[string]struct{}) })
}

func writeSized(t *testing.T, path string, size int, modTime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func TestFindVideoFromDirectory(t *testing.T) {
	resetProcessed(t)
	dir := t.TempDir()
	old := time.Now().AddDate(0, -2, 0)
	fresh := time.Now().AddDate(0, 0, -1)

	writeSized(t, filepath.Join(dir, "fresh-huge.mkv"), 3000, fresh)
	writeSized(t, filepath.Join(dir, "old-big.mkv"), 2000, old)
	writeSized(t, filepath.Join(dir, "old-small.mp4"), 1000, old)
	writeSized(t, filepath.Join(dir, "old-not-video.iso"), 5000, old)

	cfg := Config{MediaDir: dir}
	got, err := findVideoFromDirectory(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "old-big.mkv"); got != want {
		t.Errorf("largest old video: got %q, want %q", got, want)
	}

	alreadyProcessedFiles[got] = struct{}{}
	got, err = findVideoFromDirectory(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "old-small.mp4"); got != want {
		t.Errorf("after marking processed: got %q, want %q", got, want)
	}

	// A file that crossed the one month threshold while the daemon was running
	// must become eligible without a restart.
	alreadyProcessedFiles[got] = struct{}{}
	became := filepath.Join(dir, "fresh-huge.mkv")
	if err := os.Chtimes(became, old, old); err != nil {
		t.Fatal(err)
	}
	got, err = findVideoFromDirectory(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != became {
		t.Errorf("file aged past threshold: got %q, want %q", got, became)
	}
}

func TestFindVideoFromListSkipsProcessed(t *testing.T) {
	if _, err := exec.LookPath("mediainfo"); err != nil {
		t.Skip("mediainfo not installed")
	}
	resetProcessed(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "first.mkv")
	second := filepath.Join(dir, "second.mkv")
	writeSized(t, first, 100, time.Now())
	writeSized(t, second, 100, time.Now())

	list := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(list, []byte(first+"\n\n"+second+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{MediaListPath: list}

	got, err := findVideoFromList(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Fatalf("first scan: got %q, want %q", got, first)
	}

	alreadyProcessedFiles[first] = struct{}{}
	got, err = findVideoFromList(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != second {
		t.Errorf("processed entry must be skipped: got %q, want %q", got, second)
	}

	alreadyProcessedFiles[second] = struct{}{}
	got, err = findVideoFromList(context.Background(), cfg)
	if err != nil {
		t.Errorf("exhausted list is not an error, got %v", err)
	}
	if got != "" {
		t.Errorf("exhausted list should return no candidate, got %q", got)
	}
}
