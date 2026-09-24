package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOptimizedFilePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "x264 token in name, directory with 264 untouched, extension becomes mkv",
			in:   "/media/x264-rips/Movie.x264.mp4",
			want: "/media/x264-rips/Movie.x265.mkv",
		},
		{
			name: "upper case H264",
			in:   "/media/Movie.H264.mkv",
			want: "/media/Movie.H265.mkv",
		},
		{
			name: "dotted h.264",
			in:   "/media/Movie.h.264.mkv",
			want: "/media/Movie.h.265.mkv",
		},
		{
			name: "264 inside hash is not a codec token",
			in:   "/media/Show [1264A3].mkv",
			want: "/media/Show [1264A3].x265.mkv",
		},
		{
			name: "264 inside number is not a codec token",
			in:   "/media/Movie 1264.mp4",
			want: "/media/Movie 1264.x265.mkv",
		},
		{
			name: "x264 followed by digit is not a codec token",
			in:   "/media/Movie.x2640.mp4",
			want: "/media/Movie.x2640.x265.mkv",
		},
		{
			name: "no codec token gets x265 suffix",
			in:   "/media/Movie.avi",
			want: "/media/Movie.x265.mkv",
		},
		{
			name: "aac in directory name untouched",
			in:   "/media/Isaac Asimov/Movie.mkv",
			want: "/media/Isaac Asimov/Movie.x265.mkv",
		},
		{
			name: "flac in directory name untouched",
			in:   "/media/flac-rips/Movie.mkv",
			want: "/media/flac-rips/Movie.x265.mkv",
		},
		{
			name: "audio codec in file name replaced with case preserved",
			in:   "/media/Movie.FLAC.x264.mkv",
			want: "/media/Movie.OGG.x265.mkv",
		},
		{
			name: "aac in file name replaced",
			in:   "/media/Movie.aac.mp4",
			want: "/media/Movie.ogg.x265.mkv",
		},
		{
			name: "aac inside a word is not a codec token",
			in:   "/media/Isaac Asimov.mkv",
			want: "/media/Isaac Asimov.x265.mkv",
		},
		{
			name: "flac followed by letters is not a codec token",
			in:   "/media/Flacky.flac.mkv",
			want: "/media/Flacky.ogg.x265.mkv",
		},
		{
			name: "codec token in brackets",
			in:   "/media/Movie [AAC].mkv",
			want: "/media/Movie [OGG].x265.mkv",
		},
		{
			name: "codec token followed by channel count",
			in:   "/media/Movie.aac5.1.mkv",
			want: "/media/Movie.ogg5.1.x265.mkv",
		},
		{
			name: "several codec tokens are all replaced",
			in:   "/media/Movie.aac.flac.mkv",
			want: "/media/Movie.ogg.ogg.x265.mkv",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := optimizedFilePath(tc.in)
			if got != tc.want {
				t.Errorf("optimizedFilePath(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// Merged sidecars are deleted, so a false positive here loses another file's
// subtitles for good. Every row is a real naming convention seen in the wild.
func TestIsSidecarOf(t *testing.T) {
	cases := []struct {
		stem string
		name string
		want bool
	}{
		{"Episode 1", "Episode 1.srt", true},
		{"Episode 1", "Episode 1.en.srt", true},
		{"Episode 1", "Episode 1.eng.forced.srt", true},
		{"Episode 1", "Episode 1 [en].srt", true},
		{"Episode 1", "Episode 1 [en] [forced].srt", true},
		{"Episode 1", "Episode 1.ru.default.ASS", true},
		{"Episode 1", "Episode 1-en.mka", true},
		{"Episode 1", "Episode 1.mkv", false}, // the video itself
		{"Episode 1", "Episode 10.srt", false},
		{"Episode 1", "Episode 1.5.srt", false},
		{"Episode 1", "Episode 1 Extended.srt", false},
		{"Episode 1", "Episode 1.nfo", false},
		{"Episode 1", "Other.srt", false},
		{"Movie", "Movie 2.srt", false},
		{"Movie 1", "Movie 1 Cut.srt", true}, // accepted hole: 3 letters look like a language code
		{"Show.S01E01.1080p", "Show.S01E01.1080p.srt", true},
		{"Show.S01E01.1080p", "Show.S01E01.1080p.rus.forced.srt", true},
		{"Show.S01E01.1080p", "Show.S01E010.1080p.srt", false},
		{"Show.S01E01.1080p", "Show.S01E01.720p.srt", false},
	}
	for _, tc := range cases {
		if got := isSidecarOf(tc.stem, tc.name); got != tc.want {
			t.Errorf("isSidecarOf(%q, %q) = %v, want %v", tc.stem, tc.name, got, tc.want)
		}
	}
}

// fakeConverter wires closures for every seam: encode writes encodedSize
// bytes, mux copies the encoded file, probe reports the given duration. The
// converter's policy (confirm, encode, mux, verify, savings, confirm, replace)
// runs for real on real files in a temp dir, without any external tool.
type fakeConverter struct {
	conv     converter
	encoded  int
	answers  []bool
	asked    int
	encodes  int
	duration float64
}

func newFakeConverter(t *testing.T, tempDir string) *fakeConverter {
	f := &fakeConverter{duration: 100}
	f.conv = converter{
		tempDir: tempDir,
		encode: func(ctx context.Context, facts VideoFacts, input, output string) error {
			f.encodes++
			return os.WriteFile(output, make([]byte, f.encoded), 0o600)
		},
		mux: func(ctx context.Context, output, encoded, original string, sidecars []string) error {
			return copyFile(encoded, output)
		},
		probe: func(ctx context.Context, path string) (VideoFacts, error) {
			return VideoFacts{Duration: f.duration}, nil
		},
		confirm: func(ctx context.Context, question string) (bool, error) {
			f.asked++
			if f.asked > len(f.answers) {
				t.Fatalf("confirm asked %d times, only %d answers prepared", f.asked, len(f.answers))
			}
			return f.answers[f.asked-1], nil
		},
	}
	return f
}

func TestConvertPolicy(t *testing.T) {
	captureLogs(t)
	facts := VideoFacts{Width: 1920, Height: 1080, Duration: 100}

	setup := func(t *testing.T) (dir, orig string) {
		dir = t.TempDir()
		orig = filepath.Join(dir, "Movie.mkv")
		if err := os.WriteFile(orig, make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir, orig
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	tempClean := func(t *testing.T, dir string) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "video_") {
				t.Errorf("temp file left behind: %s", e.Name())
			}
		}
	}

	t.Run("declined before encoding", func(t *testing.T) {
		dir, orig := setup(t)
		f := newFakeConverter(t, dir)
		f.answers = []bool{false}
		res, err := f.conv.convert(context.Background(), orig, facts)
		if err != nil || res != convertDeclined {
			t.Errorf("convert = (%v, %v), want (convertDeclined, nil)", res, err)
		}
		if f.encodes != 0 {
			t.Errorf("nothing must be encoded after a decline, encodes=%d", f.encodes)
		}
		if !exists(orig) {
			t.Error("original must stay")
		}
	})

	t.Run("gain too small keeps original", func(t *testing.T) {
		dir, orig := setup(t)
		f := newFakeConverter(t, dir)
		f.answers = []bool{true}
		f.encoded = 950
		res, err := f.conv.convert(context.Background(), orig, facts)
		if err != nil || res != convertGainTooSmall {
			t.Errorf("convert = (%v, %v), want (convertGainTooSmall, nil)", res, err)
		}
		if f.asked != 1 {
			t.Errorf("no replacement prompt when the original is kept, asked=%d", f.asked)
		}
		if !exists(orig) || exists(filepath.Join(dir, "Movie.x265.mkv")) {
			t.Error("original must stay and no new file may appear")
		}
		tempClean(t, dir)
	})

	t.Run("declined at replacement", func(t *testing.T) {
		dir, orig := setup(t)
		f := newFakeConverter(t, dir)
		f.answers = []bool{true, false}
		f.encoded = 100
		res, err := f.conv.convert(context.Background(), orig, facts)
		if err != nil || res != convertDeclined {
			t.Errorf("convert = (%v, %v), want (convertDeclined, nil)", res, err)
		}
		if !exists(orig) || exists(filepath.Join(dir, "Movie.x265.mkv")) {
			t.Error("original must stay and no new file may appear")
		}
		tempClean(t, dir)
	})

	t.Run("truncated output fails before replacement", func(t *testing.T) {
		dir, orig := setup(t)
		f := newFakeConverter(t, dir)
		f.answers = []bool{true}
		f.encoded = 100
		f.duration = 80
		_, err := f.conv.convert(context.Background(), orig, facts)
		if err == nil || !strings.Contains(err.Error(), "duration mismatch") {
			t.Errorf("err = %v, want duration mismatch", err)
		}
		if !exists(orig) {
			t.Error("original must stay")
		}
		tempClean(t, dir)
	})

	t.Run("replaced", func(t *testing.T) {
		dir, orig := setup(t)
		f := newFakeConverter(t, dir)
		f.answers = []bool{true, true}
		f.encoded = 100
		res, err := f.conv.convert(context.Background(), orig, facts)
		if err != nil || res != convertReplaced {
			t.Errorf("convert = (%v, %v), want (convertReplaced, nil)", res, err)
		}
		if exists(orig) {
			t.Error("original must be gone")
		}
		if st, err := os.Stat(filepath.Join(dir, "Movie.x265.mkv")); err != nil || st.Size() != 100 {
			t.Errorf("new file missing or wrong size: %v", err)
		}
		tempClean(t, dir)
	})
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
