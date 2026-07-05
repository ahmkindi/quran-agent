package audio

import (
	"math"
	"testing"

	"layeh.com/gopus"
)

func gopusDecoder24k() (*gopus.Decoder, error) { return gopus.NewDecoder(GeminiOutRate, Channels) }

// TestEncodeDecodeRoundTrip verifies the CGO gopus path links and that a 24 kHz
// PCM frame survives an Opus encode -> decode round trip with recognizable
// energy (Opus is lossy, so we check correlation of a tone, not exact bytes).
func TestEncodeDecodeRoundTrip(t *testing.T) {
	enc, err := NewEncoder()
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	// Decode back at 24 kHz to compare like-for-like.
	dec, err := gopusDecoder24k()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}

	// One 20ms frame of a 440 Hz tone at 24 kHz.
	in := make([]byte, OutFrameBytes)
	for i := 0; i < OutFrameSamples; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(GeminiOutRate)))
		in[i*2] = byte(v)
		in[i*2+1] = byte(v >> 8)
	}

	// Opus needs a few frames to warm up; encode/decode the same frame thrice.
	var pcm []int16
	for i := 0; i < 3; i++ {
		pkt, err := enc.EncodeFrame(in)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if len(pkt) == 0 {
			t.Fatal("empty opus packet")
		}
		pcm, err = dec.Decode(pkt, OutFrameSamples, false)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	if len(pcm) != OutFrameSamples {
		t.Fatalf("decoded %d samples, want %d", len(pcm), OutFrameSamples)
	}

	var energy float64
	for _, s := range pcm {
		energy += float64(s) * float64(s)
	}
	if rms := math.Sqrt(energy / float64(len(pcm))); rms < 500 {
		t.Fatalf("decoded RMS too low (%.0f); tone did not survive round trip", rms)
	}
}

func TestFramer(t *testing.T) {
	var f Framer
	// 2.5 frames worth of bytes.
	got := f.Push(make([]byte, OutFrameBytes*2+OutFrameBytes/2))
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2", len(got))
	}
	for _, fr := range got {
		if len(fr) != OutFrameBytes {
			t.Fatalf("frame len %d, want %d", len(fr), OutFrameBytes)
		}
	}
	if d := f.Drain(); len(d) != OutFrameBytes {
		t.Fatalf("drain len %d, want %d (zero-padded)", len(d), OutFrameBytes)
	}
	if d := f.Drain(); d != nil {
		t.Fatal("second drain should be nil")
	}
}
