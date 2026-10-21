package airplay

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestMixConverterFloat48kTo44k(t *testing.T) {
	c := newMixConverterFor(waveFormatIEEEFloat, 32, 2, 48000)
	var out []byte
	for b := 0; b < 100; b++ { // 100 x 480 frames = 1 s
		raw := make([]byte, 480*2*4)
		for i := 0; i < 480; i++ {
			v := float32(0.5 * math.Sin(2*math.Pi*440*float64(b*480+i)/48000))
			binary.LittleEndian.PutUint32(raw[i*8:], math.Float32bits(v))
			binary.LittleEndian.PutUint32(raw[i*8+4:], math.Float32bits(v))
		}
		out = append(out, c.convert(raw, 480, false)...)
	}
	frames := len(out) / 4
	if frames < 44090 || frames > 44110 {
		t.Fatalf("got %d frames for 1 s, want ~44100", frames)
	}
	peak := 0
	for i := 0; i < len(out); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(out[i:])))
		if v > peak {
			peak = v
		}
	}
	if peak < 16000 || peak > 16500 {
		t.Fatalf("peak %d, want ~16383", peak)
	}
}
