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
	subsExtensions      = []string{".srt", ".ass", ".vtt", ".sub", ".ssa", ".idx", ".txt", ".txt"}
	timeThreshold       = time.Now().AddDate(0, -1, 0) // 1 month ago
	processedExtensions = func() map[string]struct{} {
		exts := []string{"mkv", "mp4", "avi", "mov", "m4v", "webm", "ts"}
		ret := make(map[string]struct{}, len(exts))
		for _, e := range exts {
			ret["."+e] = struct{}{}
		}
		return ret
	}()
	optimizedFiles = make(map[string]struct{})
)

// Config holds command line flags
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
	// Parse Flags
	promptPtr := flag.Bool("prompt", false, "Ask for confirmation before replacing original files")
	mediaDirPtr := flag.String("media-dir", defaultMediaDir, "Directory to scan for media files")
	handbrakeConfPtr := flag.String("handbrake-conf", defaultHandbrakeConf, "Path to HandBrake presets JSON file")
	mediaListPtr := flag.String("media-list", "", "Path to media list file (not used currently)")
	flag.Parse()

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

	// handle shutdown signals
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
			if err := encodeFile(ctx, cfg); err != nil {
				log.Println("Error during encoding: ", err)
				os.Exit(1)
				ticker.Reset(time.Hour)
			}
		}
	}
}

func encodeFile(ctx context.Context, cfg Config) error {
	targetFile, err := findTargetFile(ctx, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("search files: %w", err)
	}

	if targetFile == "" {
		return fmt.Errorf("no matching files found (older than %v)", time.Since(timeThreshold))
	}

	log.Printf("Found target candidate: %s", targetFile)

	mediaInfo, err := getMediaInfo(targetFile)
	if err != nil {
		return fmt.Errorf("get mediainfo: %w", err)
	}

	// print vide track bitrate
	for _, track := range mediaInfo.Media.Tracks {
		if strings.EqualFold(track.Type, "video") {
			bitrate := formatStr(track.Bitrate)
			log.Printf("Video Format: %s, CodecID: %s, Bitrate: %s bps", track.Format, track.CodecID, bitrate)
			break
		}
	}

	log.Printf("File Size: %s", getFileSize(targetFile))

	if shouldSkipFile(mediaInfo) {
		log.Println("File already optimized, skipping.")
		optimizedFiles[targetFile] = struct{}{}
		return nil
	}

	preset := pickHandbrakePreset(mediaInfo)
	log.Printf("Selected Preset: %s", preset)

	if cfg.PromptMode {
		fmt.Printf("\n--- ACTION REQUIRED ---\n")
		fmt.Printf("File to convert: %s\n", targetFile)
		fmt.Print("Start convertation? (y/n): ")

		reader := bufio.NewReader(os.Stdin)

		// non blocking read
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
			log.Printf("Convertation was declined")
			return nil
		}
	}

	// Setup Temp Files
	encodedFile, err := os.CreateTemp(os.TempDir(), "video_opt_*.mkv")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	encodedPath := encodedFile.Name()
	encodedFile.Close() // Close immediately, Handbrake will write to it

	log.Println("temp file created:", encodedPath)

	// Defer cleanup of the main encoded file (and potential remux file)
	var filesToDelete []string
	filesToDelete = append(filesToDelete, encodedPath)

	defer func() {
		for _, f := range filesToDelete {
			if _, err := os.Stat(f); err == nil {
				log.Printf("Cleaning up temp file: %s", f)
				os.Remove(f)
			}
		}
	}()

	// Run Handbrake
	log.Println("Starting HandBrake conversion...")
	err = runHandbrake(ctx, cfg, targetFile, encodedPath, preset)
	if err != nil {
		return fmt.Errorf("run handbrake: %w", err)
	}
	log.Println("HandBrake finished successfully.")

	log.Println("Checking audio tracks on converted file...")
	finalPath, err := processAudioTracks(encodedPath, &filesToDelete)
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

		// non blocking read
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

	if err := replaceEncodedFile(targetFile, finalPath); err != nil {
		return fmt.Errorf("replace encoded file: %w", err)
	}

	log.Println("Encoding completed successfully for:", targetFile)

	return nil
}

func replaceEncodedFile(targetFile, finalPath string) error {
	// replace .ext with .x265.mkv
	newFilePath := strings.TrimSuffix(targetFile, filepath.Ext(targetFile)) + ".x265.mkv"

	log.Printf("Replacing %s with optimized version with name %s", targetFile, newFilePath)

	// New encoded file should be named as the original, but with .x265.mkv extension
	// After we move temp to this name, we can remove the original

	err := os.Rename(finalPath, newFilePath)
	if err != nil {
		log.Println("Rename failed, attempting copy and delete:", err)
		// Fallback for cross-device link errors
		err = copyFile(finalPath, newFilePath)
		if err != nil {
			return fmt.Errorf("Replace original file: %w", err)
		} else {
			log.Println("File copied successfully.")
		}
	} else {
		log.Println("File renamed successfully.")
	}

	err = os.Remove(targetFile)
	if err != nil {
		return fmt.Errorf("Remove original file: %w", err)
	}

	log.Println("Original file removed successfully (", targetFile, ")")

	// rename subtitle files if needed
	origBase := strings.TrimSuffix(targetFile, filepath.Ext(targetFile))
	for _, subExt := range subsExtensions {
		origSubPath := origBase + subExt
		if _, err := os.Stat(origSubPath); err == nil {
			newSubPath := strings.TrimSuffix(newFilePath, filepath.Ext(newFilePath)) + subExt
			err = os.Rename(origSubPath, newSubPath)
			if err != nil {
				log.Printf("Failed to rename subtitle file %s: %v", origSubPath, err)
			} else {
				log.Printf("Renamed subtitle file %s to %s", origSubPath, newSubPath)
			}
		}
	}

	return nil
}

// findTargetFile looks for the largest video file older than 1 month
func findTargetFile(ctx context.Context, cfg Config) (string, error) {
	if cfg.MediaListPath != "" {
		return findTargetFileList(ctx, cfg)
	}
	return findTargetFileWalk(ctx, cfg)
}

func findTargetFileList(ctx context.Context, cfg Config) (string, error) {
	file, err := os.Open(cfg.MediaListPath)
	if err != nil {
		return "", fmt.Errorf("open media list: %w", err)
	}
	defer file.Close()

	log.Println("Reading media list from:", cfg.MediaListPath)

	// read first line, check if file exists and is valid, return the found file
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

		if shouldSkipFile(mediaInfo) {
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

func findTargetFileWalk(ctx context.Context, cfg Config) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	log.Println("Scanning for largest eligible video file...")

	var largestFile string
	var largestSize int64
	threshold := timeThreshold

	err := filepath.Walk(cfg.MediaDir, func(path string, info os.FileInfo, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err != nil || // Skip access errors
			info.IsDir() {
			return nil
		}

		if _, ok := optimizedFiles[path]; ok {
			return nil // already optimized
		}

		// Filter for video extensions
		ext := strings.ToLower(filepath.Ext(path))
		if _, ok := processedExtensions[ext]; !ok {
			return nil // not a video file
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

func shouldSkipFile(info *MediaInfoOutput) bool {
	skipCodecs := []string{"MPEG-H/HEVC/h.265", "HEVC", "V_MPEGH/ISO/HEVC", "265", "AV1", "V_AV1", "VVC"}

	for _, track := range info.Media.Tracks {
		if strings.EqualFold(track.Type, "video") {
			codec := strings.ToUpper(track.CodecID)
			for _, skip := range skipCodecs {
				if strings.Contains(codec, skip) {
					return true
				}
			}
		}
	}
	return false
}

func getMediaInfo(path string) (*MediaInfoOutput, error) {
	// --Output=JSON is cleaner
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

func getMkvInfo(path string) (*MkvMergeOutput, error) {
	// Using -J for JSON output
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

func pickHandbrakePreset(info *MediaInfoOutput) string {
	width := 0
	height := 0
	bitrate := 0

	// Parse dimensions from MediaInfo
	for _, track := range info.Media.Tracks {
		if strings.EqualFold(track.Type, "video") {
			width, _ = strconv.Atoi(track.Width)
			height, _ = strconv.Atoi(track.Height)
			bitrate, _ = strconv.Atoi(track.Bitrate)
		}
	}

	// default quality
	mode := "slow"
	resolution := "1080p"
	q := 20

	if width > 1920 || height > 1080 {
		resolution = "2160p"
		q++
	}
	if width < 1280 && height < 720 {
		q--
		if width < 854 && height < 480 {
			q--
			if width < 640 && height < 360 {
				q--
			}
		}
	}
	if bitrate > 7_000_000 {
		q--
		if bitrate > 15_000_000 {
			q--
		}
	}
	if bitrate < 1_500_000 {
		q++
	}

	switch resolution {
	case "2160p":
		q = clamp(q, 17, 21)
	case "1080p":
		q = clamp(q, 14, 21)
	}

	return strings.Join([]string{mode, resolution, strconv.Itoa(q)}, "-")
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

// nice -n 19 HandBrakeCLI ...
func runHandbrake(ctx context.Context, cfg Config, input, output, preset string) error {
	args := []string{
		"-n", "19",
		"HandBrakeCLI",
		"--preset-import-file", cfg.HandbrakePresetsPath,
		"-Z", preset,
		"-i", input,
		"-o", output,
		"--format", "mkv", // Enforce container
	}

	log.Println("Running HandbrakeCLI command: nice", args)

	cmd := exec.Command("nice", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr // Handbrake logs progress to stderr

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

// processAudioTracks checks for duplicate languages and remuxes if necessary
// Returns the path to the valid file (either the original encoded one, or a new remuxed one)
func processAudioTracks(filePath string, filesToDelete *[]string) (string, error) {
	info, err := getMkvInfo(filePath)
	if err != nil {
		return "", err
	}

	// Analyze tracks
	seenLangs := make(map[string]bool)
	var keepTrackIDs []string
	needsRemux := false
	audioCount := 0

	for _, track := range info.Tracks {
		if strings.EqualFold(track.Type, "audio") {
			audioCount++
			lang := track.Properties.Language
			// If language is missing, treat as 'und'
			if lang == "" {
				lang = "und"
			}

			if seenLangs[lang] {
				// Duplicate found, do not add to keep list
				needsRemux = true
				log.Printf("Duplicate audio language found: %s. Dropping track ID %d.", lang, track.ID)
			} else {
				seenLangs[lang] = true
				keepTrackIDs = append(keepTrackIDs, strconv.Itoa(track.ID))
			}
		} else {
			// Keep video/subs/etc
			keepTrackIDs = append(keepTrackIDs, strconv.Itoa(track.ID))
		}
	}

	// "If there are more than 2 languages, file is checked with mediainfo"

	if !needsRemux {
		log.Println("Audio tracks are optimal. No remuxing needed.")
		return filePath, nil
	}

	log.Println("Remuxing to remove duplicate audio tracks...")

	remuxFile, err := os.CreateTemp(os.TempDir(), "video_remux_*.mkv")
	if err != nil {
		return "", err
	}
	remuxPath := remuxFile.Name()
	remuxFile.Close()

	*filesToDelete = append(*filesToDelete, remuxPath)

	// mkvmerge -o output.mkv --audio-tracks id1,id2 input.mkv
	// Note: mkvmerge --audio-tracks expects specifically audio IDs.
	// However, a simpler way to keep specific tracks globally is using generic -a -d -s logic
	// or --tracks but that can be complex.
	// Easiest is to construct a specific command that disables specific tracks,
	// OR use the --tracks (or -t) logic to keep everything listed.

	// Actually, mkvmerge logic: By default keeps all. We want to explicitely keep specific ones.
	// Let's use the track IDs found in JSON (which are global IDs for mkvmerge).

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

// copyFile is a manual fallback if Rename fails across filesystems
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

// formatNum formats an integer with comma separators (e.g. 1234567 -> "1,234,567")
func formatNum[T int | int64](n T) string {
	return formatStr(strconv.Itoa(int(n)))
}

// Format bitrate nicely (e.g. 1,234,567 bps) by 3 digits from end
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
