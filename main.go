package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
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
}

// Daemon is the orchestration: which file next, one task per pass, what to
// record. The slow parts come in as two function values so this logic can be
// driven in tests without mediainfo or an encoder on PATH.
type Daemon struct {
	scan    scanSettings
	state   *State
	window  *workWindow
	probe   func(ctx context.Context, path string) (VideoFacts, error)
	convert func(ctx context.Context, path string, facts VideoFacts) (convertResult, error)
}

// newDaemon is where Config is taken apart: every module below gets only the
// settings it uses.
func newDaemon(cfg Config, state *State, window *workWindow) *Daemon {
	confirm := alwaysConfirm
	if cfg.PromptMode {
		confirm = terminalConfirm
	}
	conv := converter{
		tempDir: cfg.TempDirPath,
		encode:  newEncoder(cfg, window).run,
		mux:     muxWithMkvmerge,
		probe:   probeVideo,
		confirm: confirm,
	}
	return &Daemon{
		scan:    scanSettings{mediaDir: cfg.MediaDir, mediaListPath: cfg.MediaListPath, minAge: cfg.MinAge},
		state:   state,
		window:  window,
		probe:   probeVideo,
		convert: conv.convert,
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
	candidates, err := findCandidates(ctx, d.scan, d.state.Has)
	if err != nil {
		return false, fmt.Errorf("search files: %w", err)
	}

	for _, path := range candidates {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		facts, err := d.probe(ctx, path)
		if err != nil {
			slog.Warn("Skipping file: probe failed", "path", path, "err", err)
			d.record(path, StateEntry{Outcome: OutcomeFailed, Error: "probe: " + err.Error()})
			continue
		}
		if facts.AlreadyOptimized() {
			slog.Debug("Skipping file: already optimized", "path", path, "codec", facts.CodecID)
			d.record(path, StateEntry{Outcome: OutcomeSkippedHEVC})
			continue
		}

		slog.Info("Found target candidate", "path", path)
		// The probe walk above can outlast the work interval on a first run
		// over a library full of HEVC files; an encode must never start
		// outside it.
		if !d.window.waitUntilOpen(ctx) {
			return false, ctx.Err()
		}
		res, err := d.convert(ctx, path, facts)
		if ctx.Err() != nil {
			// Shutdown interrupted the task; leave it eligible for the next start.
			return false, ctx.Err()
		}
		d.record(path, outcomeOf(res, err))
		return true, err
	}

	return false, nil
}

// record logs a failed write instead of returning it: the entry is kept in
// memory either way, and the loop has nothing better to do with the error.
func (d *Daemon) record(path string, e StateEntry) {
	if err := d.state.Record(path, e); err != nil {
		slog.Error("Failed to write state file", "path", d.state.path, "err", err)
	}
}

// lowerPriority puts the whole daemon on nice 19 so every child (HandBrake,
// mkvmerge, mediainfo) inherits it and no external "nice" binary is needed.
//
// Linux nice is per thread and the Go runtime already has several by now, so
// PRIO_PROCESS would cover only the calling thread and children started from
// other goroutines would keep nice 0. PRIO_PGRP with pid 0 covers every thread
// of this process (and anything else in its process group).
func lowerPriority() {
	if err := syscall.Setpriority(syscall.PRIO_PGRP, 0, 19); err != nil {
		slog.Warn("Failed to lower process priority", "err", err)
		return
	}
	slog.Debug("Process priority lowered", "nice", 19)
}
