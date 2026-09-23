package main

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var validVideoExtensions = func() map[string]struct{} {
	exts := []string{"mkv", "mp4", "avi", "mov", "m4v", "webm", "ts"}
	ret := make(map[string]struct{}, len(exts))
	for _, e := range exts {
		ret["."+e] = struct{}{}
	}
	return ret
}()

type scanSettings struct {
	mediaDir      string
	mediaListPath string // when set, replaces the directory walk
	minAge        time.Duration
}

// findCandidates returns every file worth looking at, most valuable first, so
// that one tree walk serves a whole run of skips instead of one walk per file.
// Files for which skip returns true (already in the state) are excluded here;
// the codec check needs mediainfo and is done by the caller.
func findCandidates(ctx context.Context, s scanSettings, skip func(path string) bool) ([]string, error) {
	if s.mediaListPath != "" {
		return candidatesFromList(ctx, s, skip)
	}
	return candidatesFromDirectory(ctx, s, skip)
}

func candidatesFromList(ctx context.Context, s scanSettings, skip func(path string) bool) ([]string, error) {
	file, err := os.Open(s.mediaListPath)
	if err != nil {
		return nil, fmt.Errorf("open media list: %w", err)
	}
	defer closeCloser(file)

	slog.Debug("Reading media list", "path", s.mediaListPath)

	var candidates []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		path := strings.TrimSpace(scanner.Text())
		if path == "" {
			continue
		}
		if skip(path) {
			slog.Debug("Skipping list entry: already in state", "path", path)
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			slog.Debug("Skipping list entry: stat failed", "path", path, "err", err)
			continue
		}
		if info.IsDir() {
			slog.Debug("Skipping list entry: is a directory", "path", path)
			continue
		}
		candidates = append(candidates, path)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read media list: %w", err)
	}

	slog.Debug("Media list scanned", "candidates", len(candidates))
	return candidates, nil
}

type candidate struct {
	path string
	size int64
}

func candidatesFromDirectory(ctx context.Context, s scanSettings, skip func(path string) bool) ([]string, error) {
	// Recomputed per scan: a daemon-wide constant would freeze at start time
	// and newer files would never become eligible.
	threshold := time.Now().Add(-s.minAge)
	slog.Debug("Scanning media directory", "dir", s.mediaDir, "modified_before", threshold.Format(time.RFC3339))

	var found []candidate
	err := filepath.WalkDir(s.mediaDir, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Debug("Walk error, skipping entry", "path", path, "err", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if _, ok := validVideoExtensions[strings.ToLower(filepath.Ext(path))]; !ok {
			return nil
		}
		if skip(path) {
			slog.Debug("Skipping: already in state", "path", path)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			slog.Debug("Skipping: stat failed", "path", path, "err", err)
			return nil
		}
		if !info.ModTime().Before(threshold) {
			slog.Debug("Skipping: too recent", "path", path, "mod_time", info.ModTime().Format(time.RFC3339))
			return nil
		}
		found = append(found, candidate{path: path, size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortStableFunc(found, func(a, b candidate) int { return cmp.Compare(b.size, a.size) })

	paths := make([]string, 0, len(found))
	for _, c := range found {
		paths = append(paths, c.path)
	}
	slog.Debug("Media directory scanned", "candidates", len(paths))
	return paths, nil
}
