package quran

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/hajimehoshi/go-mp3"
)

// outRate is the sample rate the outbound audio path expects (Gemini's native
// output rate). Injected recitation is resampled to match so web-service can
// Opus-encode it unchanged.
const outRate = 24000

// decodeMP3ToPCM24kMono decodes an MP3 (any sample rate) to 24 kHz mono PCM16
// little-endian. go-mp3 is pure Go, so the agent stays CGO-free. It always
// yields 16-bit stereo PCM at the source rate; we downmix to mono then resample.
func decodeMP3ToPCM24kMono(data []byte) ([]byte, error) {
	dec, err := mp3.NewDecoder(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("mp3 decode init: %w", err)
	}
	srcRate := dec.SampleRate()
	if srcRate <= 0 {
		return nil, fmt.Errorf("mp3 has invalid sample rate %d", srcRate)
	}

	raw, err := io.ReadAll(dec)
	if err != nil {
		return nil, fmt.Errorf("mp3 read: %w", err)
	}
	// raw is interleaved stereo int16 LE: 4 bytes per sample frame.
	frames := len(raw) / 4
	mono := make([]int16, frames)
	for i := 0; i < frames; i++ {
		l := int16(binary.LittleEndian.Uint16(raw[i*4:]))
		r := int16(binary.LittleEndian.Uint16(raw[i*4+2:]))
		mono[i] = int16((int32(l) + int32(r)) / 2)
	}

	resampled := resampleLinear(mono, srcRate, outRate)
	out := make([]byte, len(resampled)*2)
	for i, s := range resampled {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out, nil
}

// resampleLinear resamples mono int16 PCM from srcRate to dstRate with linear
// interpolation — adequate for speech/recitation and dependency-free.
func resampleLinear(in []int16, srcRate, dstRate int) []int16 {
	if srcRate == dstRate || len(in) == 0 {
		return in
	}
	outLen := int(int64(len(in)) * int64(dstRate) / int64(srcRate))
	if outLen <= 0 {
		return nil
	}
	out := make([]int16, outLen)
	ratio := float64(srcRate) / float64(dstRate)
	for i := 0; i < outLen; i++ {
		pos := float64(i) * ratio
		idx := int(pos)
		frac := pos - float64(idx)
		if idx+1 < len(in) {
			out[i] = int16(float64(in[idx])*(1-frac) + float64(in[idx+1])*frac)
		} else {
			out[i] = in[len(in)-1]
		}
	}
	return out
}
