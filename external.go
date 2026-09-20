package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

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

// runJSON runs an external tool that prints JSON on stdout and decodes it
// into dst. Both streams go to the debug log; on failure stderr goes to error.
func runJSON(name string, args []string, dst any) error {
	slog.Debug("Running "+name, "args", args)

	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		slog.Error(name+" failed", "args", args, "err", err, "stdout", string(out), "stderr", stderr.String())
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	slog.Debug(name+" output", "args", args, "stdout", string(out), "stderr", stderr.String())

	if err := json.Unmarshal(out, dst); err != nil {
		return fmt.Errorf("%s: unmarshal JSON: %w", name, err)
	}
	return nil
}

func getMediaInfo(path string) (*MediaInfoOutput, error) {
	var data MediaInfoOutput
	if err := runJSON("mediainfo", []string{"--fullscan", "--Output=JSON", path}, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func getMkvMergeInfo(path string) (*MkvMergeOutput, error) {
	var data MkvMergeOutput
	if err := runJSON("mkvmerge", []string{"-J", path}, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func runMkvmerge(ctx context.Context, args []string) error {
	slog.Debug("Running mkvmerge", "args", args)

	cmd := exec.CommandContext(ctx, "mkvmerge", args...)
	out, err := cmd.CombinedOutput()

	// mkvmerge exits with 1 when the output was written but warnings were
	// printed; only 2 means the mux actually failed.
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		slog.Debug("mkvmerge output", "output", string(out))
		return nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		slog.Warn("mkvmerge finished with warnings", "output", string(out))
		return nil
	default:
		slog.Error("mkvmerge failed", "err", err, "output", string(out))
		return err
	}
}

// stderrTailLines is how much of HandBrake's stderr is kept for the error log
// when it fails; the full stream is already on debug.
const stderrTailLines = 30

// runHandbrakeCLI discards HandBrake's stdout: it carries only the progress
// line rewritten with carriage returns, which grows container logs by megabytes
// per movie. Everything useful (encoder settings, errors) is on stderr.
func runHandbrakeCLI(ctx context.Context, cfg Config, input, output, preset string, crf int, window *workWindow) error {
	args := []string{
		"--preset-import-file", cfg.HandbrakePresetsPath,
		"-Z", preset,
		"-q", strconv.Itoa(crf),
		"-i", input,
		"-o", output,
		"--format", "mkv",
	}
	slog.Debug("Running HandBrakeCLI", "args", args)

	cmd := exec.CommandContext(ctx, "HandBrakeCLI", args...)
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start HandBrakeCLI: %w", err)
	}
	slog.Debug("HandBrakeCLI started", "pid", cmd.Process.Pid)

	superviseCtx, stopSupervise := context.WithCancel(ctx)
	defer stopSupervise()
	go window.supervise(superviseCtx, cmd.Process)

	// The pipe must be drained before Wait, and reading it here also blocks
	// until HandBrake closes stderr, i.e. exits.
	var tail []string
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		slog.Debug("HandBrakeCLI stderr", "line", line)
		tail = append(tail, line)
		if len(tail) > stderrTailLines {
			tail = tail[1:]
		}
	}

	if err := cmd.Wait(); err != nil {
		slog.Error("HandBrakeCLI failed", "err", err, "stderr_tail", strings.Join(tail, "\n"))
		return err
	}
	return nil
}
