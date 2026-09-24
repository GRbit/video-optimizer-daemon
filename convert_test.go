package main

import (
	"context"
	"os"
	"path/filepath"
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

// Declining at the first prompt must end the conversion before anything is
// encoded; the encoder here would fail loudly if it were reached.
func TestConvertDeclinedBeforeEncoding(t *testing.T) {
	captureLogs(t)
	dir := t.TempDir()
	orig := filepath.Join(dir, "Movie.mkv")
	touch(t, orig)

	asked := 0
	conv := converter{
		tempDir: dir,
		encoder: encoder{presetsPath: "/nonexistent", preset1080p: "p"},
		confirm: func(ctx context.Context, question string) (bool, error) {
			asked++
			return false, nil
		},
	}
	res, err := conv.convert(context.Background(), orig, VideoFacts{Width: 1920, Height: 1080})
	if err != nil || res != convertDeclined {
		t.Errorf("convert = (%v, %v), want (convertDeclined, nil)", res, err)
	}
	if asked != 1 {
		t.Errorf("confirm asked %d times, want 1", asked)
	}
	if _, err := os.Stat(orig); err != nil {
		t.Errorf("declined conversion must leave the original: %v", err)
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
