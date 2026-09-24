package main

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
)

// muxFunc assembles the final file: video and audio from the encoder output,
// everything else (subtitles, chapters, attachments such as ASS fonts, tags)
// from the original, plus the sidecars. muxWithMkvmerge is the production
// adapter; tests substitute a closure that just writes a file.
type muxFunc func(ctx context.Context, output, encoded, original string, sidecars []string) error

type mkvMergeTrack struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	Properties struct {
		Language string `json:"language"`
	} `json:"properties"`
}

type mkvMergeOutput struct {
	Tracks []mkvMergeTrack `json:"tracks"`
}

func muxWithMkvmerge(ctx context.Context, output, encoded, original string, sidecars []string) error {
	slog.Debug("Checking audio tracks on encoded file", "path", encoded)
	var info mkvMergeOutput
	if err := runJSON(ctx, mkvmergeBin, []string{"-J", encoded}, &info); err != nil {
		return err
	}

	args := mkvmergeArgs(output, encoded, original, audioTracksToKeep(info), sidecars)
	slog.Debug("Running mkvmerge", "args", args)

	cmd := exec.CommandContext(ctx, mkvmergeBin, args...)
	out, err := cmd.CombinedOutput()

	// mkvmerge exits with 1 when the output was written but warnings were
	// printed; only 2 means the mux actually failed.
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		slog.Debug("mkvmerge output", "output", string(out))
		return nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		slog.Warn("mkvmerge finished with warnings", "output", string(out))
		return nil
	default:
		slog.Error("mkvmerge failed", "err", err, "output", string(out))
		return err
	}
}

// audioTracksToKeep returns the IDs of audio tracks in the encoded file, keeping
// the first track per language. Video and subtitle IDs are excluded on purpose:
// mkvmerge ignores them in --audio-tracks, but they make the logged command lie.
func audioTracksToKeep(info mkvMergeOutput) []string {
	seenLangs := make(map[string]bool)
	var keep []string

	for _, track := range info.Tracks {
		if !strings.EqualFold(track.Type, "audio") {
			continue
		}
		lang := track.Properties.Language
		if lang == "" {
			lang = "und"
		}
		if seenLangs[lang] {
			slog.Info("Dropping duplicate audio track", "language", lang, "track_id", track.ID)
			continue
		}
		seenLangs[lang] = true
		keep = append(keep, strconv.Itoa(track.ID))
	}
	return keep
}

func mkvmergeArgs(output, encoded, original string, keepAudio, sidecars []string) []string {
	args := []string{"-o", output}
	if len(keepAudio) > 0 {
		args = append(args, "--audio-tracks", strings.Join(keepAudio, ","))
	}
	args = append(args, "--no-subtitles", "--no-chapters", "--no-attachments", encoded)
	args = append(args, "--no-video", "--no-audio", original)
	args = append(args, sidecars...)
	return args
}
