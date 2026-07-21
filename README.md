# video-optimizer-daemon

[![Go Reference](https://pkg.go.dev/badge/github.com/grbit/video-optimizer-daemon.svg)](https://pkg.go.dev/github.com/grbit/video-optimizer-daemon)
[![Go Report Card](https://goreportcard.com/badge/github.com/grbit/video-optimizer-daemon)](https://goreportcard.com/report/github.com/grbit/video-optimizer-daemon)
[![Go Version](https://img.shields.io/github/go-mod/go-version/GRbit/video-optimizer-daemon)](go.mod)
[![License: AGPL-3.0](https://img.shields.io/github/license/GRbit/video-optimizer-daemon)](LICENSE)

Continuous video transcoding daemon. Scan media directories and automatically transcode video files to HEVC/x265 using HandBrakeCLI.

# DISCLAIMER

Purpose-built for a specific use case, not a generic solution.

## SYNOPSIS

```
video-optimizer [-prompt] [-media-dir=directory] [-handbrake-conf=path]
                [-media-list=path] [-tmp=directory]
```

## DESCRIPTION

**video-optimizer** is a continuous daemon that periodically scans a media
directory (or list of abs file paths) for eligible video files and transcodes
them to more efficient HEVC/x265 codec using HandBrakeCLI.

The daemon runs in a tight loop, selecting the largest eligible video file each
cycle, transcoding it, deduplicating audio tracks, merging sidecar subtitle and
audio files, and replacing the original with the optimized output.

**Requirements**:
* mediainfo
* mkvmerge
* HandBrakeCLI
* HandBrakeCLI profiles with a name format of *mode*-*resolution*-*crf* (e.g. `slow-1080p-20`),
  see `selectHandbrakePreset` function for details.

### Eligibility

When scanning a directory, a video file is eligible for processing if it
satisfies all of the following:

- File extension is one of: `.mkv`, `.mp4`, `.avi`, `.mov`, `.m4v`, `.webm`,
  or `.ts`.
- File modification time is older than 1 month.
- Video track codec is not already an optimized format (HEVC/H.265, AV1, AV2, VVC,
  DVHE, DVH1, HVC1, HVC2).

When using a media list file, the extension and modification time checks are
not applied: any existing regular file from the list that is not already
optimized is eligible.

### File Selection

When scanning a directory, the daemon selects the **largest** eligible video
file (by file size). When using a media list file, the first eligible file in
the list is selected.

### Transcoding

The daemon uses **HandBrakeCLI** with a preset selected dynamically based on
the source video properties:

- Resolution: `1080p` or `2160p` presets.
- Quality (CRF): Adjusted per resolution and bitrate band. Sources below
  720p/480p/360p lower the CRF step by step; very large 2160p sources raise it.
- Bitrate considerations: High bitrate (> 5 Mbps) reduces CRF, very high
  bitrate (> 12 Mbps) reduces it further; low bitrate (< 1.5 Mbps) increases
  CRF.
- The final CRF is clamped to 14-21 for `1080p` and 17-21 for `2160p`.

The preset name format is: *mode*-*resolution*-*crf* (e.g. `slow-1080p-20`).

### Post-Processing

After transcoding, the daemon performs the following steps:

1. **Audio deduplication** -- Detects duplicate audio tracks by language using
   **mkvmerge** and remuxes to keep only one track per language.
2. **Sidecar merge** -- Finds sidecar files whose names start with the original
   base filename and merges them into the output. Supported extensions: `.ass`,
   `.srt`, `.mka`. Merged sidecar files are deleted after a successful run.
3. **File replacement** -- Renames the optimized file to replace the original,
   updating sibling file names accordingly. The output filename is derived from
   the original: if the name contains `264`, it is replaced with `265`;
   otherwise `.x265.mkv` is appended. Any `flac`/`aac` in the name is replaced
   with `ogg` (case-preserving).

### Prompt Mode

When `-prompt` is enabled, the daemon pauses before each step requiring a
decision:

- Before transcoding: asks whether to start conversion.
- After transcoding: asks whether to replace the original file, showing
  original and new file sizes.

The user must type `y` or `yes` to proceed.

## OPTIONS

**-prompt**
: Ask for confirmation before starting a conversion and before replacing
  original files.

**-media-dir=directory**
: Directory to scan for media files. Default: `/media`.

**-handbrake-conf=path**
: Path to HandBrake presets JSON file. Default:
  `$HOME/.config/ghb/presets.json`.

**-media-list=path**
: Path to a file listing media files (one per line, absolute path). If set,
  the daemon scans this list instead of a directory.

**-tmp=directory**
: Directory to use for temporary files. Current Linux systems can use RAM for
  temp file system, usually it's not enough for big media files. You can set it
  to a directory on a disk with enough free space. Default: system temp directory.

## ENVIRONMENT VARIABLES

The following environment variables override their corresponding command-line
options:

**MEDIA_DIR**
: Overrides `-media-dir`.

**HANDBRAKE_CONF**
: Overrides `-handbrake-conf`.

**PROMPT_MODE**
: Overrides `-prompt`. Must be a boolean string. Only takes effect when the
  `-prompt` flag is not set (it can enable prompt mode, not disable it).

**MEDIA_LIST_PATH**
: Overrides `-media-list`.

**TEMP_DIR**
: Overrides `-tmp`.
