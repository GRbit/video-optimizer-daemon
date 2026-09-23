package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readStateFile checks the state the way the user does: by reading the JSON
// file, not through any accessor on State.
func readStateFile(t *testing.T, path string) map[string]StateEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	entries := map[string]StateEntry{}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("parse state file: %v\n%s", err, raw)
	}
	return entries
}

// The state file is edited by hand, so its on-disk shape is part of the
// contract: flat "path": "outcome" map, "failed: <error>" for failures,
// written on every Record.
func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateFileName)

	s, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState on missing file: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("loading must not create the file, stat err = %v", err)
	}

	if err := s.Record("/media/a.mkv", StateEntry{Outcome: OutcomeDone}); err != nil {
		t.Fatal(err)
	}
	if got := readStateFile(t, path); len(got) != 1 || got["/media/a.mkv"].Outcome != OutcomeDone {
		t.Errorf("first Record must already be on disk, got %v", got)
	}
	if err := s.Record("/media/b.mkv", StateEntry{Outcome: OutcomeFailed, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("/media/c.mkv", StateEntry{Outcome: OutcomeDeclined}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\n  ") {
		t.Errorf("state file should be indented for hand editing:\n%s", raw)
	}
	for _, want := range []string{`"/media/a.mkv": "done"`, `"/media/b.mkv": "failed: boom"`, `"/media/c.mkv": "declined"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state file should contain %s:\n%s", want, raw)
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("state file mode = %v, want 0644", st.Mode().Perm())
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}

	reloaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/media/a.mkv", "/media/b.mkv", "/media/c.mkv"} {
		if !reloaded.Has(p) {
			t.Errorf("%s must survive a reload", p)
		}
	}
	if got := readStateFile(t, path)["/media/b.mkv"]; got != (StateEntry{Outcome: OutcomeFailed, Error: "boom"}) {
		t.Errorf("failed entry parsed back as %+v", got)
	}
}
