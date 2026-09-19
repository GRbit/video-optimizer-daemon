package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateFileName)

	s, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState on missing file: %v", err)
	}
	if s.Len() != 0 {
		t.Fatalf("fresh state should be empty, has %d entries", s.Len())
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("flush with no changes must not create the file, stat err = %v", err)
	}

	s.Record("/media/a.mkv", StateEntry{Outcome: OutcomeDone, SizeBefore: 100, SizeAfter: 40})
	s.Record("/media/b.mkv", StateEntry{Outcome: OutcomeFailed, Error: "boom"})
	s.Record("/media/c.mkv", StateEntry{Outcome: OutcomeDeclined})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\n  ") {
		t.Errorf("state file should be indented for hand editing:\n%s", raw)
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
	if reloaded.Len() != 3 {
		t.Fatalf("reloaded %d entries, want 3", reloaded.Len())
	}
	a, ok := reloaded.Get("/media/a.mkv")
	if !ok || a.Outcome != OutcomeDone || a.SizeBefore != 100 || a.SizeAfter != 40 || a.Time.IsZero() {
		t.Errorf("entry a = %+v, ok=%v", a, ok)
	}
	b, _ := reloaded.Get("/media/b.mkv")
	if b.Outcome != OutcomeFailed || b.Error != "boom" {
		t.Errorf("entry b = %+v", b)
	}
	if !reloaded.Has("/media/c.mkv") {
		t.Errorf("declined entry must survive a reload")
	}
}

func TestStateRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), stateFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(path); err == nil {
		t.Error("corrupt state file must be an error, not silently replaced with an empty state")
	}
}

func TestStateAutoFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), stateFileName)
	s, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < flushEvery; i++ {
		s.Record("/media/"+time.Duration(i).String()+".mkv", StateEntry{Outcome: OutcomeSkippedHEVC})
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("state should be flushed automatically after %d records: %v", flushEvery, err)
	}
}
