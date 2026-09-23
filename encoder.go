package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processHook runs alongside a started encoder process until ctx is done.
// It is how lifecycle policy (work hours, later maybe load or a user signal)
// reaches the process without the conversion task knowing about it.
type processHook func(ctx context.Context, proc *os.Process)

// encoder runs HandBrakeCLI. Its whole interface is run(); which binary,
// which arguments, what happens to stdout and stderr and how the process is
// supervised are implementation.
type encoder struct {
	presetsPath string
	hook        processHook
}

func newEncoder(cfg Config, window *workWindow) encoder {
	e := encoder{presetsPath: cfg.HandbrakePresetsPath}
	if window != nil {
		e.hook = pauseOutsideWindow(window)
	}
	return e
}

// stderrTailLines is how much of HandBrake's stderr is kept for the error log
// when it fails; the full stream is already on debug.
const stderrTailLines = 30

// run discards HandBrake's stdout: it carries only the progress line
// rewritten with carriage returns, which grows container logs by megabytes
// per movie. Everything useful (encoder settings, errors) is on stderr.
func (e encoder) run(ctx context.Context, input, output, preset string, crf int) error {
	args := []string{
		"--preset-import-file", e.presetsPath,
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

	if e.hook != nil {
		hookCtx, stopHook := context.WithCancel(ctx)
		defer stopHook()
		go e.hook(hookCtx, cmd.Process)
	}

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

// pauseOutsideWindow is the processHook adapter for work hours: SIGSTOP when
// the window closes, SIGCONT when it opens again. Killing an encode hours in
// at the window edge and starting over is what this avoids.
func pauseOutsideWindow(w *workWindow) processHook {
	return func(ctx context.Context, proc *os.Process) {
		paused := false
		for {
			next := w.nextChange(time.Now())
			select {
			case <-ctx.Done():
				return
			// The extra second lands safely inside the next minute.
			case <-time.After(time.Until(next) + time.Second):
			}

			inside := w.contains(time.Now())
			switch {
			case !inside && !paused:
				if err := proc.Signal(syscall.SIGSTOP); err != nil {
					slog.Debug("SIGSTOP failed", "pid", proc.Pid, "err", err)
					continue
				}
				paused = true
				slog.Info("Work window closed, encoder paused", "pid", proc.Pid, "resume_at", w.nextChange(time.Now()).Format(time.RFC3339))
			case inside && paused:
				if err := proc.Signal(syscall.SIGCONT); err != nil {
					slog.Debug("SIGCONT failed", "pid", proc.Pid, "err", err)
					continue
				}
				paused = false
				slog.Info("Work window opened, encoder resumed", "pid", proc.Pid)
			}
		}
	}
}
