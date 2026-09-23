package main

import (
	"path/filepath"
	"testing"
)

// Every assumption about mediainfo's JSON (track types, string numbers,
// where the duration lives) is checked here against the real tool, once.
func TestProbeVideo(t *testing.T) {
	requireTools(t, "ffmpeg", "mediainfo")
	captureLogs(t)

	path := filepath.Join(t.TempDir(), "clip.mp4")
	makeVideo(t, path, 2)

	facts, err := probeVideo(path)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Format != "AVC" || facts.CodecID != "avc1" {
		t.Errorf("format/codec = %q/%q, want AVC/avc1", facts.Format, facts.CodecID)
	}
	if facts.Width != 128 || facts.Height != 128 {
		t.Errorf("resolution = %dx%d, want 128x128", facts.Width, facts.Height)
	}
	if facts.Bitrate <= 0 {
		t.Errorf("bitrate = %d, want > 0 (mediainfo --fullscan derives it from stream size)", facts.Bitrate)
	}
	if facts.Duration < 1.9 || facts.Duration > 2.1 {
		t.Errorf("duration = %.3f, want about 2s", facts.Duration)
	}
	if facts.AlreadyOptimized() {
		t.Error("h264 source must not count as already optimized")
	}

	if _, err := probeVideo(filepath.Join(t.TempDir(), "missing.mkv")); err == nil {
		t.Error("probe of a missing file should fail")
	}
}
