package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type VideoConvertTask struct {
	cfg          Config
	targetPath   string
	finalPath    string
	tempFiles    []string
	sidecarFiles []string
	completed    bool
}

func (t *VideoConvertTask) CleanUp() {
	for _, f := range t.tempFiles {
		if _, err := os.Stat(f); err == nil {
			if err := os.Remove(f); err != nil {
				log.Printf("Failed to remove temp file %s: %v", f, err)
			} else {
				log.Printf("Removed temp file: %s", f)
			}
		} else {
			log.Printf("Failed to stat temp file %s for cleanup: %v", f, err)
		}
	}

	if t.completed {
		for _, sf := range t.sidecarFiles {
			if err := os.Remove(sf); err != nil {
				log.Printf("Failed to remove sidecar file %s: %v", sf, err)
			} else {
				log.Printf("Removed sidecar file: %s", sf)
			}
		}
	}
}

func (t *VideoConvertTask) Run(ctx context.Context) error {
	mediaInfo, err := getMediaInfo(t.targetPath)
	if err != nil {
		return fmt.Errorf("get mediainfo: %w", err)
	}

	for _, track := range mediaInfo.Media.Tracks {
		if strings.EqualFold(track.Type, "video") {
			bitrate := formatStr(track.Bitrate)
			log.Printf("Format: %s, CodecID: %s, %sx%sp %s bps", track.Format, track.CodecID, track.Width, track.Height, bitrate)
			break
		}
	}

	log.Printf("File Size: %s", getFileSize(t.targetPath))

	if isAlreadyOptimized(mediaInfo) {
		log.Println("File already optimized, skipping.")
		alreadyProcessedFiles[t.targetPath] = struct{}{}
		return nil
	}

	preset := selectHandbrakePreset(mediaInfo)
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
			alreadyProcessedFiles[t.targetPath] = struct{}{}
			return nil
		}
	}

	if err := t.createTempFile("video_opt_*.mkv"); err != nil {
		return fmt.Errorf("creating video_opt: %w", err)
	}
	log.Println("temp file created:", t.tempFiles[0])

	defer t.CleanUp()

	log.Println("Starting HandBrake conversion...")
	err = runHandbrakeCLI(ctx, t.cfg, t.targetPath, t.tempFiles[0], preset)
	if err != nil {
		return fmt.Errorf("run handbrake: %w", err)
	}
	log.Println("HandBrake finished successfully.")

	log.Println("Checking audio tracks on converted file...")
	if err := t.deduplicateAudioTracks(ctx); err != nil {
		return fmt.Errorf("deduplicating audio tracks: %w", err)
	}

	if t.cfg.PromptMode {
		fmt.Printf("\n--- ACTION REQUIRED ---\n")
		fmt.Printf("Original: %s\n", t.targetPath)
		fmt.Printf("Original size: %s\n", getFileSize(t.targetPath))
		fmt.Printf("New File: %s\n", t.finalPath)
		fmt.Printf("New size: %s\n", getFileSize(t.finalPath))
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

	if err := t.mergeSidecarFiles(ctx); err != nil {
		return fmt.Errorf("merge subtitles and sound: %w", err)
	}

	if err := t.replaceOriginalWithEncoded(); err != nil {
		return fmt.Errorf("replace encoded file: %w", err)
	}

	t.completed = true
	log.Println("Encoding completed successfully for:", t.targetPath)

	return nil
}

func (t *VideoConvertTask) mergeSidecarFiles(ctx context.Context) error {
	dir := filepath.Dir(t.targetPath)
	origBase := strings.TrimSuffix(filepath.Base(t.targetPath), filepath.Ext(t.targetPath))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read directory for sidecar files: %w", err)
	}

	var sidecarFiles []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, origBase) {
			continue
		}
		if !sidecarExtensions[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		sidecarFiles = append(sidecarFiles, filepath.Join(dir, name))
	}

	if len(sidecarFiles) == 0 {
		log.Println("No sidecar subtitle/audio files found, skipping merge.")
		return nil
	}

	log.Printf("Found %d sidecar file(s) to merge: %v", len(sidecarFiles), sidecarFiles)
	t.sidecarFiles = sidecarFiles

	if err := t.createTempFile("video_merged_*.mkv"); err != nil {
		return fmt.Errorf("creating video_merged_: %w", err)
	}
	mergedPath := t.tempFiles[len(t.tempFiles)-1]

	args := []string{"-o", mergedPath, t.finalPath}
	for _, sf := range sidecarFiles {
		args = append(args, sf)
	}

	log.Println("Running mkvmerge to merge sidecars: mkvmerge", args)

	cmd := exec.CommandContext(ctx, "mkvmerge", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkvmerge sidecar merge: %w", err)
	}

	log.Println("Sidecar merge successful.")
	t.finalPath = mergedPath
	return nil
}

func (t *VideoConvertTask) replaceOriginalWithEncoded() error {
	var newFilePath string
	if strings.Contains(strings.ToLower(t.targetPath), "264") {
		newFilePath = strings.ReplaceAll(t.targetPath, "264", "265")
	} else {
		newFilePath = strings.TrimSuffix(t.targetPath, filepath.Ext(t.targetPath)) + ".x265.mkv"
	}

	newFilePath = replaceCasePreserving(newFilePath, "flac", "ogg")
	newFilePath = replaceCasePreserving(newFilePath, "aac", "ogg")

	log.Printf("Replacing %s with optimized version with name %s", t.targetPath, newFilePath)

	err := os.Rename(t.finalPath, newFilePath)
	if err != nil {
		log.Println("Rename failed, attempting copy and delete:", err)
		err = copyFile(t.finalPath, newFilePath)
		if err != nil {
			return fmt.Errorf("replace original file: %w", err)
		}
		log.Println("File copied successfully.")
	} else {
		log.Println("File renamed successfully.")
	}

	err = os.Remove(t.targetPath)
	if err != nil {
		return fmt.Errorf("remove original file: %w", err)
	}

	log.Println("Original file removed successfully (", t.targetPath, ")")

	origBase := strings.TrimSuffix(filepath.Base(t.targetPath), filepath.Ext(t.targetPath))
	newBase := strings.TrimSuffix(filepath.Base(newFilePath), filepath.Ext(newFilePath))
	dir := filepath.Dir(t.targetPath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read directory for sibling rename: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, origBase) {
			continue
		}
		oldPath := filepath.Join(dir, name)
		if oldPath == t.targetPath || oldPath == newFilePath {
			continue
		}
		suffix := name[len(origBase):]
		newPath := filepath.Join(dir, newBase+suffix)
		if err := os.Rename(oldPath, newPath); err != nil {
			log.Printf("Failed to rename sibling file %s: %v", oldPath, err)
		} else {
			log.Printf("Renamed sibling file %s to %s", oldPath, newPath)
		}
	}

	return nil
}

func (t *VideoConvertTask) deduplicateAudioTracks(ctx context.Context) error {
	info, err := getMkvMergeInfo(t.finalPath)
	if err != nil {
		return err
	}

	seenLangs := make(map[string]bool)
	var keepTrackIDs []string
	needsRemux := false

	for _, track := range info.Tracks {
		if strings.EqualFold(track.Type, "audio") {
			lang := track.Properties.Language
			if lang == "" {
				lang = "und"
			}

			if seenLangs[lang] {
				needsRemux = true
				log.Printf("Duplicate audio language found: %s. Dropping track ID %d.", lang, track.ID)
			} else {
				seenLangs[lang] = true
				keepTrackIDs = append(keepTrackIDs, strconv.Itoa(track.ID))
			}
		} else {
			keepTrackIDs = append(keepTrackIDs, strconv.Itoa(track.ID))
		}
	}

	if !needsRemux {
		log.Println("Audio tracks are optimal. No remuxing needed.")
		return nil
	}

	log.Println("Remuxing to remove duplicate audio tracks...")

	if err := t.createTempFile("video_remux_*.mkv"); err != nil {
		return fmt.Errorf("creating video_remux: %w", err)
	}
	remuxPath := t.tempFiles[len(t.tempFiles)-1]

	args := []string{
		"-o", remuxPath,
		"--audio-tracks", strings.Join(keepTrackIDs, ","),
		t.finalPath,
	}

	log.Println("Running mkvmerge with args: mkvmerge", args)

	cmd := exec.CommandContext(ctx, "mkvmerge", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("remux failed: %w", err)
	}

	log.Println("Audio deduplication mkvmerge remux successful, updated file is:", remuxPath)

	t.finalPath = remuxPath
	return nil
}

func (t *VideoConvertTask) createTempFile(prefix string) error {
	f, err := os.CreateTemp(t.cfg.TempDirPath, prefix)
	defer closeCloser(f)
	if err != nil {
		return fmt.Errorf("creating tmp file: %w", err)
	}
	path := f.Name()
	t.tempFiles = append(t.tempFiles, path)
	return nil
}
