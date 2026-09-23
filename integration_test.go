package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeHandbrake is a stand-in for HandBrakeCLI: it re-encodes the video with
// ffmpeg at a throwaway quality (sources from makeVideo are lossless, so the
// result is far smaller and passes the size-gain guard). FAKE_HB_SECONDS
// truncates the output so the duration guard can be exercised.
const fakeHandbrake = `#!/bin/sh
in=""; out=""; crf=""
while [ $# -gt 0 ]; do
  case "$1" in
    -i) in="$2"; shift ;;
    -o) out="$2"; shift ;;
    -q) crf="$2"; shift ;;
  esac
  shift
done
[ -n "$crf" ] || { echo "missing -q" >&2; exit 3; }
limit=""
if [ -n "$FAKE_HB_SECONDS" ]; then limit="-t $FAKE_HB_SECONDS"; fi
echo "[fake] crf=$crf" >&2
exec ffmpeg -loglevel error -y -i "$in" $limit -c:v libx264 -preset ultrafast -crf 40 -c:a copy -f matroska "$out"
`

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

func makeVideo(t *testing.T, path string, seconds int) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration="+strconv.Itoa(seconds)+":size=128x128:rate=10",
		"-f", "lavfi", "-i", "sine=duration="+strconv.Itoa(seconds),
		"-c:v", "libx264", "-qp", "0", "-c:a", "aac", "-shortest", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
}

func mustProbe(t *testing.T, path string) VideoFacts {
	t.Helper()
	facts, err := probeVideo(path)
	if err != nil {
		t.Fatalf("probeVideo(%s): %v", path, err)
	}
	return facts
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func setupPipeline(t *testing.T) (Config, string) {
	t.Helper()
	requireTools(t, "ffmpeg", "mkvmerge", "mediainfo")

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "HandBrakeCLI"), []byte(fakeHandbrake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	media := t.TempDir()
	tmp := t.TempDir()
	return Config{
		MediaDir:             media,
		TempDirPath:          tmp,
		HandbrakePresetsPath: "/dev/null",
		Preset1080p:          defaultPreset1080p,
		Preset2160p:          defaultPreset2160p,
	}, media
}

func TestPipelineReplacesOriginalAndMergesSidecars(t *testing.T) {
	cfg, media := setupPipeline(t)
	logs := captureLogs(t)

	orig := filepath.Join(media, "Movie.mp4")
	makeVideo(t, orig, 2)
	if err := os.Chmod(orig, 0o664); err != nil {
		t.Fatal(err)
	}
	srt := filepath.Join(media, "Movie.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nhello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(media, "Movie.nfo"))
	touch(t, filepath.Join(media, "Movie 2.mkv"))
	origSize := fileSize(orig)

	task := &VideoConvertTask{cfg: cfg, targetPath: orig, facts: mustProbe(t, orig)}
	entry, err := task.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, logs.String())
	}
	if entry.Outcome != OutcomeDone {
		t.Errorf("entry = %+v, want done", entry)
	}

	result := filepath.Join(media, "Movie.x265.mkv")
	st, err := os.Stat(result)
	if err != nil {
		t.Fatalf("converted file missing: %v", err)
	}
	if st.Mode().Perm() != 0o664 {
		t.Errorf("converted file mode = %v, want 0664 (copied from original)", st.Mode().Perm())
	}
	if st.Size() >= origSize {
		t.Errorf("converted file is %d bytes, original was %d, expected a smaller result", st.Size(), origSize)
	}
	for _, gone := range []string{orig, srt} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should be removed, stat err = %v", gone, err)
		}
	}
	for _, kept := range []string{"Movie.nfo", "Movie 2.mkv"} {
		if _, err := os.Stat(filepath.Join(media, kept)); err != nil {
			t.Errorf("%s must be left alone: %v", kept, err)
		}
	}

	info, err := getMkvMergeInfo(result)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, tr := range info.Tracks {
		types[tr.Type]++
	}
	if types["video"] != 1 || types["audio"] != 1 || types["subtitles"] != 1 {
		t.Errorf("result tracks = %v, want 1 video, 1 audio, 1 subtitles (from sidecar)", types)
	}

	entries, err := os.ReadDir(cfg.TempDirPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("temp dir not cleaned: %v", entries)
	}

	// The CRF reaches HandBrake through -q (the fake echoes it on stderr).
	if out := logs.String(); !strings.Contains(out, "crf=18") {
		t.Errorf("log should show the fake encoder received crf=18:\n%s", out)
	}
}

func TestPipelineKeepsOriginalWhenOutputTruncated(t *testing.T) {
	cfg, media := setupPipeline(t)
	captureLogs(t)
	t.Setenv("FAKE_HB_SECONDS", "2")

	orig := filepath.Join(media, "Long.h264.mp4")
	makeVideo(t, orig, 15)

	task := &VideoConvertTask{cfg: cfg, targetPath: orig, facts: mustProbe(t, orig)}
	_, err := task.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "duration mismatch") {
		t.Fatalf("Run err = %v, want duration mismatch", err)
	}
	if _, err := os.Stat(orig); err != nil {
		t.Errorf("original must survive a failed verification: %v", err)
	}
	if _, err := os.Stat(filepath.Join(media, "Long.h265.mkv")); !os.IsNotExist(err) {
		t.Errorf("truncated result must not be placed into the library, stat err = %v", err)
	}
	entries, err := os.ReadDir(cfg.TempDirPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("temp dir not cleaned: %v", entries)
	}
}

// TestDaemonProcessNext drives the scan -> skip HEVC -> encode -> record loop
// end to end against the state file.
func TestDaemonProcessNext(t *testing.T) {
	cfg, media := setupPipeline(t)
	captureLogs(t)
	// The fake encoder does not produce HEVC, so a freshly written result
	// would be picked up again; the age threshold keeps it out like it keeps
	// out any file written today.
	cfg.MinAge = time.Hour
	cfg.StatePath = filepath.Join(media, stateFileName)

	big := filepath.Join(media, "Big.mp4")
	small := filepath.Join(media, "Small.mp4")
	broken := filepath.Join(media, "Broken.mkv")
	makeVideo(t, big, 3)
	makeVideo(t, small, 1)
	touch(t, broken)
	old := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{big, small, broken} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	state, err := loadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: cfg, state: state}

	processed, err := d.processNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("first pass: processed=%v err=%v", processed, err)
	}
	if e, ok := state.Get(big); !ok || e.Outcome != OutcomeDone {
		t.Errorf("largest file should be done first, entry=%+v ok=%v", e, ok)
	}
	if _, ok := state.Get(small); ok {
		t.Errorf("only one file is encoded per pass")
	}

	processed, err = d.processNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("second pass: processed=%v err=%v", processed, err)
	}
	if e, ok := state.Get(small); !ok || e.Outcome != OutcomeDone {
		t.Errorf("small file should be done on the second pass, entry=%+v ok=%v", e, ok)
	}

	// The garbage file has no video track: mediainfo parses it, AlreadyOptimized
	// says no, and the fake encoder fails on it. The attempt counts as a
	// processed task and is recorded as failed so the next pass skips it.
	processed, err = d.processNext(context.Background())
	if !processed || err == nil {
		t.Errorf("third pass should attempt the broken file and fail: processed=%v err=%v", processed, err)
	}
	if e, ok := state.Get(broken); !ok || e.Outcome != OutcomeFailed || e.Error == "" {
		t.Errorf("broken file should be recorded as failed with a reason, entry=%+v ok=%v", e, ok)
	}

	processed, err = d.processNext(context.Background())
	if processed || err != nil {
		t.Errorf("fourth pass should find nothing: processed=%v err=%v", processed, err)
	}

	reloaded, err := loadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Len() != 3 {
		t.Errorf("state on disk has %d entries, want 3", reloaded.Len())
	}
}
