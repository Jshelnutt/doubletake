# doubletake-win (Windows port)

Fork of [omarroth/doubletake](https://github.com/omarroth/doubletake) (LGPL-3.0) that
also runs on Windows 10/11. The AirPlay protocol code (pairing, FairPlay SAP,
encrypted H.264 mirroring, audio) is unchanged; only capture is new:

| | Linux (upstream) | Windows (this branch) |
|---|---|---|
| Screen | GStreamer ximagesrc / PipeWire | `ffmpeg.exe` ddagrab (falls back to gdigrab) |
| Encoder | GStreamer x264/NVENC/VA-API | ffmpeg h264_nvenc / h264_qsv / h264_amf / libx264 (auto-probed) |
| Sound | GStreamer pulsesrc/pipewiresrc | native WASAPI loopback of the default speakers (no Stereo Mix needed) |

## Build (from any OS)
```
GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" -o doubletake.exe ./cmd/doubletake
```
Put `ffmpeg.exe` (e.g. `winget install Gyan.FFmpeg`, any 6.1+ build) next to
`doubletake.exe` or on PATH, or set `DOUBLETAKE_FFMPEG`.

## Use
```
doubletake.exe -target 192.168.1.50 -pair -pair-only      # type the PIN shown on the TV
doubletake.exe -target 192.168.1.50                       # mirror; Ctrl+C to stop
```
New flags: `-pair-only` (exit after pairing), `-quit-on-stdin-eof` (clean stop for GUI
front ends). Environment: `DOUBLETAKE_CAPTURE=ffmpeg|gstreamer`,
`DOUBLETAKE_AUDIO=ffmpeg|wasapi|gstreamer`, `DOUBLETAKE_FFMPEG_INPUT`,
`DOUBLETAKE_FFMPEG_AUDIO_INPUT`, `DOUBLETAKE_FFMPEG`. Windows supports H.264 only
(no HEVC yet). Daemon mode is not supported on Windows.

## Status
Tested on Linux against `doubletake-test-receiver` using the ffmpeg backend (test
pattern and x11grab) with ffmpeg audio: pairing, FairPlay, video and audio all flow.
Windows binary cross-compiles; ddagrab/gdigrab and WASAPI paths are not yet tested on
real Windows hardware or a real Apple TV.
