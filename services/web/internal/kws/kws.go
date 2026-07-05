// Package kws is a keyword spotter for controlling a playing Quran recitation
// (stop / again / next / previous) without involving the LLM. It runs in the
// shared bridge layer so BOTH transports (pion + LiveKit) get it, and only
// while a recitation is playing.
//
// The real engine (sherpa-onnx, CGO + native libs) is compiled only under the
// `sherpa` build tag (see spotter_sherpa.go). Without that tag a no-op spotter
// is used (spotter_stub.go), so the default build/tests need no native library.
package kws

import "log/slog"

// Config configures the spotter. ModelDir should contain the sherpa-onnx KWS
// model (encoder*.onnx, decoder*.onnx, joiner*.onnx, tokens.txt) and a
// keywords.txt (unless KeywordsFile overrides it).
type Config struct {
	ModelDir     string
	KeywordsFile string
	Threshold    float32 // detection threshold (sherpa KeywordsThreshold), 0 = engine default
	Score        float32 // boosting score (sherpa KeywordsScore), 0 = engine default
	NumThreads   int
	Log          *slog.Logger
}

// Spotter is shared across calls; it mints one Stream per call.
type Spotter interface {
	NewStream() Stream
	Close()
}

// Stream is a per-call decode stream. Feed is called from a single goroutine
// (the call's mic loop).
type Stream interface {
	// Feed processes one chunk of 16 kHz mono PCM16 LE. When a keyword is
	// detected it returns the keyword's reported name (the @NAME from
	// keywords.txt, e.g. "STOP") and internally resets; otherwise "".
	Feed(pcm16 []byte) string
	Close()
}

// --- shared no-op implementation (used by the stub, and on load failure) ---

type noopSpotter struct{}

func (noopSpotter) NewStream() Stream { return noopStream{} }
func (noopSpotter) Close()            {}

type noopStream struct{}

func (noopStream) Feed([]byte) string { return "" }
func (noopStream) Close()             {}
