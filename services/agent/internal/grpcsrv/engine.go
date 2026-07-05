package grpcsrv

import (
	"context"

	voicev1 "github.com/ahmkindi/quran-agent/gen/voice/v1"
)

// Sink delivers a ServerFrame back to the web-service. Implementations are
// safe for concurrent use (gRPC stream sends are serialized internally).
type Sink func(*voicev1.ServerFrame) error

// Engine turns an incoming call into a Call. In M1 this is an echo; in M2 it
// is backed by ADK + Gemini Live. The gRPC server owns the stream lifecycle
// and is agnostic to which Engine is wired in.
type Engine interface {
	// StartCall begins a call. The engine emits audio/control by calling sink
	// (possibly from its own goroutines) until Close is called or ctx is done.
	StartCall(ctx context.Context, start *voicev1.CallStart, sink Sink) (Call, error)
}

// Call is one in-flight call.
type Call interface {
	// PushAudio feeds one frame of mic audio: PCM16 LE, 16 kHz, mono.
	PushAudio(pcm16le16k []byte) error
	// ControlPlayback acts on an in-progress recitation immediately. Driven by
	// the web-service keyword spotter via a RecitationControl frame. Actions:
	// "stop" (halt), "again" (replay the playing verse), "next"/"previous"
	// (jump to the neighbouring verse).
	ControlPlayback(action string) error
	// Close releases the call's resources. Safe to call once.
	Close() error
}
