package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// validateConfig catches configuration mistakes before the first scan. A
// missing tool or preset would otherwise surface as "failed" on every file
// in the library, one per minute, and those records are wrong: the files are
// fine, the setup is not.
func validateConfig(cfg Config) error {
	var problems []string

	for _, tool := range requiredTools {
		if _, err := exec.LookPath(tool); err != nil {
			problems = append(problems, fmt.Sprintf("%s not found in PATH", tool))
		}
	}

	names, err := presetNames(cfg.HandbrakePresetsPath)
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		for _, p := range []string{cfg.Preset1080p, cfg.Preset2160p} {
			if !names[p] {
				problems = append(problems, fmt.Sprintf("preset %q not found in %s", p, cfg.HandbrakePresetsPath))
			}
		}
	}

	if cfg.PromptMode && !isTerminal(os.Stdin) {
		problems = append(problems, "-prompt requires an interactive terminal on stdin")
	}

	for _, dir := range []string{cfg.TempDirPath, filepath.Dir(cfg.StatePath)} {
		if err := checkWritable(dir); err != nil {
			problems = append(problems, err.Error())
		}
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// presetNames returns every PresetName in a HandBrake presets file, walking
// folders recursively. HandBrake's own listing (-z) would do the same job but
// gives no usable error when the file itself is broken.
func presetNames(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read presets file: %w", err)
	}

	var file struct {
		PresetList []presetNode `json:"PresetList"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse presets file %s: %w", path, err)
	}

	names := make(map[string]bool)
	var walk func([]presetNode)
	walk = func(nodes []presetNode) {
		for _, n := range nodes {
			if n.Folder {
				walk(n.Children)
				continue
			}
			names[n.Name] = true
		}
	}
	walk(file.PresetList)
	return names, nil
}

type presetNode struct {
	Name     string       `json:"PresetName"`
	Folder   bool         `json:"Folder"`
	Children []presetNode `json:"ChildrenArray"`
}

// isTerminal asks the kernel for terminal attributes. ModeCharDevice is not
// enough: /dev/null is a character device too, and a service started with
// stdin from /dev/null would pass that check and then read EOF on every prompt.
func isTerminal(f *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

// checkWritable creates and removes a probe file, which is the only reliable
// test on a read-only bind mount where the permission bits still say rw.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".video-optimizer-probe-*")
	if err != nil {
		return fmt.Errorf("directory %s is not writable: %w", dir, err)
	}
	name := f.Name()
	closeCloser(f)
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove probe file %s: %w", name, err)
	}
	return nil
}
