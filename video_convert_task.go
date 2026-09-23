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

type VideoConvertTask struct {
	cfg        Config
	targetPath string
	facts      VideoFacts
	encoder    encoder
	tempFiles  []string
}

func (t *VideoConvertTask) CleanUp() {
	for _, f := range t.tempFiles {
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

// Run converts the target and returns what to record about it. The caller
// decides what to do with a returned error; it is never "done" then.
func (t *VideoConvertTask) Run(ctx context.Context) (StateEntry, error) {
	sizeBefore := fileSize(t.targetPath)
	slog.Info("Source video", "path", t.targetPath, "format", t.facts.Format, "codec", t.facts.CodecID,
		"resolution", fmt.Sprintf("%dx%d", t.facts.Width, t.facts.Height), "bitrate", t.facts.Bitrate, "size", sizeBefore)

	preset, crf := selectEncoding(t.cfg, t.facts)
	slog.Info("Selected encoding", "preset", preset, "crf", crf)

	if t.cfg.PromptMode {
		fmt.Printf("\n--- ACTION REQUIRED ---\n")
		fmt.Printf("File to convert: %s\n", t.targetPath)
		fmt.Print("Start conversion? (y/n): ")
		confirmed, err := promptConfirm(ctx)
		if err != nil {
			return StateEntry{}, err
		}
		if !confirmed {
			slog.Info("Conversion declined", "path", t.targetPath)
			return StateEntry{Outcome: OutcomeDeclined}, nil
		}
	}

	encodedPath, err := t.createTempFile("video_opt_*.mkv")
	if err != nil {
		return StateEntry{}, fmt.Errorf("creating video_opt: %w", err)
	}

	defer t.CleanUp()

	slog.Info("Starting HandBrake conversion", "path", t.targetPath)
	started := time.Now()
	if err := t.encoder.run(ctx, t.targetPath, encodedPath, preset, crf); err != nil {
		return StateEntry{}, fmt.Errorf("run handbrake: %w", err)
	}
	slog.Info("HandBrake finished", "took", time.Since(started).Round(time.Second))

	slog.Debug("Checking audio tracks on converted file", "path", encodedPath)
	encodedInfo, err := getMkvMergeInfo(encodedPath)
	if err != nil {
		return StateEntry{}, err
	}
	keepAudio := audioTracksToKeep(encodedInfo)

	sidecars, err := findSidecarFiles(t.targetPath)
	if err != nil {
		return StateEntry{}, fmt.Errorf("find sidecar files: %w", err)
	}
	if len(sidecars) > 0 {
		slog.Info("Sidecar files will be merged", "count", len(sidecars), "files", sidecars)
	}

	finalPath, err := t.createTempFile("video_final_*.mkv")
	if err != nil {
		return StateEntry{}, fmt.Errorf("creating video_final: %w", err)
	}

	args := mkvmergeArgs(finalPath, encodedPath, t.targetPath, keepAudio, sidecars)
	if err := runMkvmerge(ctx, args); err != nil {
		return StateEntry{}, fmt.Errorf("mkvmerge final mux: %w", err)
	}
	slog.Debug("Final mux successful", "path", finalPath)

	finalFacts, err := probeVideo(finalPath)
	if err != nil {
		return StateEntry{}, fmt.Errorf("probe converted file: %w", err)
	}
	if err := checkDuration(t.facts, finalFacts); err != nil {
		return StateEntry{}, fmt.Errorf("verify converted file: %w", err)
	}
	slog.Debug("Duration check passed", "original_s", t.facts.Duration, "converted_s", finalFacts.Duration)
	sizeAfter := fileSize(finalPath)
	if !checkSavings(sizeBefore, sizeAfter) {
		slog.Info("Size gain too small, keeping original", "path", t.targetPath,
			"size_before", sizeBefore, "size_after", sizeAfter,
			"saved_percent", fmt.Sprintf("%.1f", savingsPercent(sizeBefore, sizeAfter)),
			"min_percent", minSavingsPercent)
		return StateEntry{Outcome: OutcomeSmallGain}, nil
	}

	if t.cfg.PromptMode {
		fmt.Printf("\n--- ACTION REQUIRED ---\n")
		fmt.Printf("Original: %s\n", t.targetPath)
		fmt.Printf("Original size: %s\n", formatNum(sizeBefore))
		fmt.Printf("New File: %s\n", finalPath)
		fmt.Printf("New size: %s\n", formatNum(sizeAfter))
		fmt.Print("Replace original file? (y/n): ")
		confirmed, err := promptConfirm(ctx)
		if err != nil {
			return StateEntry{}, err
		}
		if !confirmed {
			slog.Info("File replacement declined", "path", t.targetPath)
			return StateEntry{Outcome: OutcomeDeclined}, nil
		}
	}

	if err := t.replaceOriginalWithEncoded(finalPath, sidecars); err != nil {
		return StateEntry{}, fmt.Errorf("replace encoded file: %w", err)
	}

	slog.Info("Encoding completed", "path", t.targetPath, "size_before", sizeBefore, "size_after", sizeAfter,
		"saved_percent", fmt.Sprintf("%.1f", savingsPercent(sizeBefore, sizeAfter)))

	return StateEntry{Outcome: OutcomeDone}, nil
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

func (t *VideoConvertTask) replaceOriginalWithEncoded(finalPath string, sidecars []string) error {
	newFilePath := optimizedFilePath(t.targetPath)

	origStat, err := os.Stat(t.targetPath)
	if err != nil {
		return fmt.Errorf("stat original file: %w", err)
	}

	slog.Info("Replacing original with optimized version", "original", t.targetPath, "new", newFilePath)

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

	if err := os.Remove(t.targetPath); err != nil {
		return fmt.Errorf("remove original file: %w", err)
	}
	slog.Debug("Original file removed", "path", t.targetPath)

	for _, sf := range sidecars {
		if err := os.Remove(sf); err != nil {
			slog.Warn("Failed to remove merged sidecar file", "path", sf, "err", err)
		} else {
			slog.Debug("Removed merged sidecar file", "path", sf)
		}
	}

	return nil
}

func (t *VideoConvertTask) createTempFile(prefix string) (string, error) {
	f, err := os.CreateTemp(t.cfg.TempDirPath, prefix)
	defer closeCloser(f)
	if err != nil {
		return "", fmt.Errorf("creating tmp file: %w", err)
	}
	path := f.Name()
	t.tempFiles = append(t.tempFiles, path)
	slog.Debug("Temp file created", "path", path)
	return path, nil
}
