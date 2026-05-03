package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const (
	envMediaDir      = "MEDIA_DIR"
	envHandbrakeConf = "HANDBRAKE_CONF"
	envPromptMode    = "PROMPT_MODE"
	envMediaList     = "MEDIA_LIST_PATH"

	defaultMediaDir      = "/media"
	defaultHandbrakeConf = "/root/.config/ghb/presets.json"
)

var (
	oldestAllowedModTime = time.Now().AddDate(0, -1, 0) // 1 month ago
	validVideoExtensions = func() map[string]struct{} {
		exts := []string{"mkv", "mp4", "avi", "mov", "m4v", "webm", "ts"}
		ret := make(map[string]struct{}, len(exts))
		for _, e := range exts {
			ret["."+e] = struct{}{}
		}
		return ret
	}()
	alreadyProcessedFiles = make(map[string]struct{})
	tempDirectory         string

	// sidecarExtensions define list of file extensions that should be merged into the final output if they exist alongside the original video file
	sidecarExtensions = map[string]bool{
		".ass": true,
		".srt": true,
		".mka": true,
	}
)

type Config struct {
	PromptMode           bool
	MediaDir             string
	MediaListPath        string
	HandbrakePresetsPath string
}

type MediaInfoOutput struct {
	Media struct {
		Tracks []struct {
			Type     string `json:"@type"`
			Format   string `json:"Format"`
			CodecID  string `json:"CodecID"`
			Language string `json:"Language"`
			Width    string `json:"Width"`
			Height   string `json:"Height"`
			Bitrate  string `json:"Bitrate"`
		} `json:"track"`
	} `json:"media"`
}

type MkvMergeOutput struct {
	Tracks []struct {
		ID         int    `json:"id"`
		Type       string `json:"type"`
		Codec      string `json:"codec"`
		Properties struct {
			Language        string `json:"language"`
			PixelDimensions string `json:"pixel_dimensions"`
		} `json:"properties"`
	} `json:"tracks"`
}

func main() {
	promptPtr := flag.Bool("prompt", false, "Ask for confirmation before replacing original files")
	mediaDirPtr := flag.String("media-dir", defaultMediaDir, "Directory to scan for media files")
	handbrakeConfPtr := flag.String("handbrake-conf", defaultHandbrakeConf, "Path to HandBrake presets JSON file")
	mediaListPtr := flag.String("media-list", "", "Path to media list file (not used currently)")
	tmpDirPtr := flag.String("tmp", os.TempDir(), "Directory to use for temporary files")
	flag.Parse()

	tempDirectory = *tmpDirPtr

	cfg := Config{
		PromptMode:           *promptPtr,
		MediaDir:             *mediaDirPtr,
		MediaListPath:        *mediaListPtr,
		HandbrakePresetsPath: *handbrakeConfPtr,
	}
	if os.Getenv(envMediaDir) != "" {
		cfg.MediaDir = os.Getenv(envMediaDir)
	}
	if os.Getenv(envHandbrakeConf) != "" {
		cfg.HandbrakePresetsPath = os.Getenv(envHandbrakeConf)
	}
	if os.Getenv(envPromptMode) != "" && cfg.PromptMode == false {
		envVal, _ := strconv.ParseBool(os.Getenv(envPromptMode))
		cfg.PromptMode = envVal
	}
	if os.Getenv(envMediaList) != "" {
		cfg.MediaListPath = os.Getenv(envMediaList)
	}

	log.Println("Starting Video Optimizer Daemon...")
	log.Println("Configuration: ", cfg)

	ctx, cancel := context.WithCancel(context.Background())

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Println("Shutting down...")
		cancel()
	}()

	ticker := time.NewTicker(time.Nanosecond)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := processVideoFile(ctx, cfg); err != nil {
				log.Println("Error during encoding: ", err)
				os.Exit(1)
				ticker.Reset(time.Hour)
			}
		}
	}
}

func processVideoFile(ctx context.Context, cfg Config) error {
	targetFile, err := findTargetVideoFile(ctx, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("search files: %w", err)
	}

	if targetFile == "" {
		return fmt.Errorf("no matching files found (older than %v)", time.Since(oldestAllowedModTime))
	}

	log.Printf("Found target candidate: %s", targetFile)

	mediaInfo, err := getMediaInfo(targetFile)
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

	log.Printf("File Size: %s", getFileSize(targetFile))

	if isAlreadyOptimized(mediaInfo) {
		log.Println("File already optimized, skipping.")
		alreadyProcessedFiles[targetFile] = struct{}{}
		return nil
	}

	preset := selectHandbrakePreset(mediaInfo)
	log.Printf("Selected Preset: %s", preset)

	if cfg.PromptMode {
		fmt.Printf("\n--- ACTION REQUIRED ---\n")
		fmt.Printf("File to convert: %s\n", targetFile)
		fmt.Print("Start conversion? (y/n): ")

		reader := bufio.NewReader(os.Stdin)

		var (
			response string
			errCh    = make(chan error)
		)
		go func() {
			var err error
			response, err = reader.ReadString('\n')
			errCh <- err
		}()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("reading user input: %w", err)
			}
		}

		response = strings.TrimSpace(strings.ToLower(response))

		if response != "y" && response != "yes" {
			log.Printf("Conversion was declined")
			alreadyProcessedFiles[targetFile] = struct{}{}
			return nil
		}
	}

	encodedFile, err := os.CreateTemp(tempDirectory, "video_opt_*.mkv")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempFilePath := encodedFile.Name()
	encodedFile.Close()

	log.Println("temp file created:", tempFilePath)

	tempFiles := []string{tempFilePath}

	defer func() {
		for _, f := range tempFiles {
			if _, err := os.Stat(f); err == nil {
				log.Printf("Cleaning up temp file: %s", f)
				os.Remove(f)
			}
		}
	}()

	log.Println("Starting HandBrake conversion...")
	err = runHandbrakeCLI(ctx, cfg, targetFile, tempFilePath, preset)
	if err != nil {
		return fmt.Errorf("run handbrake: %w", err)
	}
	log.Println("HandBrake finished successfully.")

	log.Println("Checking audio tracks on converted file...")
	finalPath, err := deduplicateAudioTracks(tempFilePath, &tempFiles)
	if err != nil {
		return fmt.Errorf("process audio tracks: %w", err)
	}

	if cfg.PromptMode {
		fmt.Printf("\n--- ACTION REQUIRED ---\n")
		fmt.Printf("Original: %s\n", targetFile)
		fmt.Printf("Original size: %s\n", getFileSize(targetFile))
		fmt.Printf("New File: %s\n", finalPath)
		fmt.Printf("New size: %s\n", getFileSize(finalPath))
		fmt.Print("Replace original file? (y/n): ")

		reader := bufio.NewReader(os.Stdin)

		var (
			response string
			errCh    = make(chan error)
		)
		go func() {
			var err error
			response, err = reader.ReadString('\n')
			errCh <- err
		}()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("reading user input: %w", err)
			}
		}

		response = strings.TrimSpace(strings.ToLower(response))

		if response != "y" && response != "yes" {
			log.Printf("File replacement cancelled")
			return nil
		}
	}

	var sidecarFiles []string
	finalPath, sidecarFiles, err = mergeSidecarFiles(ctx, targetFile, finalPath, &tempFiles)
	if err != nil {
		return fmt.Errorf("merge subtitles and sound: %w", err)
	}

	if err := replaceOriginalWithEncoded(targetFile, finalPath); err != nil {
		return fmt.Errorf("replace encoded file: %w", err)
	}

	for _, sf := range sidecarFiles {
		if err := os.Remove(sf); err != nil {
			log.Printf("Failed to remove sidecar file %s: %v", sf, err)
		} else {
			log.Printf("Removed sidecar file: %s", sf)
		}
	}

	log.Println("Encoding completed successfully for:", targetFile)

	return nil
}

func mergeSidecarFiles(ctx context.Context, targetFile, finalPath string, tempFiles *[]string) (string, []string, error) {
	dir := filepath.Dir(targetFile)
	origBase := strings.TrimSuffix(filepath.Base(targetFile), filepath.Ext(targetFile))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return finalPath, nil, fmt.Errorf("read directory for sidecar files: %w", err)
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
		return finalPath, nil, nil
	}

	log.Printf("Found %d sidecar file(s) to merge: %v", len(sidecarFiles), sidecarFiles)

	mergedFile, err := os.CreateTemp(tempDirectory, "video_merged_*.mkv")
	if err != nil {
		return finalPath, nil, fmt.Errorf("create temp file for sidecar merge: %w", err)
	}
	mergedPath := mergedFile.Name()
	mergedFile.Close()

	*tempFiles = append(*tempFiles, mergedPath)

	args := []string{"-o", mergedPath, finalPath}
	for _, sf := range sidecarFiles {
		args = append(args, sf)
	}

	log.Println("Running mkvmerge to merge sidecars: mkvmerge", args)

	cmd := exec.CommandContext(ctx, "mkvmerge", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("mkvmerge merge output: %s", string(out))
		return finalPath, nil, fmt.Errorf("mkvmerge sidecar merge: %w", err)
	}

	log.Println("Sidecar merge successful.")
	return mergedPath, sidecarFiles, nil
}

func replaceOriginalWithEncoded(targetFile, finalPath string) error {
	var newFilePath string
	if strings.Contains(strings.ToLower(targetFile), "264") {
		newFilePath = strings.ReplaceAll(targetFile, "264", "265")
	} else {
		newFilePath = strings.TrimSuffix(targetFile, filepath.Ext(targetFile)) + ".x265.mkv"
	}

	newFilePath = replaceCasePreserving(newFilePath, "flac", "ogg")
	newFilePath = replaceCasePreserving(newFilePath, "aac", "ogg")

	log.Printf("Replacing %s with optimized version with name %s", targetFile, newFilePath)

	err := os.Rename(finalPath, newFilePath)
	if err != nil {
		log.Println("Rename failed, attempting copy and delete:", err)
		err = copyFile(finalPath, newFilePath)
		if err != nil {
			return fmt.Errorf("Replace original file: %w", err)
		}
		log.Println("File copied successfully.")
	} else {
		log.Println("File renamed successfully.")
	}

	err = os.Remove(targetFile)
	if err != nil {
		return fmt.Errorf("Remove original file: %w", err)
	}

	log.Println("Original file removed successfully (", targetFile, ")")

	origBase := strings.TrimSuffix(filepath.Base(targetFile), filepath.Ext(targetFile))
	newBase := strings.TrimSuffix(filepath.Base(newFilePath), filepath.Ext(newFilePath))
	dir := filepath.Dir(targetFile)

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
		if oldPath == targetFile || oldPath == newFilePath {
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

func replaceCasePreserving(s, old, new string) string {
	lowerS := strings.ToLower(s)
	lowerOld := strings.ToLower(old)
	idx := strings.Index(lowerS, lowerOld)
	if idx == -1 {
		return s
	}
	matched := s[idx : idx+len(old)]
	result := []rune(new)
	for i, ch := range result {
		if i < len(matched) {
			if unicode.IsUpper(rune(matched[i])) {
				result[i] = unicode.ToUpper(ch)
			} else {
				result[i] = unicode.ToLower(ch)
			}
		}
	}
	return s[:idx] + string(result) + s[idx+len(old):]
}

func findTargetVideoFile(ctx context.Context, cfg Config) (string, error) {
	if cfg.MediaListPath != "" {
		return findVideoFromList(ctx, cfg)
	}
	return findVideoFromDirectory(ctx, cfg)
}

func findVideoFromList(ctx context.Context, cfg Config) (string, error) {
	file, err := os.Open(cfg.MediaListPath)
	if err != nil {
		return "", fmt.Errorf("open media list: %w", err)
	}
	defer file.Close()

	log.Println("Reading media list from:", cfg.MediaListPath)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		path := strings.TrimSpace(scanner.Text())
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}

		mediaInfo, err := getMediaInfo(path)
		if err != nil {
			return "", fmt.Errorf("get mediainfo '%s': %w", path, err)
		}

		if isAlreadyOptimized(mediaInfo) {
			continue
		}

		log.Println("Found valid file in media list: ", path)
		log.Println("Info:", info)

		return path, nil
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read media list: %w", err)
	}

	return "", fmt.Errorf("no valid files found in media list")
}

func findVideoFromDirectory(ctx context.Context, cfg Config) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	log.Println("Scanning for largest eligible video file...")

	var largestFile string
	var largestSize int64
	threshold := oldestAllowedModTime

	err := filepath.Walk(cfg.MediaDir, func(path string, info os.FileInfo, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err != nil || info.IsDir() {
			return nil
		}

		if _, ok := alreadyProcessedFiles[path]; ok {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if _, ok := validVideoExtensions[ext]; !ok {
			return nil
		}

		if info.ModTime().Before(threshold) {
			if info.Size() > largestSize {
				largestSize = info.Size()
				largestFile = path
			}
		}
		return nil
	})

	return largestFile, err
}

func isAlreadyOptimized(info *MediaInfoOutput) bool {
	alreadyOptimizedCodecs := []string{
		"MPEG-H/HEVC/h.265", "HEVC", "V_MPEGH/ISO/HEVC", "265",
		"AV1", "V_AV1", "VVC",
		"DVHE", "V_DVHE", "DVH1", "V_DVH1",
		"HVC1", "HVC2",
	}

	for _, track := range info.Media.Tracks {
		if strings.EqualFold(track.Type, "video") {
			codec := strings.ToUpper(track.CodecID)
			for _, skip := range alreadyOptimizedCodecs {
				if strings.Contains(codec, skip) {
					return true
				}
			}
			if strings.EqualFold(track.Format, "HEVC") {
				return true
			}
		}
	}

	return false
}

func getMediaInfo(path string) (*MediaInfoOutput, error) {
	cmd := exec.Command("mediainfo", "--fullscan", "--Output=JSON", path)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var data MediaInfoOutput
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func getMkvMergeInfo(path string) (*MkvMergeOutput, error) {
	cmd := exec.Command("mkvmerge", "-J", path)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var data MkvMergeOutput
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func selectHandbrakePreset(info *MediaInfoOutput) string {
	width := 0
	height := 0
	bitrate := 0

	for _, track := range info.Media.Tracks {
		if strings.EqualFold(track.Type, "video") {
			width, _ = strconv.Atoi(track.Width)
			height, _ = strconv.Atoi(track.Height)
			bitrate, _ = strconv.Atoi(track.Bitrate)
		}
	}

	mode := "slow"
	resolution := "1080p"
	quality := 20

	if width > 1920 || height > 1080 {
		resolution = "2160p"
		if width >= 2100 || height >= 1200 {
			quality++
		}
	}
	if width < 1280 && height < 720 {
		quality--
		if width < 854 && height < 480 {
			quality--
			if width < 640 && height < 360 {
				quality--
			}
		}
	}

	if bitrate != 0 {
		if bitrate > 5_000_000 {
			quality--
			if bitrate > 12_000_000 {
				quality--
			}
		}
		if bitrate < 1_500_000 {
			quality++
		}
	}

	switch resolution {
	case "2160p":
		quality = clamp(quality, 17, 21)
	case "1080p":
		quality = clamp(quality, 14, 21)
	}

	return strings.Join([]string{mode, resolution, strconv.Itoa(quality)}, "-")
}

func clamp(val, min, max int) int {
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}

func runHandbrakeCLI(ctx context.Context, cfg Config, input, output, preset string) error {
	args := []string{
		"-n", "19",
		"HandBrakeCLI",
		"--preset-import-file", cfg.HandbrakePresetsPath,
		"-Z", preset,
		"-i", input,
		"-o", output,
		"--format", "mkv",
	}

	log.Println("Running HandbrakeCLI command: nice", args)

	cmd := exec.Command("nice", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Run()
	}()

	select {
	case <-ctx.Done():
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func deduplicateAudioTracks(filePath string, tempFiles *[]string) (string, error) {
	info, err := getMkvMergeInfo(filePath)
	if err != nil {
		return "", err
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
		return filePath, nil
	}

	log.Println("Remuxing to remove duplicate audio tracks...")

	remuxFile, err := os.CreateTemp(tempDirectory, "video_remux_*.mkv")
	if err != nil {
		return "", err
	}
	remuxPath := remuxFile.Name()
	remuxFile.Close()

	*tempFiles = append(*tempFiles, remuxPath)

	args := []string{
		"-o", remuxPath,
		"--audio-tracks", strings.Join(keepTrackIDs, ","),
		filePath,
	}

	log.Println("Running mkvmerge with args: mkvmerge", args)

	cmd := exec.Command("mkvmerge", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Mkvmerge remux output: %s", string(output))
		return "", fmt.Errorf("remux failed: %w", err)
	}

	return remuxPath, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}

	return out.Close()
}

func formatNum[T int | int64](n T) string {
	return formatStr(strconv.Itoa(int(n)))
}

func formatStr(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}

	var b strings.Builder
	pre := n % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if n > pre {
			b.WriteString(",")
		}
	}
	for i := pre; i < n; i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < n {
			b.WriteString(",")
		}
	}
	return b.String()
}

func getFileSize(p string) string {
	info, err := os.Stat(p)
	if err != nil {
		log.Println("Error getting file size:", err)
		return "N/A"
	}
	return formatNum(info.Size())
}
