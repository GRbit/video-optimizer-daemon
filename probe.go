package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// VideoFacts is everything the daemon decides on about a video file. It is
// the whole interface of the probe: the mediainfo JSON schema, the choice of
// which track counts as "the" video track and the string-to-number parsing
// stay inside probeVideo, so every decision sees the same facts.
type VideoFacts struct {
	Format   string
	CodecID  string
	Width    int
	Height   int
	Bitrate  int     // bits per second
	Duration float64 // seconds
}

// mediainfo --Output=JSON reports every value as a string, even numbers.
type mediaInfoTrack struct {
	Type     string `json:"@type"`
	Format   string `json:"Format"`
	CodecID  string `json:"CodecID"`
	Width    string `json:"Width"`
	Height   string `json:"Height"`
	Bitrate  string `json:"Bitrate"`
	Duration string `json:"Duration"`
}

type mediaInfoOutput struct {
	Media struct {
		Tracks []mediaInfoTrack `json:"track"`
	} `json:"media"`
}

// probeVideo runs mediainfo and reduces its output to VideoFacts. The first
// video track is the one that matters: a second one, when present, is cover
// art or a thumbnail stream.
func probeVideo(path string) (VideoFacts, error) {
	var out mediaInfoOutput
	if err := runJSON("mediainfo", []string{"--fullscan", "--Output=JSON", path}, &out); err != nil {
		return VideoFacts{}, err
	}
	// mediainfo exits 0 and prints "media": null for a file it cannot open.
	if len(out.Media.Tracks) == 0 {
		return VideoFacts{}, fmt.Errorf("mediainfo could not read %s", path)
	}

	var facts VideoFacts
	videoSeen := false
	for _, track := range out.Media.Tracks {
		switch {
		case strings.EqualFold(track.Type, "General"):
			facts.Duration, _ = strconv.ParseFloat(track.Duration, 64)
		case strings.EqualFold(track.Type, "Video") && !videoSeen:
			videoSeen = true
			facts.Format = track.Format
			facts.CodecID = track.CodecID
			facts.Width, _ = strconv.Atoi(track.Width)
			facts.Height, _ = strconv.Atoi(track.Height)
			facts.Bitrate, _ = strconv.Atoi(track.Bitrate)
		}
	}
	return facts, nil
}

// AlreadyOptimized reports whether the video is in a codec at least as
// efficient as HEVC, in which case re-encoding it would only lose quality.
func (f VideoFacts) AlreadyOptimized() bool {
	// Substring match on the upper-cased CodecID, so "HEVC" also covers
	// "V_MPEGH/ISO/HEVC" and "MPEG-H/HEVC/h.265", "AV1" covers "V_AV1", etc.
	alreadyOptimizedCodecs := []string{
		"HEVC", "265", "HVC1", "HVC2",
		"AV1", "AV2", "VVC",
		"DVHE", "DVH1",
	}
	codec := strings.ToUpper(f.CodecID)
	for _, skip := range alreadyOptimizedCodecs {
		if strings.Contains(codec, skip) {
			return true
		}
	}
	return strings.EqualFold(f.Format, "HEVC")
}

// checkDuration compares two probes and rejects a converted file whose length
// drifted more than maxDurationDrift from the original: a truncated encode
// still exits 0, and this is the guard before the original is deleted.
func checkDuration(orig, converted VideoFacts) error {
	if orig.Duration <= 0 {
		return errors.New("original: no duration in mediainfo output")
	}
	if converted.Duration <= 0 {
		return errors.New("converted: no duration in mediainfo output")
	}
	drift := absDuration(orig.Duration - converted.Duration)
	if drift > maxDurationDrift.Seconds() {
		return fmt.Errorf("duration mismatch: original %.1fs, converted %.1fs, drift %.1fs exceeds %v",
			orig.Duration, converted.Duration, drift, maxDurationDrift)
	}
	return nil
}

func absDuration(d float64) float64 {
	if d < 0 {
		return -d
	}
	return d
}
