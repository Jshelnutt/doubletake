//go:build windows

package airplay

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
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
	waveFormatIEEEFloat                 = 0x0003
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
	initErr := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED,
		wca.AUDCLNT_STREAMFLAGS_LOOPBACK|audclntStreamflagsAutoconvertPCM|audclntStreamflagsSrcDefaultQuality,
		wasapiBufferDuration, 0, &want, nil)
	if initErr != nil {
		log.Printf("[AUDIO] WASAPI auto-convert unavailable (%v); converting the mix format in software", initErr)
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
	log.Printf("[AUDIO] capturing Windows system audio (WASAPI loopback, convert=%v)", conv)

	var cv *mixConverter
	if conv {
		cv = newMixConverter(mix)
		if cv == nil {
			return fmt.Errorf("unsupported mix format tag=0x%x bits=%d", mix.WFormatTag, mix.WBitsPerSample)
		}
	}

	start := time.Now()
	var written int64 // output frames
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
			if len(pcm) > 0 {
				if _, e := out.Write(pcm); e != nil {
					return nil // reader closed: normal stop
				}
				written += int64(len(pcm) / 4)
				gotData = true
			}
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
					due -= n
				}
			}
		} else if behind := int64(time.Since(start).Seconds()*audioSampleRate) - written; behind > audioSampleRate {
			// Re-anchor after long gaps so silence fill does not burst later.
			start = time.Now().Add(-time.Duration(float64(written) / audioSampleRate * float64(time.Second)))
		}
	}
}

// mixConverter turns the device mix format (usually 32-bit float, 48 kHz,
// 2+ channels) into S16LE 44.1 kHz stereo with linear resampling.
type mixConverter struct {
	isFloat  bool
	bits     int
	channels int
	inRate   float64
	pos      float64 // fractional input position carried across buffers
	prev     [2]float64
	havePrev bool
}

func newMixConverter(f *wca.WAVEFORMATEX) *mixConverter {
	tag := f.WFormatTag
	if tag == waveFormatExtensible {
		// WAVEFORMATEXTENSIBLE: SubFormat GUID's first two bytes hold the tag.
		ext := unsafe.Slice((*byte)(unsafe.Pointer(f)), 18+22)
		tag = binary.LittleEndian.Uint16(ext[24:26])
	}
	c := &mixConverter{bits: int(f.WBitsPerSample), channels: int(f.NChannels), inRate: float64(f.NSamplesPerSec)}
	switch {
	case tag == waveFormatIEEEFloat && c.bits == 32:
		c.isFloat = true
	case tag == wca.WAVE_FORMAT_PCM && (c.bits == 16 || c.bits == 24 || c.bits == 32):
	default:
		return nil
	}
	if c.channels < 1 {
		return nil
	}
	return c
}

func (c *mixConverter) sample(raw []byte, frame, ch int) float64 {
	if ch >= c.channels {
		ch = 0
	}
	bps := c.bits / 8
	off := (frame*c.channels + ch) * bps
	switch {
	case c.isFloat:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[off:])))
	case c.bits == 16:
		return float64(int16(binary.LittleEndian.Uint16(raw[off:]))) / 32768
	case c.bits == 24:
		v := int32(raw[off]) | int32(raw[off+1])<<8 | int32(int8(raw[off+2]))<<16
		return float64(v) / 8388608
	default:
		return float64(int32(binary.LittleEndian.Uint32(raw[off:]))) / 2147483648
	}
}

func (c *mixConverter) convert(raw []byte, frames int, silent bool) []byte {
	step := c.inRate / audioSampleRate
	out := make([]byte, 0, int(float64(frames)/step+2)*4)
	get := func(i, ch int) float64 {
		if silent {
			return 0
		}
		if i < 0 {
			return c.prev[ch]
		}
		return c.sample(raw, i, ch)
	}
	for c.pos < float64(frames-1) {
		i := int(math.Floor(c.pos))
		t := c.pos - float64(i)
		for ch := 0; ch < 2; ch++ {
			a, b := get(i, ch), get(i+1, ch)
			v := a + (b-a)*t
			if v > 1 {
				v = 1
			} else if v < -1 {
				v = -1
			}
			out = binary.LittleEndian.AppendUint16(out, uint16(int16(math.Round(v*32767))))
		}
		c.pos += step
	}
	if frames > 0 {
		for ch := 0; ch < 2; ch++ {
			c.prev[ch] = get(frames-1, ch)
		}
	}
	c.pos -= float64(frames)
	return out
}
