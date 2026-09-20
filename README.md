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
video-optimizer [-prompt] [-media-dir=directory] [-media-list=path]
                [-handbrake-conf=path] [-preset-1080p=name] [-preset-2160p=name]
                [-tmp=directory] [-state=path] [-min-age=duration]
                [-work-hours=HH:MM-HH:MM] [-log-level=level]
```

## DESCRIPTION

**video-optimizer** is a continuous daemon that scans a media directory (or a
list of absolute file paths) for eligible video files and transcodes them to
the more efficient HEVC/x265 codec using HandBrakeCLI.

Each cycle the daemon collects all eligible files in one pass, orders them by
size (largest first) and takes the first one that is not already HEVC. That
file is transcoded, assembled with mkvmerge, verified and put in place of the
original. Then the cycle starts over. Every file the daemon has looked at is
recorded in a state file, so nothing is examined twice, even across restarts.

**Requirements**:
* mediainfo
* mkvmerge
* HandBrakeCLI
* A HandBrake presets file (the GUI's `presets.json` works) containing the
  two base presets, see [Transcoding](#transcoding).

### Eligibility

When scanning a directory, a video file is a candidate if it satisfies all of
the following:

- File extension is one of: `.mkv`, `.mp4`, `.avi`, `.mov`, `.m4v`, `.webm`,
  or `.ts`.
- File modification time is older than `-min-age` (default 30 days). The
  threshold is recomputed on every scan.
- The file has no entry in the state file.

When using a media list file, the extension and modification time checks are
not applied: any existing regular file from the list without a state entry is
a candidate. Lines are taken in file order.

A candidate whose video track is already an optimized format (HEVC/H.265,
AV1, AV2, VVC, DVHE, DVH1, HVC1, HVC2) is recorded as `skipped_hevc` and the
next candidate is tried within the same cycle; no extra scan is needed.

### Transcoding

HandBrakeCLI runs with `-Z <preset> -q <crf> --format mkv`. Only two presets
are needed:

- `-preset-1080p` (default `slow-1080p-20`) for sources up to 1920x1080.
- `-preset-2160p` (default `slow-2160p-20`) for anything larger.

The preset should describe the video and audio encoder settings. Its own
quality value is overridden by `-q`. Subtitles, chapters and attachments in
the preset do not matter: only video and audio are taken from HandBrake's
output.

The CRF passed with `-q` is derived from the source:

- Base value 20.
- Sources below 720p/480p/360p lower the CRF step by step; very large 2160p
  sources (2100 px wide or 1200 px tall and up) raise it.
- High bitrate (> 5 Mbps) lowers CRF, very high bitrate (> 12 Mbps) lowers it
  further; low bitrate (< 1.5 Mbps) raises it.
- The result is clamped to 14-21 for 1080p and 17-21 for 2160p.

### Assembling the result

The final file is built by a single mkvmerge call:

```
mkvmerge -o final.mkv [--audio-tracks ids] --no-subtitles --no-chapters --no-attachments handbrake.mkv \
         --no-video --no-audio original [sidecars...]
```

- Video and audio come from HandBrake's output. If HandBrake produced several
  audio tracks with the same language, only the first per language is kept.
- Subtitles, chapters, attachments (for example ASS fonts) and tags come from
  the original file, so nothing the original carried is lost.
- Sidecar files whose names start with the original's base name and have one
  of the extensions `.ass`, `.srt`, `.mka` are merged in as extra sources.

### Verification and replacement

Before the original is touched, the duration of the assembled file (as
reported by mediainfo) must be within 10 seconds of the original's. If it is
not, the task fails, the original stays and the file is recorded as `failed`.

The assembled file must also be at least 10% smaller than the original.
Otherwise the original is kept and the file is recorded as `small_gain`: a
re-encode always adds artifacts, and a few percent of disk space is not worth
them. This check runs before the replacement prompt in prompt mode.

Then the daemon:

1. Moves the assembled file next to the original under its new name
   (falling back to copy, fsync and size check when rename crosses devices).
2. Gives it the permission bits of the original. Owner and group are not
   changed, so run the daemon as the user who owns the library.
3. Deletes the original and every sidecar that was merged.

Other files that share the original's name prefix (`.nfo`, `.jpg`, `Movie
2.mkv`) are never touched or renamed.

The new name is derived from the original's file name only; the directory is
never changed. If the name contains a codec token `x264`, `h264` or `h.264`
(any case), it becomes `265`; otherwise `.x265` is appended. The extension is
always `.mkv`. The audio codec tokens `flac` and `aac` become `ogg` when they
stand alone in the name (`Movie.AAC.mkv` -> `Movie.OGG.x265.mkv`, but `Isaac
Asimov.mkv` keeps its spelling).

### State file

The daemon keeps a JSON journal of every file it has decided about. Default
location: `.video-optimizer-state.json` inside the media directory, or next to
the media list file when one is used. Override with `-state` / `STATE_PATH`.

```json
{
  "/media/Movie.mkv": "done",
  "/media/Series/S01E01.mkv": "skipped_hevc",
  "/media/Other.mkv": "failed: run handbrake: exit status 1"
}
```

Outcomes are `done`, `skipped_hevc`, `declined` (refused in prompt mode),
`small_gain` (converted, but less than 10% smaller, original kept) and
`failed: <error text>`. A file with an entry is never offered again.
To retry a file, delete its entry; the file is written with indentation for
exactly that purpose. Entries for files that no longer exist are harmless.

The file is rewritten atomically after every task and during long scans.

### Prompt mode

When `-prompt` is enabled, the daemon pauses before each step requiring a
decision:

- Before transcoding: asks whether to start conversion.
- After the result is assembled and verified: asks whether to replace the
  original file, showing original and new file sizes.

The user must type `y` or `yes` to proceed. Any other answer records the file
as `declined`, which is how a file is permanently excluded (see
[State file](#state-file)).

### Work hours

With `-work-hours=23:00-07:00` (local time, may cross midnight) new tasks are
started only inside the window; outside it the daemon sleeps until the window
opens. A HandBrake encode that is still running when the window closes is
paused with SIGSTOP and resumed with SIGCONT when the window opens again, so
long encodes are never thrown away. mkvmerge and mediainfo are short and are
not paused.

### Scheduling and priority

After a successful task the next scan starts immediately. After a failed task
or an empty scan the daemon waits one minute.

At start the daemon lowers its own priority to nice 19; HandBrakeCLI, mkvmerge
and mediainfo inherit it.

### Logging

Logs go to stderr via `log/slog`. `info` covers start-up, the chosen file and
encoding parameters, HandBrake progress every 10 percent, and the outcome of
each task with sizes. `debug` adds every external command with its arguments,
every step of a task, HandBrake progress per percent, HandBrake's own stderr,
and the reason for every skipped file. HandBrake's raw progress stream is
never echoed.

## OPTIONS

**-prompt**
: Ask for confirmation before starting a conversion and before replacing
  original files.

**-media-dir=directory**
: Directory to scan for media files. Default: `/media`.

**-media-list=path**
: Path to a file listing media files (one absolute path per line). If set,
  the daemon uses this list instead of scanning a directory.

**-handbrake-conf=path**
: Path to HandBrake presets JSON file. Default:
  `~/.config/ghb/presets.json` (resolved at start). The daemon refuses to
  start if the home directory is unknown and no path is given.

**-preset-1080p=name**
: HandBrake preset for sources up to 1080p. Default: `slow-1080p-20`.

**-preset-2160p=name**
: HandBrake preset for sources above 1080p. Default: `slow-2160p-20`.

**-tmp=directory**
: Directory to use for temporary files. Current Linux systems can use RAM for
  temp file system, usually it's not enough for big media files. You can set it
  to a directory on a disk with enough free space. Default: system temp directory.

**-state=path**
: Path to the JSON state file. Default: `.video-optimizer-state.json` in the
  media directory, or next to the media list.

**-min-age=duration**
: Minimum time since last modification for a file to be eligible, as a Go
  duration (`720h`, `24h`, `30m`). Default: `720h` (30 days).

**-work-hours=HH:MM-HH:MM**
: Local time window in which encoding runs, e.g. `23:00-07:00`. Default:
  empty, work always.

**-log-level=level**
: `debug`, `info`, `warn` or `error`. Default: `info`.

## ENVIRONMENT VARIABLES

The following environment variables override their corresponding command-line
options:

**MEDIA_DIR**
: Overrides `-media-dir`.

**MEDIA_LIST_PATH**
: Overrides `-media-list`.

**HANDBRAKE_CONF**
: Overrides `-handbrake-conf`.

**PRESET_1080P**, **PRESET_2160P**
: Override `-preset-1080p` and `-preset-2160p`.

**PROMPT_MODE**
: Overrides `-prompt`. Must be a boolean string. Only takes effect when the
  `-prompt` flag is not set (it can enable prompt mode, not disable it).

**TEMP_DIR**
: Overrides `-tmp`.

**STATE_PATH**
: Overrides `-state`.

**MIN_AGE**
: Overrides `-min-age`.

**WORK_HOURS**
: Overrides `-work-hours`.

**LOG_LEVEL**
: Overrides `-log-level`.

## DOCKER

`docker-compose.yaml` builds the image and runs the daemon as `PUID:PGID`
(default `1000:1000`); set them to the owner of your media files, since the
daemon replaces files in place and cannot change ownership. Put the settings
in `.env`:

```
MEDIA_DIR=/srv/media
TEMP_DIR=/srv/tmp
PUID=1000
PGID=1000
# optional
GHB_DIR=/home/user/.config/ghb   # directory with presets.json, default ~/.config/ghb
MEDIA_LIST_PATH=/srv/lists/todo.txt
STATE_PATH=/srv/media/.video-optimizer-state.json
WORK_HOURS=23:00-07:00
TZ=Europe/Berlin                 # work hours are in the container's local time
LOG_LEVEL=info
```

The presets directory is mounted read-only at `/ghb` and `HANDBRAKE_CONF` is
set to `/ghb/presets.json`. In list mode the state file defaults to the media
directory, because a single-file bind mount has no writable parent inside the
container.
