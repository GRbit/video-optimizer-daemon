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

// encoder runs HandBrakeCLI. Its whole interface is run(); which preset and
// CRF the source gets, which binary, which arguments, what happens to stdout
// and stderr and how the process is supervised are implementation.
type encoder struct {
	presetsPath string
	preset1080p string
	preset2160p string
	hook        processHook
}

func newEncoder(cfg Config, window *workWindow) encoder {
	e := encoder{
		presetsPath: cfg.HandbrakePresetsPath,
		preset1080p: cfg.Preset1080p,
		preset2160p: cfg.Preset2160p,
	}
	if window != nil {
		e.hook = pauseOutsideWindow(window)
	}
	return e
}

// selectEncoding picks the preset family by resolution and the CRF from the
// source's resolution and bitrate. The CRF is passed to HandBrake with -q, so
// the preset itself only needs to describe the encoder settings.
func (e encoder) selectEncoding(f VideoFacts) (preset string, crf int) {
	width, height, bitrate := f.Width, f.Height, f.Bitrate

	preset = e.preset1080p
	quality := 20

	uhd := width > 1920 || height > 1080
	if uhd {
		preset = e.preset2160p
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

// stderrTailLines is how much of HandBrake's stderr is kept for the error log
// when it fails; the full stream is already on debug.
const stderrTailLines = 30

// run discards HandBrake's stdout: it carries only the progress line
// rewritten with carriage returns, which grows container logs by megabytes
// per movie. Everything useful (encoder settings, errors) is on stderr.
func (e encoder) run(ctx context.Context, facts VideoFacts, input, output string) error {
	preset, crf := e.selectEncoding(facts)
	slog.Info("Selected encoding", "preset", preset, "crf", crf)

	args := []string{
		"--preset-import-file", e.presetsPath,
		"-Z", preset,
		"-q", strconv.Itoa(crf),
		"-i", input,
		"-o", output,
		"--format", "mkv",
	}
	slog.Debug("Running HandBrakeCLI", "args", args)

	cmd := exec.CommandContext(ctx, handbrakeBin, args...)
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
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		slog.Debug("HandBrakeCLI stderr", "line", line)
		tail = append(tail, line)
		if len(tail) > stderrTailLines {
			tail = tail[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		// An oversized line stops the scanner; keep draining so HandBrake
		// never blocks on a full pipe and Wait can return.
		slog.Warn("HandBrakeCLI stderr read stopped", "err", err)
		_, _ = io.Copy(io.Discard, stderr)
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
		sync := func() {
			inside := w.contains(time.Now())
			switch {
			case !inside && !paused:
				if err := proc.Signal(syscall.SIGSTOP); err != nil {
					slog.Debug("SIGSTOP failed", "pid", proc.Pid, "err", err)
					return
				}
				paused = true
				slog.Info("Work interval closed, encoder paused", "pid", proc.Pid, "resume_at", w.nextChange(time.Now()).Format(time.RFC3339))
			case inside && paused:
				if err := proc.Signal(syscall.SIGCONT); err != nil {
					slog.Debug("SIGCONT failed", "pid", proc.Pid, "err", err)
					return
				}
				paused = false
				slog.Info("Work interval opened, encoder resumed", "pid", proc.Pid)
			}
		}

		// The process may already be outside the interval when it starts;
		// waiting for the next boundary would let it run the whole closed
		// period.
		sync()
		for {
			next := w.nextChange(time.Now())
			select {
			case <-ctx.Done():
				return
			// The extra second lands safely inside the next minute.
			case <-time.After(time.Until(next) + time.Second):
			}
			sync()
		}
	}
}
