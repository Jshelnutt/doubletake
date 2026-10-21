//go:build windows

package airplay

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

const (
	audclntStreamflagsAutoconvertPCM    = 0x80000000
	audclntStreamflagsSrcDefaultQuality = 0x08000000
	waveFormatExtensible                = 0xFFFE
	wasapiBufferDuration                = wca.REFERENCE_TIME(200 * 10000) // 200 ms in 100-ns units
)

// startWASAPILoopback captures whatever the default Windows playback device is
// playing and writes it as S16LE 44.1 kHz stereo. While nothing is playing
// WASAPI delivers no packets, so the producer synthesizes silence on the wall
// clock to keep the AirPlay audio timeline continuous.
func startWASAPILoopback(ctx context.Context) (io.ReadCloser, <-chan error, error) {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := runWASAPILoopback(ctx, pw, ready)
		_ = pw.CloseWithError(err)
		done <- err
		close(done)
	}()
	if err := <-ready; err != nil {
		_ = pr.Close()
		return nil, nil, err
	}
	return pr, done, nil
}

func activateAudioClient(dev *wca.IMMDevice) (*wca.IAudioClient, error) {
	var ac *wca.IAudioClient
	// IMMDevice::Activate(REFIID, DWORD dwClsCtx, PROPVARIANT*, void**); the
	// class context is passed by value.
	hr, _, _ := syscall.SyscallN(dev.VTable().Activate,
		uintptr(unsafe.Pointer(dev)),
		uintptr(unsafe.Pointer(wca.IID_IAudioClient)),
		uintptr(wca.CLSCTX_ALL),
		0,
		uintptr(unsafe.Pointer(&ac)))
	if hr != 0 {
		return nil, ole.NewError(hr)
	}
	return ac, nil
}

func runWASAPILoopback(ctx context.Context, out *io.PipeWriter, ready chan<- error) (err error) {
	signalled := false
	fail := func(e error) error {
		if !signalled {
			signalled = true
			ready <- e
		}
		return e
	}
	if e := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); e != nil {
		if oe, ok := e.(*ole.OleError); !ok || (oe.Code() != 1 /*S_FALSE*/ && oe.Code() != 0) {
			return fail(fmt.Errorf("CoInitializeEx: %w", e))
		}
	}
	defer ole.CoUninitialize()

	var enum *wca.IMMDeviceEnumerator
	if e := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &enum); e != nil {
		return fail(fmt.Errorf("create device enumerator: %w", e))
	}
	defer enum.Release()
	var dev *wca.IMMDevice
	if e := enum.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &dev); e != nil {
		return fail(fmt.Errorf("no default playback device: %w", e))
	}
	defer dev.Release()
	ac, e := activateAudioClient(dev)
	if e != nil {
		return fail(fmt.Errorf("activate audio client: %w", e))
	}
	defer func() { ac.Release() }()

	// Preferred: let Windows convert the mix to exactly what AirPlay needs.
	want := wca.WAVEFORMATEX{
		WFormatTag: wca.WAVE_FORMAT_PCM, NChannels: audioChannels, NSamplesPerSec: audioSampleRate,
		WBitsPerSample: 16, NBlockAlign: audioChannels * 2,
		NAvgBytesPerSec: audioSampleRate * audioChannels * 2,
	}
	conv := false
	var mix *wca.WAVEFORMATEX
	// Default: open the device in its native mix format (normally 48 kHz
	// float32) and convert in software. Windows' AUTOCONVERTPCM on a loopback
	// stream proved unreliable (stream opened but stayed silent on a Win11
	// laptop), so it is opt-in via DOUBLETAKE_WASAPI_AUTOCONVERT=1.
	initErr := fmt.Errorf("auto-convert not requested")
	if os.Getenv("DOUBLETAKE_WASAPI_AUTOCONVERT") == "1" {
		initErr = ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED,
			wca.AUDCLNT_STREAMFLAGS_LOOPBACK|audclntStreamflagsAutoconvertPCM|audclntStreamflagsSrcDefaultQuality,
			wasapiBufferDuration, 0, &want, nil)
	}
	if initErr != nil {
		ac.Release()
		if ac, e = activateAudioClient(dev); e != nil {
			return fail(fmt.Errorf("re-activate audio client: %w", e))
		}
		if e := ac.GetMixFormat(&mix); e != nil {
			return fail(fmt.Errorf("get mix format: %w", e))
		}
		defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(mix)))
		if e := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, wca.AUDCLNT_STREAMFLAGS_LOOPBACK,
			wasapiBufferDuration, 0, mix, nil); e != nil {
			return fail(fmt.Errorf("initialize loopback: %w", e))
		}
		conv = true
	}
	var acc *wca.IAudioCaptureClient
	if e := ac.GetService(wca.IID_IAudioCaptureClient, &acc); e != nil {
		return fail(fmt.Errorf("get capture client: %w", e))
	}
	defer acc.Release()
	if e := ac.Start(); e != nil {
		return fail(fmt.Errorf("start loopback: %w", e))
	}
	defer ac.Stop()
	signalled = true
	ready <- nil
	if conv {
		log.Printf("[AUDIO] capturing Windows system audio (WASAPI loopback, mix format tag=0x%x %d Hz %d ch %d-bit, software convert to 44.1 kHz s16)",
			mix.WFormatTag, mix.NSamplesPerSec, mix.NChannels, mix.WBitsPerSample)
	} else {
		log.Printf("[AUDIO] capturing Windows system audio (WASAPI loopback, Windows auto-convert to 44.1 kHz s16)")
	}

	var cv *mixConverter
	if conv {
		cv = newMixConverter(mix)
		if cv == nil {
			return fmt.Errorf("unsupported mix format tag=0x%x bits=%d", mix.WFormatTag, mix.WBitsPerSample)
		}
	}

	start := time.Now()
	var written int64 // output frames
	var realFrames, fillFrames, packets, silentFlagged int64
	var peak int
	lastReport := time.Now()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	silence := make([]byte, 4096*4)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		gotData := false
		for {
			var next uint32
			if e := acc.GetNextPacketSize(&next); e != nil {
				return fmt.Errorf("loopback packet size: %w", e)
			}
			if next == 0 {
				break
			}
			var data *byte
			var frames, flags uint32
			var devPos, qpc uint64
			if e := acc.GetBuffer(&data, &frames, &flags, &devPos, &qpc); e != nil {
				return fmt.Errorf("loopback get buffer: %w", e)
			}
			var pcm []byte
			if conv {
				raw := unsafe.Slice(data, int(frames)*int(mix.NBlockAlign))
				pcm = cv.convert(raw, int(frames), flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0)
			} else {
				n := int(frames) * 4
				pcm = make([]byte, n)
				if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT == 0 {
					copy(pcm, unsafe.Slice(data, n))
				}
			}
			_ = acc.ReleaseBuffer(frames)
			packets++
			if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 {
				silentFlagged++
			}
			for i := 0; i+1 < len(pcm); i += 2 {
				v := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
				if v < 0 {
					v = -v
				}
				if v > peak {
					peak = v
				}
			}
			realFrames += int64(len(pcm) / 4)
			if len(pcm) > 0 {
				if _, e := out.Write(pcm); e != nil {
					return nil // reader closed: normal stop
				}
				written += int64(len(pcm) / 4)
				gotData = true
			}
		}
		if time.Since(lastReport) >= 5*time.Second {
			log.Printf("[AUDIO] WASAPI last %.0fs: %d packets (%d flagged silent), %d captured frames, %d silence-fill frames, peak level %.1f%%",
				time.Since(lastReport).Seconds(), packets, silentFlagged, realFrames, fillFrames, float64(peak)*100/32767)
			packets, silentFlagged, realFrames, fillFrames, peak = 0, 0, 0, 0, 0
			lastReport = time.Now()
		}
		// Keep the timeline moving during silence (no packets at all).
		if !gotData {
			due := int64(time.Since(start).Seconds()*audioSampleRate) - written
			if due > audioSampleRate/50 { // more than 20 ms behind
				for due > 0 {
					n := due
					if n > 4096 {
						n = 4096
					}
					if _, e := out.Write(silence[:n*4]); e != nil {
						return nil
					}
					written += n
					fillFrames += n
					due -= n
				}
			}
		} else if behind := int64(time.Since(start).Seconds()*audioSampleRate) - written; behind > audioSampleRate {
			// Re-anchor after long gaps so silence fill does not burst later.
			start = time.Now().Add(-time.Duration(float64(written) / audioSampleRate * float64(time.Second)))
		}
	}
}

func newMixConverter(f *wca.WAVEFORMATEX) *mixConverter {
	tag := f.WFormatTag
	if tag == waveFormatExtensible {
		// WAVEFORMATEXTENSIBLE: SubFormat GUID's first two bytes hold the tag.
		ext := unsafe.Slice((*byte)(unsafe.Pointer(f)), 18+22)
		tag = binary.LittleEndian.Uint16(ext[24:26])
	}
	return newMixConverterFor(tag, int(f.WBitsPerSample), int(f.NChannels), float64(f.NSamplesPerSec))
}
