package airplay

// FFmpeg capture backend (doubletake-win).
//
// Linux capture uses GStreamer, which is rarely present on Windows. This
// backend runs a bundled ffmpeg(.exe) that captures the desktop (ddagrab, with
// gdigrab as a fallback), scales/pads to the receiver canvas, encodes H.264 and
// writes raw Annex-B to stdout. The mirror sender's existing untimestamped
// path consumes that stream unchanged.
//
// It is selected automatically on Windows, or anywhere with
// DOUBLETAKE_CAPTURE=ffmpeg. DOUBLETAKE_FFMPEG_INPUT replaces the capture
// input arguments (e.g. "-f lavfi -i testsrc2=size=1280x720:rate=30"), and
// DOUBLETAKE_FFMPEG names the ffmpeg binary (default: ffmpeg next to the
// executable, then PATH).

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const capturePreparationFFmpeg capturePreparationKind = 100
const capturePreparationFFmpegTest capturePreparationKind = 101

func useFFmpegCapture() bool {
	switch strings.ToLower(os.Getenv("DOUBLETAKE_CAPTURE")) {
	case "ffmpeg":
		return true
	case "gstreamer", "gst":
		return false
	}
	return runtime.GOOS == "windows"
}

// FFmpegPath returns the ffmpeg binary used for capture.
func FFmpegPath() string {
	if p := os.Getenv("DOUBLETAKE_FFMPEG"); p != "" {
		return p
	}
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return name
}

func prepareFFmpegCapture(ctx context.Context, cfg CaptureConfig, test bool) (*CapturePreparation, error) {
	if err := ValidateVideoCodec(string(cfg.VideoCodec)); err != nil {
		return nil, err
	}
	if err := ValidateHWAccel(cfg.HWAccel); err != nil {
		return nil, err
	}
	if normalizeVideoCodec(cfg.VideoCodec) == VideoCodecHEVC && cfg.VideoCodec != VideoCodecAuto {
		return nil, fmt.Errorf("the ffmpeg capture backend supports H.264 only; use -video-codec h264 or auto")
	}
	if _, err := exec.LookPath(FFmpegPath()); err != nil {
		return nil, fmt.Errorf("ffmpeg not found (%s): put ffmpeg.exe next to doubletake.exe or set DOUBLETAKE_FFMPEG: %w", FFmpegPath(), err)
	}
	if _, err := selectFFmpegEncoder(cfg.HWAccel); err != nil {
		return nil, err
	}
	kind := capturePreparationFFmpeg
	if test {
		kind = capturePreparationFFmpegTest
	}
	return &CapturePreparation{ctx: ctx, cfg: cfg, kind: kind}, nil
}

var (
	ffmpegEncoderOnce  sync.Map // hwaccel -> []string
	ffmpegProbeResults sync.Map // encoder -> bool
)

func ffmpegEncoderWorks(name string) bool {
	if v, ok := ffmpegProbeResults.Load(name); ok {
		return v.(bool)
	}
	cmd := exec.Command(FFmpegPath(), "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=320x240:r=30:d=0.2",
		"-pix_fmt", "yuv420p", "-c:v", name, "-f", "null", "-")
	cmd.SysProcAttr = hiddenWindowAttr()
	out, err := cmd.CombinedOutput()
	ok := err == nil
	if !ok {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		log.Printf("[CAPTURE] encoder %s unavailable: %v %s", name, err, msg)
	}
	ffmpegProbeResults.Store(name, ok)
	return ok
}

// selectFFmpegEncoder returns the first working encoder for a -hwaccel value.
func selectFFmpegEncoder(hwaccel string) (string, error) {
	list, err := workingFFmpegEncoders(hwaccel)
	if err != nil {
		return "", err
	}
	return list[0], nil
}

// workingFFmpegEncoders returns every encoder for a -hwaccel value that passes
// a quick probe, best first. Capture falls through them if a real capture
// start fails (e.g. NVENC rejecting frames from an Intel-owned display).
func workingFFmpegEncoders(hwaccel string) ([]string, error) {
	var candidates []string
	switch hwaccel {
	case "nvenc":
		candidates = []string{"h264_nvenc"}
	case "vaapi": // on Windows: Intel Quick Sync / AMD AMF
		candidates = []string{"h264_qsv", "h264_amf", "h264_vaapi"}
	case "openh264":
		candidates = []string{"libopenh264"}
	case "none":
		candidates = []string{"libx264"}
	default:
		candidates = []string{"h264_nvenc", "h264_qsv", "h264_amf", "libx264", "libopenh264"}
	}
	var ok []string
	for _, c := range candidates {
		if ffmpegEncoderWorks(c) {
			ok = append(ok, c)
		}
	}
	if len(ok) == 0 {
		return nil, fmt.Errorf("no working ffmpeg H.264 encoder among %v", candidates)
	}
	return ok, nil
}

func ffmpegEncoderArgs(encoder string, bitrateKbps, fps int) []string {
	gop := keyframeIntervalFrames(fps)
	rate := []string{
		"-b:v", fmt.Sprintf("%dk", bitrateKbps),
		"-maxrate", fmt.Sprintf("%dk", bitrateKbps),
		"-bufsize", fmt.Sprintf("%dk", vbvBufferKbit(bitrateKbps, fps)),
		"-g", fmt.Sprint(gop), "-bf", "0",
	}
	var args []string
	switch encoder {
	case "libx264":
		args = []string{"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-profile:v", "high", "-x264-params", "repeat-headers=1:sliced-threads=1"}
	case "h264_nvenc":
		// Kept to options every NVENC SDK since 2021 accepts; "-delay 0" and
		// "-rc cbr" caused EINVAL (-22) on some driver/ffmpeg pairs.
		args = []string{"-c:v", "h264_nvenc", "-preset", "p2", "-tune", "ll", "-zerolatency", "1",
			"-profile:v", "high", "-forced-idr", "1"}
	case "h264_qsv":
		args = []string{"-c:v", "h264_qsv", "-preset", "veryfast", "-async_depth", "1", "-look_ahead", "0", "-profile:v", "high"}
	case "h264_amf":
		args = []string{"-c:v", "h264_amf", "-usage", "ultralowlatency", "-quality", "speed", "-profile:v", "high"}
	default:
		args = []string{"-c:v", encoder}
	}
	return append(args, rate...)
}

// ffmpegInputPlans returns capture input alternatives, best first. Each plan
// also says whether its frames live in GPU memory (ddagrab).
type ffmpegInputPlan struct {
	name  string
	input []string
	gpu   bool
}

func ffmpegInputPlans(cfg CaptureConfig, fps int, test bool) []ffmpegInputPlan {
	if custom := strings.TrimSpace(os.Getenv("DOUBLETAKE_FFMPEG_INPUT")); custom != "" {
		return []ffmpegInputPlan{{name: "custom", input: strings.Fields(custom)}}
	}
	if test {
		return []ffmpegInputPlan{{name: "testsrc", input: []string{"-re", "-f", "lavfi", "-i",
			fmt.Sprintf("testsrc2=size=%dx%d:rate=%d", testCaptureWidth, testCaptureHeight, fps)}}}
	}
	mouse := 0
	if cfg.ShowCursor {
		mouse = 1
	}
	switch runtime.GOOS {
	case "windows":
		return []ffmpegInputPlan{
			{name: "ddagrab", gpu: true, input: []string{"-f", "lavfi", "-i",
				fmt.Sprintf("ddagrab=output_idx=0:framerate=%d:draw_mouse=%d:dup_frames=1", fps, mouse)}},
			{name: "gdigrab", input: []string{"-f", "gdigrab", "-framerate", fmt.Sprint(fps),
				"-draw_mouse", fmt.Sprint(mouse), "-i", "desktop"}},
		}
	case "darwin":
		return []ffmpegInputPlan{{name: "avfoundation", input: []string{"-f", "avfoundation",
			"-framerate", fmt.Sprint(fps), "-capture_cursor", fmt.Sprint(mouse), "-i", "1:none"}}}
	default:
		display := os.Getenv("DISPLAY")
		if display == "" {
			display = ":0"
		}
		return []ffmpegInputPlan{{name: "x11grab", input: []string{"-f", "x11grab",
			"-framerate", fmt.Sprint(fps), "-draw_mouse", fmt.Sprint(mouse), "-i", display}}}
	}
}

func buildFFmpegVideoArgs(plan ffmpegInputPlan, encoder string, cfg CaptureConfig, fps, bitrate int) []string {
	args := []string{"-hide_banner", "-loglevel", "warning", "-nostdin",
		"-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32", "-analyzeduration", "0"}
	args = append(args, plan.input...)
	var filters []string
	if plan.gpu {
		filters = append(filters, "hwdownload", "format=bgra")
	}
	w, h := cfg.MaxWidth&^1, cfg.MaxHeight&^1
	if w > 0 && h > 0 {
		filters = append(filters,
			fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2", w, h),
			fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black", w, h),
			"setsar=1")
	} else {
		filters = append(filters, "scale=trunc(iw/2)*2:trunc(ih/2)*2")
	}
	filters = append(filters, "format=nv12")
	if encoder == "libx264" || encoder == "libopenh264" {
		filters[len(filters)-1] = "format=yuv420p"
	}
	args = append(args, "-vf", strings.Join(filters, ","), "-an", "-r", fmt.Sprint(fps))
	args = append(args, ffmpegEncoderArgs(encoder, bitrate, fps)...)
	args = append(args, "-bsf:v", "dump_extra=freq=keyframe", "-flush_packets", "1", "-f", "h264", "pipe:1")
	return args
}

func startPreparedFFmpegCapture(startupCtx, lifetimeCtx context.Context, cfg CaptureConfig, test bool) (*ScreenCapture, error) {
	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	encoders, err := workingFFmpegEncoders(cfg.HWAccel)
	if err != nil {
		return nil, err
	}
	var errs []string
	for _, encoder := range encoders {
		bitrate := captureBitrateKbps(cfg)
		if cfg.Bitrate <= 0 && encoder != "libx264" && encoder != "libopenh264" && bitrate < 8000 {
			// Hardware encoders are cheap; spend more bits for sharper text.
			bitrate = 8000
		}
		for _, plan := range ffmpegInputPlans(cfg, fps, test) {
			capture, err := startFFmpegCaptureAttempt(startupCtx, lifetimeCtx, plan, encoder, cfg, fps, bitrate)
			if err == nil {
				log.Printf("[CAPTURE] ffmpeg %s capture with %s at %d fps, %d kbps", plan.name, encoder, fps, bitrate)
				return capture, nil
			}
			log.Printf("[CAPTURE] ffmpeg %s capture with %s failed: %v", plan.name, encoder, err)
			errs = append(errs, fmt.Sprintf("%s/%s: %v", plan.name, encoder, err))
		}
	}
	return nil, fmt.Errorf("ffmpeg screen capture failed: %s", strings.Join(errs, "; "))
}

func startFFmpegCaptureAttempt(startupCtx, lifetimeCtx context.Context, plan ffmpegInputPlan, encoder string, cfg CaptureConfig, fps, bitrate int) (*ScreenCapture, error) {
	startupCtx, cancelStartup := context.WithTimeout(normalizeContext(startupCtx), captureStartupBudget)
	defer cancelStartup()
	captureCtx, cancel := context.WithCancel(normalizeContext(lifetimeCtx))

	args := buildFFmpegVideoArgs(plan, encoder, cfg, fps, bitrate)
	dbg("[CAPTURE] %s %s", FFmpegPath(), strings.Join(args, " "))
	cmd := exec.CommandContext(captureCtx, FFmpegPath(), args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = stdout.Close()
		return nil, fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}
	waitResult, err := startGStreamerCommand(cmd)
	if err != nil {
		cancel()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	tail := newCaptureStderrTail()
	go logStderr("FFMPEG", stderr, tail)
	capture := &ScreenCapture{cmd: cmd, stdout: stdout, cancel: cancel, waitCh: make(chan struct{}), stderr: tail}
	go func() {
		capture.waitErr = <-waitResult
		close(capture.waitCh)
	}()
	probeCtx, cancelProbe := context.WithTimeout(startupCtx, captureStartupProbeTimeout(fps))
	err = probeVideoCaptureStartup(probeCtx, capture, VideoCodecH264, false)
	cancelProbe()
	if err != nil {
		capture.Stop()
		if s := capture.stderrText(); s != "" {
			err = fmt.Errorf("%w; ffmpeg stderr: %s", err, s)
		}
		return nil, err
	}
	return capture, nil
}
