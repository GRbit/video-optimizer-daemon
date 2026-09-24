package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The encoder's interface is run(facts): the preset family and the CRF it
// derives must reach HandBrakeCLI's argv. A fake binary dumps its arguments.
func TestEncoderRunArgs(t *testing.T) {
	captureLogs(t)
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FAKE_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(bin, handbrakeBin), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_ARGS", argsFile)

	enc := encoder{presetsPath: "/presets.json", preset1080p: "hd", preset2160p: "uhd"}
	// 4K at 13 Mbps: 2160p family, CRF 20 +1 (very large) -2 (high bitrate) = 19.
	facts := VideoFacts{Width: 3840, Height: 2160, Bitrate: 13_000_000}
	if err := enc.run(context.Background(), facts, "/in.mkv", "/out.mkv"); err != nil {
		t.Fatalf("run: %v", err)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := " " + strings.Join(strings.Split(strings.TrimSpace(string(raw)), "\n"), " ") + " "
	for _, want := range []string{" --preset-import-file /presets.json ", " -Z uhd ", " -q 19 ", " -i /in.mkv ", " -o /out.mkv ", " --format mkv "} {
		if !strings.Contains(got, want) {
			t.Errorf("argv %q lacks %q", got, want)
		}
	}
}
