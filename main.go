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
	envTempDirPath   = "TEMP_DIR"

	defaultMediaDir = "/media"

	// retryDelay is the pause after an empty scan or a failed task. A successful
	// task is followed by the next scan immediately.
	retryDelay = time.Minute
)

var (
	validVideoExtensions = func() map[string]struct{} {
		exts := []string{"mkv", "mp4", "avi", "mov", "m4v", "webm", "ts"}
		ret := make(map[string]struct{}, len(exts))
		for _, e := range exts {
			ret["."+e] = struct{}{}
		}
		return ret
	}()
	alreadyProcessedFiles = make(map[string]struct{})

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
	TempDirPath          string
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
			Duration string `json:"Duration"`
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

// defaultHandbrakeConf returns the HandBrake GUI presets file in the user's
// home, or "" when the home directory is unknown. A literal "$HOME/..." would
// reach HandBrakeCLI unexpanded, so the path is resolved here.
func defaultHandbrakeConf() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "ghb", "presets.json")
}

func main() {
	promptPtr := flag.Bool("prompt", false, "Ask for confirmation before replacing original files")
	mediaDirPtr := flag.String("media-dir", defaultMediaDir, "Directory to scan for media files")
	handbrakeConfPtr := flag.String("handbrake-conf", defaultHandbrakeConf(), "Path to HandBrake presets JSON file")
	mediaListPtr := flag.String("media-list", "", "Path to a file with video paths, one per line; replaces directory scanning")
	tmpDirPtr := flag.String("tmp", os.TempDir(), "Directory to use for temporary files")
	flag.Parse()

	cfg := Config{
		PromptMode:           *promptPtr,
		MediaDir:             *mediaDirPtr,
		MediaListPath:        *mediaListPtr,
		HandbrakePresetsPath: *handbrakeConfPtr,
		TempDirPath:          *tmpDirPtr,
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
	if os.Getenv(envTempDirPath) != "" {
		cfg.TempDirPath = os.Getenv(envTempDirPath)
	}

	if cfg.HandbrakePresetsPath == "" {
		log.Fatalf("handbrake presets path is not set: pass -handbrake-conf or %s (home directory could not be determined)", envHandbrakeConf)
	}

	log.Println("Starting Video Optimizer Daemon...")
	log.Printf("Configuration: %+v", cfg)

	ctx, cancel := context.WithCancel(context.Background())

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Println("Shutting down...")
		cancel()
	}()

	for ctx.Err() == nil {
		processed, err := processVideoFiles(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			log.Println("Error during encoding: ", err)
		case processed:
			continue
		default:
			log.Printf("No eligible files found, next scan in %v", retryDelay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

// processVideoFiles picks one candidate and runs the conversion task on it. It
// returns false when nothing was eligible. The candidate is marked as processed
// whatever the outcome (success, skip, decline, error): a broken file that is
// retried every scan blocks the whole queue.
func processVideoFiles(ctx context.Context, cfg Config) (bool, error) {
	targetFile, err := findTargetVideoFile(ctx, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("search files: %w", err)
	}

	if targetFile == "" {
		return false, nil
	}

	log.Printf("Found target candidate: %s", targetFile)

	task := &VideoConvertTask{
		cfg:        cfg,
		targetPath: targetFile,
	}

	err = task.Run(ctx)
	if ctx.Err() != nil {
		// Shutdown interrupted the task; leave it eligible for the next start.
		return false, ctx.Err()
	}
	alreadyProcessedFiles[targetFile] = struct{}{}
	return true, err
}

func findTargetVideoFile(ctx context.Context, cfg Config) (string, error) {
	if cfg.MediaListPath != "" {
		return findVideoFromList(ctx, cfg)
	}
	return findVideoFromDirectory(ctx, cfg)
}

func findVideoFromList(ctx context.Context, cfg Config) (string, error) {
	file, err := os.Open(cfg.MediaListPath)
	defer closeCloser(file)
	if err != nil {
		return "", fmt.Errorf("open media list: %w", err)
	}

	log.Println("Reading media list from:", cfg.MediaListPath)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		path := strings.TrimSpace(scanner.Text())
		if path == "" {
			continue
		}
		if _, ok := alreadyProcessedFiles[path]; ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}

		mediaInfo, err := getMediaInfo(path)
		if err != nil {
			// One unreadable entry must not stall the whole list forever.
			log.Printf("Skipping '%s': get mediainfo: %v", path, err)
			alreadyProcessedFiles[path] = struct{}{}
			continue
		}

		if isAlreadyOptimized(mediaInfo) {
			alreadyProcessedFiles[path] = struct{}{}
			continue
		}

		log.Println("Found valid file in media list: ", path)

		return path, nil
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read media list: %w", err)
	}

	return "", nil
}

func findVideoFromDirectory(ctx context.Context, cfg Config) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	log.Println("Scanning for largest eligible video file...")

	var largestFile string
	var largestSize int64
	// Recomputed per scan: a daemon-wide constant would freeze at start time
	// and newer files would never become eligible.
	threshold := time.Now().AddDate(0, -1, 0)

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
		"AV2", "V_AV2",
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
		return nil, fmt.Errorf("mkvmerge -J %s: %w", path, err)
	}

	var data MkvMergeOutput
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("mkvmerge: unmarshal JSON '%s': %w", string(out), err)
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

	cmd := exec.CommandContext(ctx, "nice", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// copyFile is the cross-device fallback for os.Rename. The original is deleted
// right after it succeeds, so a partial destination is removed on any failure
// and the copy is fsynced and size-checked before reporting success.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening input file: %w", err)
	}
	defer closeCloser(in)

	srcStat, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat input file: %w", err)
	}

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer func() {
		if err != nil {
			if rmErr := os.Remove(dst); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Printf("Failed to remove partial copy %s: %v", dst, rmErr)
			}
		}
	}()

	written, err := io.Copy(out, in)
	if err != nil {
		closeCloser(out)
		return fmt.Errorf("copying data: %w", err)
	}
	if err = out.Sync(); err != nil {
		closeCloser(out)
		return fmt.Errorf("syncing output file: %w", err)
	}
	if err = out.Close(); err != nil {
		return fmt.Errorf("closing output file: %w", err)
	}

	if written != srcStat.Size() {
		err = fmt.Errorf("size mismatch after copy: source %d bytes, written %d bytes", srcStat.Size(), written)
		return err
	}

	return nil
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

func promptConfirm(ctx context.Context) (bool, error) {
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
		return false, ctx.Err()
	case err := <-errCh:
		if err != nil {
			return false, fmt.Errorf("reading user input: %w", err)
		}
	}

	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes", nil
}

func closeCloser(c io.Closer) {
	if c == nil {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			if r == "runtime error: invalid memory address or nil pointer dereference" {
				log.Printf("Attempted to close a nil pointer: %v", r)
				return
			}
			panic(r)
		}
	}()

	if err := c.Close(); err != nil {
		log.Printf("Failed to close: %v", err)
	}
}
