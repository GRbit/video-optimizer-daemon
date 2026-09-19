package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

func TestDefaultStatePath(t *testing.T) {
	if got, want := defaultStatePath(Config{MediaDir: "/media"}), "/media/"+stateFileName; got != want {
		t.Errorf("directory mode: got %q, want %q", got, want)
	}
	if got, want := defaultStatePath(Config{MediaDir: "/media", MediaListPath: "/lists/todo.txt"}), "/lists/"+stateFileName; got != want {
		t.Errorf("list mode: got %q, want %q", got, want)
	}
}

func TestSelectEncoding(t *testing.T) {
	cfg := Config{Preset1080p: "base-hd", Preset2160p: "base-uhd"}
	cases := []struct {
		name       string
		w, h, bps  string
		wantPreset string
		wantCRF    int
	}{
		{"1080p default", "1920", "1080", "3000000", "base-hd", 20},
		{"1080p high bitrate", "1920", "1080", "13000000", "base-hd", 18},
		{"2160p", "3840", "2160", "3000000", "base-uhd", 21},
		{"2160p clamped", "3840", "2160", "13000000", "base-uhd", 19},
		{"sd low bitrate", "640", "480", "1000000", "base-hd", 20},
		{"tiny", "320", "240", "", "base-hd", 17},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var info MediaInfoOutput
			var tr struct {
				Type     string `json:"@type"`
				Format   string `json:"Format"`
				CodecID  string `json:"CodecID"`
				Language string `json:"Language"`
				Width    string `json:"Width"`
				Height   string `json:"Height"`
				Bitrate  string `json:"Bitrate"`
				Duration string `json:"Duration"`
			}
			tr.Type, tr.Width, tr.Height, tr.Bitrate = "Video", tc.w, tc.h, tc.bps
			info.Media.Tracks = append(info.Media.Tracks, tr)

			preset, crf := selectEncoding(cfg, &info)
			if preset != tc.wantPreset || crf != tc.wantCRF {
				t.Errorf("selectEncoding = (%q, %d), want (%q, %d)", preset, crf, tc.wantPreset, tc.wantCRF)
			}
		})
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

func newTestState(t *testing.T) *State {
	t.Helper()
	s, err := loadState(filepath.Join(t.TempDir(), stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	return s
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

func TestCandidatesFromDirectory(t *testing.T) {
	dir := t.TempDir()
	state := newTestState(t)
	old := time.Now().AddDate(0, -2, 0)
	fresh := time.Now().AddDate(0, 0, -1)

	writeSized(t, filepath.Join(dir, "fresh-huge.mkv"), 3000, fresh)
	writeSized(t, filepath.Join(dir, "old-big.mkv"), 2000, old)
	writeSized(t, filepath.Join(dir, "old-small.mp4"), 1000, old)
	writeSized(t, filepath.Join(dir, "old-not-video.iso"), 5000, old)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(dir, "sub", "old-medium.avi"), 1500, old)

	cfg := Config{MediaDir: dir, MinAge: defaultMinAge}
	got, err := candidatesFromDirectory(context.Background(), cfg, state)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "old-big.mkv"),
		filepath.Join(dir, "sub", "old-medium.avi"),
		filepath.Join(dir, "old-small.mp4"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("candidates sorted by size desc\n got: %v\nwant: %v", got, want)
	}

	state.Record(want[0], StateEntry{Outcome: OutcomeDone})
	got, err = candidatesFromDirectory(context.Background(), cfg, state)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want[1:]) {
		t.Errorf("recorded file must be excluded\n got: %v\nwant: %v", got, want[1:])
	}

	// A file that crossed the age threshold while the daemon was running
	// must become eligible without a restart.
	became := filepath.Join(dir, "fresh-huge.mkv")
	if err := os.Chtimes(became, old, old); err != nil {
		t.Fatal(err)
	}
	got, err = candidatesFromDirectory(context.Background(), cfg, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] != became {
		t.Errorf("file aged past threshold should be first: got %v", got)
	}

	// -min-age is honoured: with a tiny threshold the fresh file counts too.
	cfg.MinAge = time.Second
	got, err = candidatesFromDirectory(context.Background(), cfg, newTestState(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Errorf("with min-age 1s all 4 videos are eligible, got %v", got)
	}
}

func TestCandidatesFromList(t *testing.T) {
	dir := t.TempDir()
	state := newTestState(t)
	first := filepath.Join(dir, "first.mkv")
	second := filepath.Join(dir, "second.mkv")
	writeSized(t, first, 100, time.Now())
	writeSized(t, second, 100, time.Now())

	list := filepath.Join(dir, "list.txt")
	content := first + "\n\n" + second + "\n" + filepath.Join(dir, "missing.mkv") + "\n" + dir + "\n"
	if err := os.WriteFile(list, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{MediaListPath: list}

	got, err := candidatesFromList(context.Background(), cfg, state)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{first, second}; !reflect.DeepEqual(got, want) {
		t.Errorf("list order kept, missing and dirs skipped\n got: %v\nwant: %v", got, want)
	}

	state.Record(first, StateEntry{Outcome: OutcomeDeclined})
	got, err = candidatesFromList(context.Background(), cfg, state)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{second}; !reflect.DeepEqual(got, want) {
		t.Errorf("recorded entry must be skipped\n got: %v\nwant: %v", got, want)
	}
}
