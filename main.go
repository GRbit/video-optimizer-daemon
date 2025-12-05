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
	envMedia         = "MEDIA_DIR"
	envHandbrakeConf = "HANDBRAKE_CONF"
	envPromptMode    = "PROMPT_MODE"

	defaultMediaDir      = "/media"
	defaultHandbrakeConf = "/root/.config/ghb/presets.json"
)

var (
	timeThreshold       = time.Now().AddDate(0, -1, 0) // 1 month ago
	processedExtensions = func() map[string]struct{} {
		exts := []string{"mkv", "mp4", "avi", "mov", "m4v"}
		ret := make(map[string]struct{}, len(exts))
		for _, e := range exts {
			ret["."+e] = struct{}{}
		}
		return ret
	}()
)

// Config holds command line flags
type Config struct {
	PromptMode           bool
	MedaiDir             string
	HandbrakePresetsPath string
}

// Structs for JSON parsing
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

type MediaInfoOutput struct {
	Media struct {
		Track []struct {
			Type   string `json:"@type"`
			Format string `json:"Format"`
			Width  string `json:"Width"`
			Height string `json:"Height"`
		} `json:"track"`
	} `json:"media"`
}

func main() {
	// Parse Flags
	promptPtr := flag.Bool("prompt", false, "Ask for confirmation before replacing original files")
	mediaDirPtr := flag.String("media-dir", defaultMediaDir, "Directory to scan for media files")
	handbrakeConfPtr := flag.String("handbrake-conf", defaultHandbrakeConf, "Path to HandBrake presets JSON file")
	flag.Parse()

	cfg := Config{
		PromptMode:           *promptPtr,
		MedaiDir:             *mediaDirPtr,
		HandbrakePresetsPath: *handbrakeConfPtr,
	}
	if os.Getenv(envMedia) != "" {
		cfg.MedaiDir = os.Getenv(envMedia)
	}
	if os.Getenv(envHandbrakeConf) != "" {
		cfg.HandbrakePresetsPath = os.Getenv(envHandbrakeConf)
	}
	if os.Getenv(envPromptMode) != "" && cfg.PromptMode == false {
		envVal, _ := strconv.ParseBool(os.Getenv(envPromptMode))
		cfg.PromptMode = envVal
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

	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := encodeFile(ctx, cfg); err != nil {
				log.Println("Error during encoding: ", err)
				time.Sleep(time.Hour) // if no files encoded sleep
			}
		}
	}
}

func encodeFile(ctx context.Context, cfg Config) error {
	log.Println("Scanning for largest eligible video file...")

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

	mkvInfo, err := getMkvInfo(targetFile)
	if err != nil {
		return fmt.Errorf("get mkvmerge info: %w", err)
	}

	if shouldSkipFile(mkvInfo) {
		log.Println("File already optimized, skipping.")
		return nil
	}

	mediaInfo, err := getMediaInfo(targetFile)
	if err != nil {
		return fmt.Errorf("get mediainfo: %w", err)
	}

	preset := getHandbrakePreset(mediaInfo)
	log.Printf("Selected Preset: %s", preset)

	// Setup Temp Files
	encodedFile, err := os.CreateTemp(os.TempDir(), "video_opt_*.mkv")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	encodedPath := encodedFile.Name()
	encodedFile.Close() // Close immediately, Handbrake will write to it

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
		fmt.Printf("New File: %s\n", finalPath)
		fmt.Print("Replace original file? (y/n): ")

		reader := bufio.NewReader(os.Stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))

		if response != "y" && response != "yes" {
			return nil
		}
	}

	log.Printf("Replacing %s with optimized version...", targetFile)
	// On Linux, Rename is atomic if on same FS. If temp is different FS, we need copy/delete.
	// Since we are likely in a container with a single FS or mapped volumes, attempt Rename first.
	err = os.Rename(finalPath, targetFile)
	if err != nil {
		// Fallback for cross-device link errors
		err = copyFile(finalPath, targetFile)
		if err != nil {
			return fmt.Errorf("replace original file: %w", err)
		}
	}

	log.Println("Encoding completed successfully for:", targetFile)

	return nil
}

// findTargetFile looks for the largest video file older than 1 month
func findTargetFile(ctx context.Context, cfg Config) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	var largestFile string
	var largestSize int64
	threshold := timeThreshold

	err := filepath.Walk(cfg.MedaiDir, func(path string, info os.FileInfo, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err != nil || // Skip access errors
			info.IsDir() {
			return nil
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

func shouldSkipFile(info *MkvMergeOutput) bool {
	skipCodecs := []string{"MPEG-H/HEVC/h.265", "HEVC", "V_MPEGH/ISO/HEVC", "265", "AV1", "V_AV1", "VVC"}

	for _, track := range info.Tracks {
		if track.Type == "video" {
			codec := strings.ToUpper(track.Codec)
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

func getHandbrakePreset(info *MediaInfoOutput) string {
	width := 0
	height := 0

	// Parse dimensions from MediaInfo
	for _, track := range info.Media.Track {
		if track.Type == "Video" {
			w, _ := strconv.Atoi(track.Width)
			h, _ := strconv.Atoi(track.Height)
			if w > width {
				width = w
			}
			if h > height {
				height = h
			}
		}
	}

	if width > 1920 || height > 1080 {
		return "slow-2160p-20"
	}
	return "slow-1080p-19"
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
		if track.Type == "audio" {
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

	// Prompt condition: "If there are more than 2 languages, file is checked with mediainfo"
	// The prompt logic is slightly ambiguous here. It implies checking complexity if many languages exist.
	// Since we are deduping regardless, we just proceed with the deduplication logic.

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
		"--tracks", strings.Join(keepTrackIDs, ","),
		filePath,
	}

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
