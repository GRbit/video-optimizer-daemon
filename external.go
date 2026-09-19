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
	"regexp"
	"strconv"
	"strings"
	"sync"
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

func getMediaInfo(path string) (*MediaInfoOutput, error) {
	args := []string{"--fullscan", "--Output=JSON", path}
	slog.Debug("Running mediainfo", "args", args)

	var stderr bytes.Buffer
	cmd := exec.Command("mediainfo", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		slog.Error("mediainfo failed", "path", path, "err", err, "stderr", stderr.String())
		return nil, err
	}
	slog.Debug("mediainfo output", "path", path, "stdout", string(out), "stderr", stderr.String())

	var data MediaInfoOutput
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func getMkvMergeInfo(path string) (*MkvMergeOutput, error) {
	args := []string{"-J", path}
	slog.Debug("Running mkvmerge", "args", args)

	var stderr bytes.Buffer
	cmd := exec.Command("mkvmerge", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		slog.Error("mkvmerge -J failed", "path", path, "err", err, "stdout", string(out), "stderr", stderr.String())
		return nil, fmt.Errorf("mkvmerge -J %s: %w", path, err)
	}
	slog.Debug("mkvmerge -J output", "path", path, "stdout", string(out), "stderr", stderr.String())

	var data MkvMergeOutput
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("mkvmerge: unmarshal JSON '%s': %w", string(out), err)
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

// handbrakeProgress matches HandBrakeCLI's stdout progress line, e.g.
// "Encoding: task 1 of 1, 12.34 % (45.67 fps, avg 40.00 fps, ETA 00h12m34s)".
var handbrakeProgress = regexp.MustCompile(`Encoding: task (\d+) of (\d+), ([\d.]+) %\s*(.*)`)

// stderrTailLines is how much of HandBrake's stderr is kept for the error log
// when it fails; the full stream is already on debug.
const stderrTailLines = 30

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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
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

	var (
		wg   sync.WaitGroup
		tail []string
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		reportHandbrakeProgress(stdout)
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			slog.Debug("HandBrakeCLI stderr", "line", line)
			tail = append(tail, line)
			if len(tail) > stderrTailLines {
				tail = tail[1:]
			}
		}
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		slog.Error("HandBrakeCLI failed", "err", err, "stderr_tail", strings.Join(tail, "\n"))
		return err
	}
	return nil
}

// reportHandbrakeProgress turns HandBrake's carriage-return progress stream into
// one info line per 10 percent and one debug line per percent. Unparsed stdout
// goes to debug as is.
func reportHandbrakeProgress(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Split(scanCRLF)

	lastInfo, lastDebug := -1, -1
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		m := handbrakeProgress.FindStringSubmatch(line)
		if m == nil {
			slog.Debug("HandBrakeCLI stdout", "line", line)
			continue
		}
		pct, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		p := int(pct)
		attrs := []any{"task", m[1] + "/" + m[2], "percent", p, "detail", strings.Trim(m[4], "()")}
		switch {
		case p/10 > lastInfo:
			lastInfo = p / 10
			lastDebug = p
			slog.Info("HandBrake progress", attrs...)
		case p > lastDebug:
			lastDebug = p
			slog.Debug("HandBrake progress", attrs...)
		}
	}
}

// scanCRLF splits on either \r or \n: HandBrake rewrites the progress line
// with a bare carriage return.
func scanCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
