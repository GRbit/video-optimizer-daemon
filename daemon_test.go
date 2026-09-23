package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The orchestration rules (largest first, one conversion per pass, every
// looked-at file recorded, interrupted file left alone) are checked here with
// probe and convert replaced by closures, so no external tool is needed.
func TestProcessNextOrchestration(t *testing.T) {
	captureLogs(t)
	media := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	for name, size := range map[string]int{"big.mkv": 300, "hevc.mkv": 200, "bad.mkv": 150, "small.mkv": 100} {
		p := filepath.Join(media, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{MediaDir: media, MinAge: time.Hour, StatePath: filepath.Join(media, stateFileName)}
	state, err := loadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}

	var converted []string
	results := map[string]convertResult{}
	convertErr := map[string]error{}
	d := &Daemon{
		cfg:   cfg,
		state: state,
		probe: func(path string) (VideoFacts, error) {
			switch filepath.Base(path) {
			case "hevc.mkv":
				return VideoFacts{CodecID: "V_MPEGH/ISO/HEVC"}, nil
			case "bad.mkv":
				return VideoFacts{}, errors.New("unreadable")
			}
			return VideoFacts{CodecID: "V_MPEG4/ISO/AVC"}, nil
		},
		convert: func(ctx context.Context, path string, facts VideoFacts) (convertResult, error) {
			converted = append(converted, filepath.Base(path))
			return results[filepath.Base(path)], convertErr[filepath.Base(path)]
		},
	}

	results["big.mkv"] = convertGainTooSmall
	processed, err := d.processNext(context.Background())
	if !processed || err != nil {
		t.Fatalf("pass 1: processed=%v err=%v", processed, err)
	}
	if len(converted) != 1 || converted[0] != "big.mkv" {
		t.Errorf("pass 1 should convert only the largest file, got %v", converted)
	}
	if got, _ := state.Get(filepath.Join(media, "big.mkv")); got != (StateEntry{Outcome: OutcomeSmallGain}) {
		t.Errorf("big.mkv recorded as %+v, want small_gain", got)
	}
	if state.Len() != 1 {
		t.Errorf("pass 1 stops at the first convertible file; state has %d entries, want 1", state.Len())
	}

	// Pass 2 walks past the HEVC file and the unreadable one, records both,
	// and converts the next candidate, which fails.
	convertErr["small.mkv"] = errors.New("boom")
	processed, err = d.processNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("pass 2: processed=%v err=%v, want a failed conversion", processed, err)
	}
	expect := map[string]StateEntry{
		"hevc.mkv":  {Outcome: OutcomeSkippedHEVC},
		"bad.mkv":   {Outcome: OutcomeFailed, Error: "probe: unreadable"},
		"small.mkv": {Outcome: OutcomeFailed, Error: "boom"},
	}
	for name, want := range expect {
		if got, ok := state.Get(filepath.Join(media, name)); !ok || got != want {
			t.Errorf("%s: recorded %+v (ok=%v), want %+v", name, got, ok, want)
		}
	}
	if len(converted) != 2 {
		t.Errorf("skipped files must not reach convert, calls: %v", converted)
	}

	processed, err = d.processNext(context.Background())
	if processed || err != nil {
		t.Errorf("pass 3 should find nothing: processed=%v err=%v", processed, err)
	}

	// A conversion interrupted by shutdown is not the file's fault.
	late := filepath.Join(media, "late.mkv")
	if err := os.WriteFile(late, make([]byte, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(late, old, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.convert = func(ctx context.Context, path string, facts VideoFacts) (convertResult, error) {
		cancel()
		return convertReplaced, errors.New("killed")
	}
	if _, err := d.processNext(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("interrupted pass should return the context error, got %v", err)
	}
	if state.Has(late) {
		t.Error("interrupted file must not be recorded")
	}
}
