package airplay

// Non-GStreamer PCM sources (doubletake-win). Each produces interleaved S16LE,
// 44.1 kHz stereo on a pipe, which AudioCapture's untimestamped path reads.
//
// Windows: native WASAPI loopback of the default playback device (no "Stereo
// Mix" or virtual cable needed). Test tone / DOUBLETAKE_AUDIO=ffmpeg: an ffmpeg
// child (DOUBLETAKE_FFMPEG_AUDIO_INPUT overrides its input arguments).

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

func useExternalAudio() bool {
	switch strings.ToLower(os.Getenv("DOUBLETAKE_AUDIO")) {
	case "ffmpeg", "wasapi":
		return true
	case "gstreamer", "gst":
		return false
	}
	return runtime.GOOS == "windows"
}

// startExternalPCM returns a PCM reader, an optional child process, and a
// channel that yields the producer's terminal error.
func startExternalPCM(ctx context.Context, testTone bool) (io.ReadCloser, *exec.Cmd, <-chan error, error) {
	mode := strings.ToLower(os.Getenv("DOUBLETAKE_AUDIO"))
	if !testTone && mode != "ffmpeg" && runtime.GOOS == "windows" {
		r, done, err := startWASAPILoopback(ctx)
		return r, nil, done, err
	}
	input := []string{"-re", "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=%d", audioSampleRate)}
	if custom := strings.TrimSpace(os.Getenv("DOUBLETAKE_FFMPEG_AUDIO_INPUT")); custom != "" && !testTone {
		input = strings.Fields(custom)
	}
	args := append([]string{"-hide_banner", "-loglevel", "warning", "-nostdin"}, input...)
	args = append(args, "-ac", fmt.Sprint(audioChannels), "-ar", fmt.Sprint(audioSampleRate),
		"-f", "s16le", "-flush_packets", "1", "pipe:1")
	dbg("[AUDIO] %s %s", FFmpegPath(), strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, FFmpegPath(), args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, _ := cmd.StderrPipe()
	wait, err := startGStreamerCommand(cmd)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("start ffmpeg audio: %w", err)
	}
	go logStderr("AUDIO-FFMPEG", stderr)
	return stdout, cmd, wait, nil
}

// StartSystemAudioPCM starts the platform system-audio capture on its own and
// returns raw S16LE 44.1 kHz stereo PCM. doubletake -pcm-stdout uses it so a
// front end can send the sound to a different speaker (e.g. HomePods) than the
// one receiving the picture.
func StartSystemAudioPCM(ctx context.Context, testTone bool) (io.ReadCloser, <-chan error, error) {
	r, _, done, err := startExternalPCM(ctx, testTone)
	return r, done, err
}
