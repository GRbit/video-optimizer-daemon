package main

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// findCandidates returns every file worth looking at, most valuable first, so
// that one tree walk serves a whole run of skips instead of one walk per file.
// Files already present in the state are excluded here; the codec check needs
// mediainfo and is done by the caller.
func findCandidates(ctx context.Context, cfg Config, state *State) ([]string, error) {
	if cfg.MediaListPath != "" {
		return candidatesFromList(ctx, cfg, state)
	}
	return candidatesFromDirectory(ctx, cfg, state)
}

func candidatesFromList(ctx context.Context, cfg Config, state *State) ([]string, error) {
	file, err := os.Open(cfg.MediaListPath)
	if err != nil {
		return nil, fmt.Errorf("open media list: %w", err)
	}
	defer closeCloser(file)

	slog.Debug("Reading media list", "path", cfg.MediaListPath)

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
		if state.Has(path) {
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

func candidatesFromDirectory(ctx context.Context, cfg Config, state *State) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Recomputed per scan: a daemon-wide constant would freeze at start time
	// and newer files would never become eligible.
	threshold := time.Now().Add(-cfg.MinAge)
	slog.Debug("Scanning media directory", "dir", cfg.MediaDir, "modified_before", threshold.Format(time.RFC3339))

	var found []candidate
	err := filepath.WalkDir(cfg.MediaDir, func(path string, d fs.DirEntry, err error) error {
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
		if state.Has(path) {
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

	sort.SliceStable(found, func(i, j int) bool { return found[i].size > found[j].size })

	paths := make([]string, len(found))
	for i, c := range found {
		paths[i] = c.path
	}
	slog.Debug("Media directory scanned", "candidates", len(paths))
	return paths, nil
}
