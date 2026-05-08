# video-optimizer-daemon 

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

A video file is eligible for processing if it satisfies all of the following:

- File extension is one of: `.mkv`, `.mp4`, `.avi`, `.mov`, `.m4v`, `.webm`,
  or `.ts`.
- File modification time is older than 1 month.
- Video track codec is not already an optimized format (HEVC/H.265, AV1, AV2, VVC,
  DVHE, HVC1, HVC2).

### File Selection

When scanning a directory, the daemon selects the **largest** eligible video
file (by file size). When using a media list file, the first eligible file in
the list is selected.

### Transcoding

The daemon uses **HandBrakeCLI** with a preset selected dynamically based on
the source video properties:

- Resolution: `1080p` or `2160p` presets.
- Quality (CRF): Adjusted per resolution and bitrate band.
- Bitrate considerations: High bitrate (> 5 Mbps) reduces CRF; low bitrate
  (< 1.5 Mbps) increases CRF.

The preset name format is: *mode*-*resolution*-*crf* (e.g. `slow-1080p-20`).

### Post-Processing

After transcoding, the daemon performs the following steps:

1. **Audio deduplication** -- Detects duplicate audio tracks by language using
   **mkvmerge** and remuxes to keep only one track per language.
2. **Sidecar merge** -- Finds sidecar files sharing the same base filename and
   merges them into the output. Supported extensions: `.ass`, `.srt`, `.mka`.
3. **File replacement** -- Renames the optimized file to replace the original,
   updating sibling file names accordingly. The output filename is derived from
   the original with `.265.mkv` appended (or `.flac`/`.aac` replaced with
   `.ogg`).

### Prompt Mode

When `-prompt` is enabled, the daemon pauses before each step requiring a
decision:

- Before transcoding: asks whether to start conversion.
- After transcoding: asks whether to replace the original file, showing
  original and new file sizes.

The user must type `y` or `yes` to proceed.

## OPTIONS

**-prompt**
: Ask for confirmation before replacing original files.

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
: Overrides `-prompt`. Must be a boolean string.

**MEDIA_LIST_PATH**
: Overrides `-media-list`.

**TEMP_DIR**
: Overrides `-tmp`.
