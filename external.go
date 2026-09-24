package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// The external tools, named once: the runners and the start-up check in
// validate.go share these, so the two can never disagree.
const (
	handbrakeBin = "HandBrakeCLI"
	mkvmergeBin  = "mkvmerge"
	mediainfoBin = "mediainfo"
)

var requiredTools = []string{handbrakeBin, mkvmergeBin, mediainfoBin}

// runJSON runs an external tool that prints JSON on stdout and decodes it
// into dst. Both streams go to the debug log; on failure stderr goes to error.
func runJSON(ctx context.Context, name string, args []string, dst any) error {
	slog.Debug("Running "+name, "args", args)

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
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
