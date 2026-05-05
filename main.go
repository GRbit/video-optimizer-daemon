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
	envTempDirPath   = "TEMP_DIR"

	defaultMediaDir      = "/media"
	defaultHandbrakeConf = "$HOME/.config/ghb/presets.json"
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
			if err := processVideoFiles(ctx, cfg); err != nil {
				log.Println("Error during encoding: ", err)
				ticker.Reset(time.Minute)
			}
		}
	}
}

func processVideoFiles(ctx context.Context, cfg Config) error {
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

	task := &VideoConvertTask{
		cfg:        cfg,
		targetPath: targetFile,
	}

	return task.Run(ctx)
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

	cmd := exec.CommandContext(ctx, "nice", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
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
