package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindSidecarFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Movie.mkv")
	for _, name := range []string{
		"Movie.mkv", "Movie.srt", "Movie.en.ASS", "Movie.commentary.mka",
		"Movie.nfo", "Movie.jpg", "Movie 2.mkv", "Movie.Extras.mkv", "Other.srt",
	} {
		touch(t, filepath.Join(dir, name))
	}
	if err := os.Mkdir(filepath.Join(dir, "Movie.srt.d"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := findSidecarFiles(target)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "Movie.commentary.mka"),
		filepath.Join(dir, "Movie.en.ASS"),
		filepath.Join(dir, "Movie.srt"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findSidecarFiles\n got: %v\nwant: %v", got, want)
	}
}

func TestAudioTracksToKeep(t *testing.T) {
	var info MkvMergeOutput
	add := func(id int, typ, lang string) {
		var tr struct {
			ID         int    `json:"id"`
			Type       string `json:"type"`
			Codec      string `json:"codec"`
			Properties struct {
				Language        string `json:"language"`
				PixelDimensions string `json:"pixel_dimensions"`
			} `json:"properties"`
		}
		tr.ID, tr.Type, tr.Properties.Language = id, typ, lang
		info.Tracks = append(info.Tracks, tr)
	}
	add(0, "video", "und")
	add(1, "audio", "eng")
	add(2, "audio", "eng")
	add(3, "audio", "rus")
	add(4, "audio", "")
	add(5, "audio", "")
	add(6, "subtitles", "eng")

	got := audioTracksToKeep(&info)
	want := []string{"1", "3", "4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("audioTracksToKeep = %v, want %v (only audio, first per language)", got, want)
	}
}

func TestMkvmergeArgs(t *testing.T) {
	got := mkvmergeArgs("/tmp/final.mkv", "/tmp/hb.mkv", "/media/Movie.mkv",
		[]string{"1", "3"}, []string{"/media/Movie.srt"})
	want := []string{
		"-o", "/tmp/final.mkv",
		"--audio-tracks", "1,3",
		"--no-subtitles", "--no-chapters", "--no-attachments", "/tmp/hb.mkv",
		"--no-video", "--no-audio", "/media/Movie.mkv",
		"/media/Movie.srt",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mkvmergeArgs\n got: %v\nwant: %v", got, want)
	}

	got = mkvmergeArgs("/tmp/final.mkv", "/tmp/hb.mkv", "/media/Movie.mkv", nil, nil)
	for _, a := range got {
		if a == "--audio-tracks" {
			t.Errorf("--audio-tracks must be omitted when there is no audio: %v", got)
		}
	}
}

func mediaInfoWithDuration(d string) *MediaInfoOutput {
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
	tr.Type, tr.Duration = "General", d
	info.Media.Tracks = append(info.Media.Tracks, tr)
	return &info
}

func TestCheckDuration(t *testing.T) {
	cases := []struct {
		name    string
		orig    string
		conv    string
		wantErr bool
	}{
		{"equal", "5400.000", "5400.000", false},
		{"within 10s", "5400.000", "5391.500", false},
		{"truncated", "5400.000", "5389.000", true},
		{"longer", "5400.000", "5420.000", true},
		{"missing converted duration", "5400.000", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDuration(mediaInfoWithDuration(tc.orig), mediaInfoWithDuration(tc.conv))
			if (err != nil) != tc.wantErr {
				t.Errorf("checkDuration(%q, %q) err = %v, wantErr %v", tc.orig, tc.conv, err, tc.wantErr)
			}
		})
	}
}

func TestCheckSavings(t *testing.T) {
	cases := []struct {
		name   string
		before int64
		after  int64
		enough bool
	}{
		{"half size", 1000, 500, true},
		{"exactly 10 percent", 1000, 900, true},
		{"just under 10 percent", 1000, 901, false},
		{"same size", 1000, 1000, false},
		{"grew", 1000, 1200, false},
		{"unknown original size", 0, 500, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkSavings(tc.before, tc.after); got != tc.enough {
				t.Errorf("checkSavings(%d, %d) = %v, want %v", tc.before, tc.after, got, tc.enough)
			}
		})
	}
}

func TestCleanUpIgnoresMissingTempFile(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	dir := t.TempDir()
	present := filepath.Join(dir, "present.mkv")
	touch(t, present)
	missing := filepath.Join(dir, "already-renamed.mkv")

	task := &VideoConvertTask{tempFiles: []string{present, missing}}
	task.CleanUp()

	if _, err := os.Stat(present); !os.IsNotExist(err) {
		t.Errorf("present temp file should be removed, stat err = %v", err)
	}
	if strings.Contains(buf.String(), "Failed") {
		t.Errorf("missing temp file must not be logged as a failure:\n%s", buf.String())
	}
}
