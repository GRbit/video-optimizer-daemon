package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The presets file nests presets inside folders; a flat lookup would miss
// everything the HandBrake GUI puts under "My Presets".
func TestPresetNames(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "presets.json")
	if err := os.WriteFile(good, []byte(`{
		"PresetList": [
			{"PresetName": "top-level"},
			{"Folder": true, "PresetName": "My Presets", "ChildrenArray": [
				{"PresetName": "slow-1080p-20"},
				{"Folder": true, "PresetName": "nested", "ChildrenArray": [
					{"PresetName": "deep"}
				]}
			]}
		]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"PresetList": [`), 0o644); err != nil {
		t.Fatal(err)
	}

	names, err := presetNames(good)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"top-level", "slow-1080p-20", "deep"} {
		if !names[want] {
			t.Errorf("preset %q not found in %v", want, names)
		}
	}
	if names["My Presets"] || names["nested"] {
		t.Errorf("folder names must not count as presets: %v", names)
	}

	if _, err := presetNames(broken); err == nil {
		t.Error("broken JSON should be an error")
	}
	if _, err := presetNames(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file should be an error")
	}
}
