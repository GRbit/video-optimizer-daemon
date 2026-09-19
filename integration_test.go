package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeHandbrake is a stand-in for HandBrakeCLI: it remuxes the input with
// ffmpeg instead of encoding. FAKE_HB_SECONDS truncates the output so the
// duration guard can be exercised.
const fakeHandbrake = `#!/bin/sh
in=""; out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -i) in="$2"; shift ;;
    -o) out="$2"; shift ;;
  esac
  shift
done
limit=""
if [ -n "$FAKE_HB_SECONDS" ]; then limit="-t $FAKE_HB_SECONDS"; fi
exec ffmpeg -loglevel error -y -i "$in" $limit -c copy -f matroska "$out"
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
		"-f", "lavfi", "-i", "testsrc=duration="+strconv.Itoa(seconds)+":size=64x64:rate=10",
		"-f", "lavfi", "-i", "sine=duration="+strconv.Itoa(seconds),
		"-c:v", "libx264", "-c:a", "aac", "-shortest", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
}

func setupPipeline(t *testing.T) (Config, string) {
	t.Helper()
	requireTools(t, "ffmpeg", "mkvmerge", "mediainfo", "nice")

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "HandBrakeCLI"), []byte(fakeHandbrake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	media := t.TempDir()
	tmp := t.TempDir()
	return Config{MediaDir: media, TempDirPath: tmp, HandbrakePresetsPath: "/dev/null"}, media
}

func TestPipelineReplacesOriginalAndMergesSidecars(t *testing.T) {
	cfg, media := setupPipeline(t)

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

	task := &VideoConvertTask{cfg: cfg, targetPath: orig}
	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	result := filepath.Join(media, "Movie.x265.mkv")
	st, err := os.Stat(result)
	if err != nil {
		t.Fatalf("converted file missing: %v", err)
	}
	if st.Mode().Perm() != 0o664 {
		t.Errorf("converted file mode = %v, want 0664 (copied from original)", st.Mode().Perm())
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
}

func TestPipelineKeepsOriginalWhenOutputTruncated(t *testing.T) {
	cfg, media := setupPipeline(t)
	t.Setenv("FAKE_HB_SECONDS", "2")

	orig := filepath.Join(media, "Long.h264.mp4")
	makeVideo(t, orig, 15)

	task := &VideoConvertTask{cfg: cfg, targetPath: orig}
	err := task.Run(context.Background())
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
