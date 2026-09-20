package main

import (
	"os"
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
