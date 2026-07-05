// Package echoengine is the M1 placeholder Engine: it echoes mic audio straight
// back as agent audio, proving the gRPC bidi contract end-to-end before ADK +
// Gemini Live is wired in (M2). It does no resampling — at this stage we only
// verify that frames flow both ways.
package echoengine

import (
	"context"

	voicev1 "github.com/ahmkindi/quran-agent/gen/voice/v1"
	"github.com/ahmkindi/quran-agent/services/agent/internal/grpcsrv"
)

// Engine implements grpcsrv.Engine.
type Engine struct{}

// New returns an echo Engine.
func New() *Engine { return &Engine{} }

// StartCall returns a Call that echoes audio back through the sink.
func (e *Engine) StartCall(_ context.Context, _ *voicev1.CallStart, sink grpcsrv.Sink) (grpcsrv.Call, error) {
	return &call{sink: sink}, nil
}

type call struct {
	sink grpcsrv.Sink
}

func (c *call) PushAudio(pcm16 []byte) error {
	// Copy: the caller may reuse the buffer after this returns.
	out := make([]byte, len(pcm16))
	copy(out, pcm16)
	return c.sink(&voicev1.ServerFrame{
		Payload: &voicev1.ServerFrame_AudioOut{AudioOut: out},
	})
}

func (c *call) ControlPlayback(string) error { return nil }

func (c *call) Close() error { return nil }
