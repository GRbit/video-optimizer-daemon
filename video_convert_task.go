package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
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

type VideoConvertTask struct {
	cfg        Config
	targetPath string
	tempFiles  []string
}

func (t *VideoConvertTask) CleanUp() {
	for _, f := range t.tempFiles {
		err := os.Remove(f)
		switch {
		case err == nil:
			log.Printf("Removed temp file: %s", f)
		case os.IsNotExist(err):
			// After a successful rename the temp file already lives at the
			// final path, so its absence here is the normal outcome.
		default:
			log.Printf("Failed to remove temp file %s: %v", f, err)
		}
	}
}

func (t *VideoConvertTask) Run(ctx context.Context) error {
	origInfo, err := getMediaInfo(t.targetPath)
	if err != nil {
		return fmt.Errorf("get mediainfo: %w", err)
	}

	for _, track := range origInfo.Media.Tracks {
		if strings.EqualFold(track.Type, "video") {
			bitrate := formatStr(track.Bitrate)
			log.Printf("Format: %s, CodecID: %s, %sx%sp %s bps", track.Format, track.CodecID, track.Width, track.Height, bitrate)
			break
		}
	}

	log.Printf("File Size: %s", getFileSize(t.targetPath))

	if isAlreadyOptimized(origInfo) {
		log.Println("File already optimized, skipping.")
		return nil
	}

	preset := selectHandbrakePreset(origInfo)
	log.Printf("Selected Preset: %s", preset)

	if t.cfg.PromptMode {
		fmt.Printf("\n--- ACTION REQUIRED ---\n")
		fmt.Printf("File to convert: %s\n", t.targetPath)
		fmt.Print("Start conversion? (y/n): ")
		confirmed, err := promptConfirm(ctx)
		if err != nil {
			return err
		}
		if !confirmed {
			log.Printf("Conversion was declined")
			return nil
		}
	}

	encodedPath, err := t.createTempFile("video_opt_*.mkv")
	if err != nil {
		return fmt.Errorf("creating video_opt: %w", err)
	}
	log.Println("temp file created:", encodedPath)

	defer t.CleanUp()

	log.Println("Starting HandBrake conversion...")
	if err := runHandbrakeCLI(ctx, t.cfg, t.targetPath, encodedPath, preset); err != nil {
		return fmt.Errorf("run handbrake: %w", err)
	}
	log.Println("HandBrake finished successfully.")

	log.Println("Checking audio tracks on converted file...")
	encodedInfo, err := getMkvMergeInfo(encodedPath)
	if err != nil {
		return err
	}
	keepAudio := audioTracksToKeep(encodedInfo)

	sidecars, err := findSidecarFiles(t.targetPath)
	if err != nil {
		return fmt.Errorf("find sidecar files: %w", err)
	}
	if len(sidecars) > 0 {
		log.Printf("Found %d sidecar file(s) to merge: %v", len(sidecars), sidecars)
	}

	finalPath, err := t.createTempFile("video_final_*.mkv")
	if err != nil {
		return fmt.Errorf("creating video_final: %w", err)
	}

	args := mkvmergeArgs(finalPath, encodedPath, t.targetPath, keepAudio, sidecars)
	log.Println("Running mkvmerge to assemble final file: mkvmerge", args)
	if err := runMkvmerge(ctx, args); err != nil {
		return fmt.Errorf("mkvmerge final mux: %w", err)
	}
	log.Println("Final mux successful:", finalPath)

	finalInfo, err := getMediaInfo(finalPath)
	if err != nil {
		return fmt.Errorf("get mediainfo of converted file: %w", err)
	}
	if err := checkDuration(origInfo, finalInfo); err != nil {
		return fmt.Errorf("verify converted file: %w", err)
	}

	if t.cfg.PromptMode {
		fmt.Printf("\n--- ACTION REQUIRED ---\n")
		fmt.Printf("Original: %s\n", t.targetPath)
		fmt.Printf("Original size: %s\n", getFileSize(t.targetPath))
		fmt.Printf("New File: %s\n", finalPath)
		fmt.Printf("New size: %s\n", getFileSize(finalPath))
		fmt.Print("Replace original file? (y/n): ")
		confirmed, err := promptConfirm(ctx)
		if err != nil {
			return err
		}
		if !confirmed {
			log.Printf("File replacement cancelled")
			return nil
		}
	}

	if err := t.replaceOriginalWithEncoded(finalPath, sidecars); err != nil {
		return fmt.Errorf("replace encoded file: %w", err)
	}

	log.Println("Encoding completed successfully for:", t.targetPath)

	return nil
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
			log.Printf("Duplicate audio language found: %s. Dropping track ID %d.", lang, track.ID)
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

func runMkvmerge(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "mkvmerge", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	// mkvmerge exits with 1 when the output was written but warnings were
	// printed; only 2 means the mux actually failed.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		log.Println("mkvmerge finished with warnings, see output above")
		return nil
	}
	return err
}

func durationSeconds(info *MediaInfoOutput) (float64, error) {
	for _, track := range info.Media.Tracks {
		if strings.EqualFold(track.Type, "General") && track.Duration != "" {
			return strconv.ParseFloat(track.Duration, 64)
		}
	}
	return 0, errors.New("no duration in mediainfo output")
}

// checkDuration is the only guard between "HandBrake exited 0" and deleting the
// original. Size is deliberately not compared: a much smaller file is the goal.
func checkDuration(orig, converted *MediaInfoOutput) error {
	origDur, err := durationSeconds(orig)
	if err != nil {
		return fmt.Errorf("original: %w", err)
	}
	convDur, err := durationSeconds(converted)
	if err != nil {
		return fmt.Errorf("converted: %w", err)
	}

	drift := time.Duration(math.Abs(origDur-convDur) * float64(time.Second))
	if drift > maxDurationDrift {
		return fmt.Errorf("duration mismatch: original %.1fs, converted %.1fs, drift %v exceeds %v",
			origDur, convDur, drift.Round(time.Millisecond), maxDurationDrift)
	}
	log.Printf("Duration check passed: original %.1fs, converted %.1fs", origDur, convDur)
	return nil
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

	log.Printf("Replacing %s with optimized version with name %s", t.targetPath, newFilePath)

	if err := os.Rename(finalPath, newFilePath); err != nil {
		log.Println("Rename failed, attempting copy and delete:", err)
		if err := copyFile(finalPath, newFilePath); err != nil {
			return fmt.Errorf("replace original file: %w", err)
		}
		log.Println("File copied successfully.")
	} else {
		log.Println("File renamed successfully.")
	}

	// os.CreateTemp hardcodes 0600; a media server running as another user
	// could not read the result. Owner is left alone: chown needs root.
	if err := os.Chmod(newFilePath, origStat.Mode().Perm()); err != nil {
		return fmt.Errorf("copy permissions to new file: %w", err)
	}
	log.Printf("Applied original permissions %v to %s", origStat.Mode().Perm(), newFilePath)

	if err := os.Remove(t.targetPath); err != nil {
		return fmt.Errorf("remove original file: %w", err)
	}
	log.Println("Original file removed successfully (", t.targetPath, ")")

	for _, sf := range sidecars {
		if err := os.Remove(sf); err != nil {
			log.Printf("Failed to remove merged sidecar file %s: %v", sf, err)
		} else {
			log.Printf("Removed merged sidecar file: %s", sf)
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
	return path, nil
}
