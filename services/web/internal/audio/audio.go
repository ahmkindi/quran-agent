// Package audio bridges WebRTC Opus and Gemini Live PCM. libopus (via
// layeh.com/gopus) does the sample-rate conversion internally, so there is no
// separate resampler:
//
//   - inbound:  browser Opus  --decode@16kHz-->  PCM16 16kHz  --> Gemini
//   - outbound: Gemini PCM 24kHz --encode@24kHz--> Opus --> browser
//
// The Opus wire clock stays 48 kHz regardless; pion sets RTP timestamps from
// the 20 ms sample Duration.
package audio

import (
	"encoding/binary"

	"layeh.com/gopus"
)

const (
	GeminiInRate  = 16000 // Gemini Live input PCM rate
	GeminiOutRate = 24000 // Gemini Live output PCM rate
	Channels      = 1
	FrameMS       = 20

	InFrameSamples  = GeminiInRate * FrameMS / 1000  // 320
	OutFrameSamples = GeminiOutRate * FrameMS / 1000 // 480
	OutFrameBytes   = OutFrameSamples * 2            // 960

	maxDecodeSamples = 60 * GeminiInRate / 1000 // 60ms cap for a single Opus packet
	maxOpusBytes     = 4000
)

// Decoder converts inbound browser Opus packets to 16 kHz PCM16 (mono) bytes.
type Decoder struct{ d *gopus.Decoder }

// NewDecoder makes a 16 kHz mono Opus decoder.
func NewDecoder() (*Decoder, error) {
	d, err := gopus.NewDecoder(GeminiInRate, Channels)
	if err != nil {
		return nil, err
	}
	return &Decoder{d: d}, nil
}

// Decode turns one Opus packet into PCM16 little-endian bytes at 16 kHz.
func (d *Decoder) Decode(opusPkt []byte) ([]byte, error) {
	pcm, err := d.d.Decode(opusPkt, maxDecodeSamples, false)
	if err != nil {
		return nil, err
	}
	return int16ToBytes(pcm), nil
}

// Encoder converts outbound Gemini 24 kHz PCM16 frames to Opus packets.
type Encoder struct{ e *gopus.Encoder }

// NewEncoder makes a 24 kHz mono VoIP Opus encoder.
func NewEncoder() (*Encoder, error) {
	e, err := gopus.NewEncoder(GeminiOutRate, Channels, gopus.Voip)
	if err != nil {
		return nil, err
	}
	e.SetBitrate(32000) // clearer voice; 24kHz mono has headroom
	return &Encoder{e: e}, nil
}

// EncodeFrame encodes exactly one 20 ms frame (OutFrameBytes of PCM16 LE at
// 24 kHz) into an Opus packet.
func (e *Encoder) EncodeFrame(pcm16le []byte) ([]byte, error) {
	return e.e.Encode(bytesToInt16(pcm16le), OutFrameSamples, maxOpusBytes)
}

// Framer accumulates a 24 kHz PCM byte stream (Gemini emits arbitrary chunk
// sizes) and slices it into exact 20 ms frames for the Opus encoder.
type Framer struct{ buf []byte }

// Push appends PCM bytes and returns any complete 20 ms frames now available.
func (f *Framer) Push(pcm []byte) [][]byte {
	f.buf = append(f.buf, pcm...)
	var out [][]byte
	for len(f.buf) >= OutFrameBytes {
		frame := make([]byte, OutFrameBytes)
		copy(frame, f.buf[:OutFrameBytes])
		out = append(out, frame)
		f.buf = f.buf[OutFrameBytes:]
	}
	return out
}

// Drain returns the buffered remainder padded with silence to a full 20 ms
// frame, or nil if the buffer is empty. Call at end-of-turn.
func (f *Framer) Drain() []byte {
	if len(f.buf) == 0 {
		return nil
	}
	frame := make([]byte, OutFrameBytes)
	copy(frame, f.buf)
	f.buf = f.buf[:0]
	return frame
}

// Reset discards buffered audio (e.g. on barge-in).
func (f *Framer) Reset() { f.buf = f.buf[:0] }

func int16ToBytes(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func bytesToInt16(b []byte) []int16 {
	s := make([]int16, len(b)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return s
}
