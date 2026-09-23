package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// The external tools, named once: the runners below and the start-up check
// in validate.go share these, so the two can never disagree.
const (
	handbrakeBin = "HandBrakeCLI"
	mkvmergeBin  = "mkvmerge"
	mediainfoBin = "mediainfo"
)

var requiredTools = []string{handbrakeBin, mkvmergeBin, mediainfoBin}

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

func getMkvMergeInfo(path string) (*MkvMergeOutput, error) {
	var data MkvMergeOutput
	if err := runJSON(mkvmergeBin, []string{"-J", path}, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func runMkvmerge(ctx context.Context, args []string) error {
	slog.Debug("Running mkvmerge", "args", args)

	cmd := exec.CommandContext(ctx, mkvmergeBin, args...)
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
