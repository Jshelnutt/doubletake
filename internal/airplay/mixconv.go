package airplay

import (
	"encoding/binary"
	"math"
)

const (
	waveFormatPCM       = 0x0001
	waveFormatIEEEFloat = 0x0003
)

// mixConverter turns a device mix format (usually 32-bit float, 48 kHz,
// 2+ channels) into S16LE 44.1 kHz stereo with linear resampling.
type mixConverter struct {
	isFloat  bool
	bits     int
	channels int
	inRate   float64
	pos      float64 // fractional input position carried across buffers
	prev     [2]float64
}

func newMixConverterFor(tag uint16, bits, channels int, rate float64) *mixConverter {
	c := &mixConverter{bits: bits, channels: channels, inRate: rate}
	switch {
	case tag == waveFormatIEEEFloat && bits == 32:
		c.isFloat = true
	case tag == waveFormatPCM && (bits == 16 || bits == 24 || bits == 32):
	default:
		return nil
	}
	if channels < 1 || rate <= 0 {
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
