package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

type Outcome string

const (
	OutcomeDone        Outcome = "done"
	OutcomeSkippedHEVC Outcome = "skipped_hevc"
	OutcomeDeclined    Outcome = "declined"
	OutcomeFailed      Outcome = "failed"
	// OutcomeSmallGain: the encode worked but shrank the file too little to be
	// worth the generation loss, so the original was kept.
	OutcomeSmallGain Outcome = "small_gain"
)

// StateEntry is what the daemon remembers about one file. Entries are never
// expired automatically: a file that is gone from disk is simply ignored, and
// a "declined" entry is the user's exclusion list (delete it by hand to retry).
//
// On disk an entry is a single string, "done" or "failed: <error>", so the
// file stays a flat "path": "outcome" map that is trivial to edit by hand.
type StateEntry struct {
	Outcome Outcome
	Error   string
}

const failedPrefix = string(OutcomeFailed) + ": "

func (e StateEntry) MarshalJSON() ([]byte, error) {
	s := string(e.Outcome)
	if e.Error != "" {
		s += ": " + e.Error
	}
	return json.Marshal(s)
}

func (e *StateEntry) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if rest, ok := strings.CutPrefix(s, failedPrefix); ok {
		*e = StateEntry{Outcome: OutcomeFailed, Error: rest}
		return nil
	}
	*e = StateEntry{Outcome: Outcome(s)}
	return nil
}

// State is the persistent "path -> outcome" journal behind the JSON state
// file. Every Record reaches the disk before it returns: one small JSON write
// per file is nothing next to the mediainfo run that always precedes it, and
// it means there is no flush policy for a caller to get wrong.
type State struct {
	path    string
	entries map[string]StateEntry
}

func loadState(path string) (*State, error) {
	s := &State{path: path, entries: make(map[string]StateEntry)}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		slog.Info("State file does not exist yet, starting empty", "path", path)
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	if err := json.Unmarshal(data, &s.entries); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	slog.Info("State file loaded", "path", path, "entries", len(s.entries))
	return s, nil
}

func (s *State) Has(path string) bool {
	_, ok := s.entries[path]
	return ok
}

// Record stores the entry and rewrites the state file atomically (temp file
// + rename). The in-memory entry stays even if the write fails, so the daemon
// does not retry the file within this run.
func (s *State) Record(path string, e StateEntry) error {
	s.entries[path] = e
	slog.Debug("State entry recorded", "path", path, "outcome", e.Outcome, "error", e.Error)
	return s.write()
}

func (s *State) write() error {
	// Indented so the file can be edited by hand.
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			slog.Warn("Failed to remove temp state file", "path", tmpPath, "err", rmErr)
		}
	}

	if _, err := tmp.Write(data); err != nil {
		closeCloser(tmp)
		cleanup()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		closeCloser(tmp)
		cleanup()
		return fmt.Errorf("sync temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp state file: %w", err)
	}
	// CreateTemp gives 0600; the file is meant to be read and edited by the user.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		cleanup()
		return fmt.Errorf("replace state file: %w", err)
	}

	slog.Debug("State file written", "path", s.path, "entries", len(s.entries))
	return nil
}
