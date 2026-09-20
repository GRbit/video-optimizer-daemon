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

// flushEvery bounds how many records may be lost on a crash during a long
// scan that skips thousands of already-HEVC files.
const flushEvery = 100

// State is the persistent "path -> outcome" journal behind the JSON state file.
type State struct {
	path    string
	entries map[string]StateEntry
	dirty   int
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

func (s *State) Get(path string) (StateEntry, bool) {
	e, ok := s.entries[path]
	return e, ok
}

func (s *State) Len() int {
	return len(s.entries)
}

func (s *State) Record(path string, e StateEntry) {
	s.entries[path] = e
	s.dirty++
	slog.Debug("State entry recorded", "path", path, "outcome", e.Outcome, "error", e.Error)

	if s.dirty >= flushEvery {
		if err := s.Flush(); err != nil {
			slog.Error("Failed to flush state", "err", err)
		}
	}
}

// Flush writes the state file atomically (temp file + rename) if anything
// changed since the last flush. Indented JSON so the file can be edited by hand.
func (s *State) Flush() error {
	if s.dirty == 0 {
		return nil
	}

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

	slog.Debug("State file written", "path", s.path, "entries", len(s.entries), "changed", s.dirty)
	s.dirty = 0
	return nil
}
