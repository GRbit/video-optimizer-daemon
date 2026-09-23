package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	envMediaList     = "MEDIA_LIST_PATH"
	envTempDirPath   = "TEMP_DIR"
	envStatePath     = "STATE_PATH"
	envLogLevel      = "LOG_LEVEL"
	envMinAge        = "MIN_AGE"
	envWorkHours     = "WORK_HOURS"
	envPreset1080p   = "PRESET_1080P"
	envPreset2160p   = "PRESET_2160P"

	defaultMediaDir    = "/media"
	defaultMinAge      = 30 * 24 * time.Hour
	defaultPreset1080p = "slow-1080p-20"
	defaultPreset2160p = "slow-2160p-20"
	stateFileName      = ".video-optimizer-state.json"

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
	StatePath            string
	LogLevel             string
	MinAge               time.Duration
	WorkHours            string
	Preset1080p          string
	Preset2160p          string
}

type MkvMergeTrack struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	Properties struct {
		Language string `json:"language"`
	} `json:"properties"`
}

type MkvMergeOutput struct {
	Tracks []MkvMergeTrack `json:"tracks"`
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

// defaultStatePath keeps the state next to the data it describes: inside the
// media directory, or beside the media list.
func defaultStatePath(cfg Config) string {
	if cfg.MediaListPath != "" {
		return filepath.Join(filepath.Dir(cfg.MediaListPath), stateFileName)
	}
	return filepath.Join(cfg.MediaDir, stateFileName)
}

func loadConfig() (Config, error) {
	promptPtr := flag.Bool("prompt", false, "Interactive mode: ask before starting a conversion and before replacing the original (terminal only)")
	mediaDirPtr := flag.String("media-dir", defaultMediaDir, "Directory to scan for media files")
	handbrakeConfPtr := flag.String("handbrake-conf", defaultHandbrakeConf(), "Path to HandBrake presets JSON file")
	mediaListPtr := flag.String("media-list", "", "Path to a file with video paths, one per line; replaces directory scanning")
	tmpDirPtr := flag.String("tmp", os.TempDir(), "Directory to use for temporary files")
	statePtr := flag.String("state", "", "Path to the JSON state file (default: "+stateFileName+" in the media dir, or next to the media list)")
	logLevelPtr := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	minAgePtr := flag.Duration("min-age", defaultMinAge, "Minimum time since last modification for a file to be eligible")
	workHoursPtr := flag.String("work-hours", "", "Local time window for encoding, e.g. 23:00-07:00 (default: always)")
	preset1080Ptr := flag.String("preset-1080p", defaultPreset1080p, "HandBrake preset for sources up to 1080p; CRF is passed separately with -q")
	preset2160Ptr := flag.String("preset-2160p", defaultPreset2160p, "HandBrake preset for sources above 1080p; CRF is passed separately with -q")
	flag.Parse()

	cfg := Config{
		PromptMode:           *promptPtr,
		MediaDir:             *mediaDirPtr,
		MediaListPath:        *mediaListPtr,
		HandbrakePresetsPath: *handbrakeConfPtr,
		TempDirPath:          *tmpDirPtr,
		StatePath:            *statePtr,
		LogLevel:             *logLevelPtr,
		MinAge:               *minAgePtr,
		WorkHours:            *workHoursPtr,
		Preset1080p:          *preset1080Ptr,
		Preset2160p:          *preset2160Ptr,
	}

	envString := func(dst *string, name string) {
		if v := os.Getenv(name); v != "" {
			*dst = v
		}
	}
	envString(&cfg.MediaDir, envMediaDir)
	envString(&cfg.HandbrakePresetsPath, envHandbrakeConf)
	envString(&cfg.MediaListPath, envMediaList)
	envString(&cfg.TempDirPath, envTempDirPath)
	envString(&cfg.StatePath, envStatePath)
	envString(&cfg.LogLevel, envLogLevel)
	envString(&cfg.WorkHours, envWorkHours)
	envString(&cfg.Preset1080p, envPreset1080p)
	envString(&cfg.Preset2160p, envPreset2160p)
	// PromptMode has no env override on purpose: it is for interactive runs
	// from a terminal, never for a container or a service.
	if v := os.Getenv(envMinAge); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", envMinAge, err)
		}
		cfg.MinAge = d
	}

	if cfg.HandbrakePresetsPath == "" {
		return cfg, fmt.Errorf("handbrake presets path is not set: pass -handbrake-conf or %s (home directory could not be determined)", envHandbrakeConf)
	}
	if cfg.StatePath == "" {
		cfg.StatePath = defaultStatePath(cfg)
	}
	return cfg, nil
}

func setupLogger(level string) error {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return fmt.Errorf("log level %q: %w", level, err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
	return nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	if err := setupLogger(cfg.LogLevel); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	slog.Info("Starting Video Optimizer Daemon")
	slog.Info("Configuration", "config", fmt.Sprintf("%+v", cfg))

	window, err := parseWorkWindow(cfg.WorkHours)
	if err != nil {
		slog.Error("Invalid configuration", "err", err)
		os.Exit(1)
	}
	if err := validateConfig(cfg); err != nil {
		slog.Error("Invalid configuration", "err", err)
		os.Exit(1)
	}
	state, err := loadState(cfg.StatePath)
	if err != nil {
		slog.Error("Cannot load state", "err", err)
		os.Exit(1)
	}

	lowerPriority()

	ctx, cancel := context.WithCancel(context.Background())

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		slog.Info("Shutting down")
		cancel()
	}()

	newDaemon(cfg, state, window).loop(ctx)

	if err := state.Flush(); err != nil {
		slog.Error("Failed to flush state on shutdown", "err", err)
	}
}

// Daemon is the orchestration: which file next, one task per pass, what to
// record. The slow parts come in as two function values so this logic can be
// driven in tests without mediainfo or an encoder on PATH.
type Daemon struct {
	cfg     Config
	state   *State
	window  *workWindow
	probe   func(path string) (VideoFacts, error)
	convert func(ctx context.Context, path string, facts VideoFacts) (convertResult, error)
}

func newDaemon(cfg Config, state *State, window *workWindow) *Daemon {
	enc := newEncoder(cfg, window)
	return &Daemon{
		cfg:    cfg,
		state:  state,
		window: window,
		probe:  probeVideo,
		convert: func(ctx context.Context, path string, facts VideoFacts) (convertResult, error) {
			task := &VideoConvertTask{cfg: cfg, targetPath: path, facts: facts, encoder: enc}
			return task.Run(ctx)
		},
	}
}

// outcomeOf is the only place a conversion result becomes a state entry.
func outcomeOf(res convertResult, err error) StateEntry {
	switch {
	case err != nil:
		return StateEntry{Outcome: OutcomeFailed, Error: err.Error()}
	case res == convertDeclined:
		return StateEntry{Outcome: OutcomeDeclined}
	case res == convertGainTooSmall:
		return StateEntry{Outcome: OutcomeSmallGain}
	default:
		return StateEntry{Outcome: OutcomeDone}
	}
}

func (d *Daemon) loop(ctx context.Context) {
	for ctx.Err() == nil {
		if !d.window.waitUntilOpen(ctx) {
			return
		}

		processed, err := d.processNext(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			slog.Error("Task failed", "err", err)
		case processed:
			continue
		default:
			slog.Info("No eligible files found", "next_scan_in", retryDelay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

// processNext walks the candidate list until one file actually needs encoding,
// runs that one task and returns. Every file it looked at is recorded in the
// state whatever the outcome: a broken file retried every scan blocks the queue.
func (d *Daemon) processNext(ctx context.Context) (bool, error) {
	candidates, err := findCandidates(ctx, d.cfg, d.state)
	if err != nil {
		return false, fmt.Errorf("search files: %w", err)
	}
	defer func() {
		if err := d.state.Flush(); err != nil {
			slog.Error("Failed to flush state", "err", err)
		}
	}()

	for _, path := range candidates {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		facts, err := d.probe(path)
		if err != nil {
			slog.Warn("Skipping file: probe failed", "path", path, "err", err)
			d.state.Record(path, StateEntry{Outcome: OutcomeFailed, Error: "probe: " + err.Error()})
			continue
		}
		if facts.AlreadyOptimized() {
			slog.Debug("Skipping file: already optimized", "path", path, "codec", facts.CodecID)
			d.state.Record(path, StateEntry{Outcome: OutcomeSkippedHEVC})
			continue
		}

		slog.Info("Found target candidate", "path", path)
		res, err := d.convert(ctx, path, facts)
		if ctx.Err() != nil {
			// Shutdown interrupted the task; leave it eligible for the next start.
			return false, ctx.Err()
		}
		d.state.Record(path, outcomeOf(res, err))
		return true, err
	}

	return false, nil
}

// selectEncoding picks the preset family by resolution and the CRF from the
// source's resolution and bitrate. The CRF is passed to HandBrake with -q, so
// the preset itself only needs to describe the encoder settings.
func selectEncoding(cfg Config, f VideoFacts) (preset string, crf int) {
	width, height, bitrate := f.Width, f.Height, f.Bitrate

	preset = cfg.Preset1080p
	quality := 20

	uhd := width > 1920 || height > 1080
	if uhd {
		preset = cfg.Preset2160p
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

	if uhd {
		quality = clamp(quality, 17, 21)
	} else {
		quality = clamp(quality, 14, 21)
	}

	return preset, quality
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
				slog.Warn("Failed to remove partial copy", "path", dst, "err", rmErr)
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

func fileSize(p string) int64 {
	info, err := os.Stat(p)
	if err != nil {
		slog.Warn("Failed to stat file", "path", p, "err", err)
		return 0
	}
	return info.Size()
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
	slog.Debug("Prompt answered", "response", response)
	return response == "y" || response == "yes", nil
}

func closeCloser(c io.Closer) {
	if err := c.Close(); err != nil {
		slog.Warn("Failed to close", "err", err)
	}
}
