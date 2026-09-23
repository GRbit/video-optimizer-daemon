package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// maxDurationDrift is how far the converted file's duration may differ from the
// original before the result is treated as truncated and the original is kept.
const maxDurationDrift = 10 * time.Second

// minSavingsPercent is the smallest size reduction worth replacing the
// original: every re-encode adds artifacts, and below this the trade is bad.
const minSavingsPercent = 10.0

// converter holds what every conversion needs and nothing about any one
// conversion: per-file state (paths, temp files) lives inside convert.
type converter struct {
	cfg     Config
	encoder encoder
	confirm confirmFunc
}

// convertResult says how a conversion ended when it did not fail. The
// converter does not know about the state file; mapping to an Outcome is the
// caller's.
type convertResult int

const (
	convertReplaced     convertResult = iota // original replaced by the new file
	convertDeclined                          // user said no at a prompt
	convertGainTooSmall                      // encoded, but original kept
)

// convert re-encodes targetPath and puts the result in its place. On error
// the original is always left untouched.
func (c converter) convert(ctx context.Context, targetPath string, facts VideoFacts) (convertResult, error) {
	sizeBefore := fileSize(targetPath)
	slog.Info("Source video", "path", targetPath, "format", facts.Format, "codec", facts.CodecID,
		"resolution", fmt.Sprintf("%dx%d", facts.Width, facts.Height), "bitrate", facts.Bitrate, "size", sizeBefore)

	preset, crf := selectEncoding(c.cfg, facts)
	slog.Info("Selected encoding", "preset", preset, "crf", crf)

	confirmed, err := c.confirm(ctx, fmt.Sprintf("\n--- ACTION REQUIRED ---\nFile to convert: %s\nStart conversion? (y/n): ", targetPath))
	if err != nil {
		return convertReplaced, err
	}
	if !confirmed {
		slog.Info("Conversion declined", "path", targetPath)
		return convertDeclined, nil
	}

	var tempFiles []string
	defer func() { removeTempFiles(tempFiles) }()
	newTemp := func(pattern string) (string, error) {
		path, err := createTempFile(c.cfg.TempDirPath, pattern)
		if err == nil {
			tempFiles = append(tempFiles, path)
		}
		return path, err
	}

	encodedPath, err := newTemp("video_opt_*.mkv")
	if err != nil {
		return convertReplaced, fmt.Errorf("creating video_opt: %w", err)
	}

	slog.Info("Starting HandBrake conversion", "path", targetPath)
	started := time.Now()
	if err := c.encoder.run(ctx, targetPath, encodedPath, preset, crf); err != nil {
		return convertReplaced, fmt.Errorf("run handbrake: %w", err)
	}
	slog.Info("HandBrake finished", "took", time.Since(started).Round(time.Second))

	slog.Debug("Checking audio tracks on converted file", "path", encodedPath)
	encodedInfo, err := getMkvMergeInfo(encodedPath)
	if err != nil {
		return convertReplaced, err
	}
	keepAudio := audioTracksToKeep(encodedInfo)

	sidecars, err := findSidecarFiles(targetPath)
	if err != nil {
		return convertReplaced, fmt.Errorf("find sidecar files: %w", err)
	}
	if len(sidecars) > 0 {
		slog.Info("Sidecar files will be merged", "count", len(sidecars), "files", sidecars)
	}

	finalPath, err := newTemp("video_final_*.mkv")
	if err != nil {
		return convertReplaced, fmt.Errorf("creating video_final: %w", err)
	}

	args := mkvmergeArgs(finalPath, encodedPath, targetPath, keepAudio, sidecars)
	if err := runMkvmerge(ctx, args); err != nil {
		return convertReplaced, fmt.Errorf("mkvmerge final mux: %w", err)
	}
	slog.Debug("Final mux successful", "path", finalPath)

	finalFacts, err := probeVideo(finalPath)
	if err != nil {
		return convertReplaced, fmt.Errorf("probe converted file: %w", err)
	}
	if err := checkDuration(facts, finalFacts); err != nil {
		return convertReplaced, fmt.Errorf("verify converted file: %w", err)
	}
	slog.Debug("Duration check passed", "original_s", facts.Duration, "converted_s", finalFacts.Duration)
	sizeAfter := fileSize(finalPath)
	if !checkSavings(sizeBefore, sizeAfter) {
		slog.Info("Size gain too small, keeping original", "path", targetPath,
			"size_before", sizeBefore, "size_after", sizeAfter,
			"saved_percent", fmt.Sprintf("%.1f", savingsPercent(sizeBefore, sizeAfter)),
			"min_percent", minSavingsPercent)
		return convertGainTooSmall, nil
	}

	confirmed, err = c.confirm(ctx, fmt.Sprintf("\n--- ACTION REQUIRED ---\nOriginal: %s\nOriginal size: %s\nNew File: %s\nNew size: %s\nReplace original file? (y/n): ",
		targetPath, formatNum(sizeBefore), finalPath, formatNum(sizeAfter)))
	if err != nil {
		return convertReplaced, err
	}
	if !confirmed {
		slog.Info("File replacement declined", "path", targetPath)
		return convertDeclined, nil
	}

	if err := replaceOriginal(targetPath, finalPath, sidecars); err != nil {
		return convertReplaced, fmt.Errorf("replace encoded file: %w", err)
	}

	slog.Info("Encoding completed", "path", targetPath, "size_before", sizeBefore, "size_after", sizeAfter,
		"saved_percent", fmt.Sprintf("%.1f", savingsPercent(sizeBefore, sizeAfter)))

	return convertReplaced, nil
}

func savingsPercent(sizeBefore, sizeAfter int64) float64 {
	if sizeBefore <= 0 {
		return 0
	}
	return 100 * float64(sizeBefore-sizeAfter) / float64(sizeBefore)
}

// checkSavings reports whether the converted file is small enough to replace
// the original. An unknown original size counts as "not enough": with no
// number to compare against, keeping the original is the safe choice.
func checkSavings(sizeBefore, sizeAfter int64) bool {
	if sizeBefore <= 0 {
		return false
	}
	return savingsPercent(sizeBefore, sizeAfter) >= minSavingsPercent
}

// findSidecarFiles returns subtitle and audio files that sit next to targetPath
// and share its name prefix. Only sidecarExtensions count: anything else with
// the same prefix (.nfo, .jpg, another video) belongs to someone else.
func findSidecarFiles(targetPath string) ([]string, error) {
	dir := filepath.Dir(targetPath)
	base := filepath.Base(targetPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var sidecars []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == base || !strings.HasPrefix(name, stem) {
			continue
		}
		if !sidecarExtensions[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		sidecars = append(sidecars, filepath.Join(dir, name))
	}
	return sidecars, nil
}

// audioTracksToKeep returns the IDs of audio tracks in the encoded file, keeping
// the first track per language. Video and subtitle IDs are excluded on purpose:
// mkvmerge ignores them in --audio-tracks, but they make the logged command lie.
func audioTracksToKeep(info *MkvMergeOutput) []string {
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

// mkvmergeArgs builds the single mkvmerge invocation that assembles the final
// file: video and audio come from the HandBrake output, everything else
// (subtitles, chapters, attachments such as ASS fonts, tags) from the original,
// and sidecars are appended as extra sources.
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

// h264CodecToken matches "x264", "h264", "h.264" in any case. A trailing digit is
// captured so that "x2640" is left alone. Bare "264" is not a token: it also
// appears in numbers and hashes ("1264", "[1264A3]").
var h264CodecToken = regexp.MustCompile(`(?i)([xh]\.?)264([^0-9]|$)`)

// optimizedFilePath returns the path the converted file is stored at.
//
// Only the file name is rewritten: the directory may legitimately contain
// "264", "aac" or "flac", and touching it would move the file to a path that
// does not exist. The extension is always .mkv because HandBrake runs with
// --format mkv regardless of what the original was called.
func optimizedFilePath(targetPath string) string {
	dir := filepath.Dir(targetPath)
	base := filepath.Base(targetPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))

	if h264CodecToken.MatchString(stem) {
		stem = h264CodecToken.ReplaceAllString(stem, "${1}265${2}")
	} else {
		stem += ".x265"
	}

	stem = replaceCodecToken(stem, "flac", "ogg")
	stem = replaceCodecToken(stem, "aac", "ogg")

	return filepath.Join(dir, stem+".mkv")
}

// replaceCodecToken replaces every occurrence of old that is not surrounded by
// letters, so "Isaac" and "Flacky" survive while "[AAC]" and ".aac5.1" do not.
// The case pattern of the match is copied onto the replacement.
func replaceCodecToken(s, old, new string) string {
	re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(old))

	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringIndex(s, -1) {
		start, end := m[0], m[1]
		before, _ := utf8.DecodeLastRuneInString(s[:start])
		after, _ := utf8.DecodeRuneInString(s[end:])
		if unicode.IsLetter(before) || unicode.IsLetter(after) {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(copyCase(s[start:end], new))
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// copyCase applies the upper/lower pattern of pattern to repl, position by
// position; repl characters past the end of pattern are lowered.
func copyCase(pattern, repl string) string {
	pat := []rune(pattern)
	out := []rune(repl)
	for i, ch := range out {
		if i < len(pat) && unicode.IsUpper(pat[i]) {
			out[i] = unicode.ToUpper(ch)
		} else {
			out[i] = unicode.ToLower(ch)
		}
	}
	return string(out)
}

// replaceOriginal moves finalPath into the library under the optimized name,
// then deletes targetPath and the sidecars that were merged into it.
func replaceOriginal(targetPath, finalPath string, sidecars []string) error {
	newFilePath := optimizedFilePath(targetPath)

	origStat, err := os.Stat(targetPath)
	if err != nil {
		return fmt.Errorf("stat original file: %w", err)
	}

	slog.Info("Replacing original with optimized version", "original", targetPath, "new", newFilePath)

	if err := os.Rename(finalPath, newFilePath); err != nil {
		slog.Debug("Rename failed, attempting copy and delete", "err", err)
		if err := copyFile(finalPath, newFilePath); err != nil {
			return fmt.Errorf("replace original file: %w", err)
		}
		slog.Debug("File copied successfully", "path", newFilePath)
	} else {
		slog.Debug("File renamed successfully", "path", newFilePath)
	}

	// os.CreateTemp hardcodes 0600; a media server running as another user
	// could not read the result. Owner is left alone: chown needs root.
	if err := os.Chmod(newFilePath, origStat.Mode().Perm()); err != nil {
		return fmt.Errorf("copy permissions to new file: %w", err)
	}
	slog.Debug("Applied original permissions", "mode", origStat.Mode().Perm().String(), "path", newFilePath)

	if err := os.Remove(targetPath); err != nil {
		return fmt.Errorf("remove original file: %w", err)
	}
	slog.Debug("Original file removed", "path", targetPath)

	for _, sf := range sidecars {
		if err := os.Remove(sf); err != nil {
			slog.Warn("Failed to remove merged sidecar file", "path", sf, "err", err)
		} else {
			slog.Debug("Removed merged sidecar file", "path", sf)
		}
	}

	return nil
}

func createTempFile(dir, pattern string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("creating tmp file: %w", err)
	}
	closeCloser(f)
	slog.Debug("Temp file created", "path", f.Name())
	return f.Name(), nil
}

func removeTempFiles(paths []string) {
	for _, f := range paths {
		err := os.Remove(f)
		switch {
		case err == nil:
			slog.Debug("Removed temp file", "path", f)
		case os.IsNotExist(err):
			// After a successful rename the temp file already lives at the
			// final path, so its absence here is the normal outcome.
		default:
			slog.Warn("Failed to remove temp file", "path", f, "err", err)
		}
	}
}
