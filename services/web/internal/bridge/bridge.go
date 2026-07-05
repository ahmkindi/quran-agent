// Package bridge is the web-service's gRPC client to agent-service. It opens
// one VoiceBridge stream per call and exposes a small audio-in / frame-out API.
//
// It is also the shared choke point where BOTH transports (pion + LiveKit) send
// mic audio and receive agent frames, so the control-word keyword spotter lives
// here: while a recitation plays, each mic frame is fed to the spotter, and on
// a hit a RecitationControl (stop / again / next / previous) is sent upstream
// (the agent then re-targets or cancels the recitation). This gives both
// transports the feature with one implementation.
package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	voicev1 "github.com/ahmkindi/quran-agent/gen/voice/v1"
	"github.com/ahmkindi/quran-agent/pkg/telemetry"
	"github.com/ahmkindi/quran-agent/services/web/internal/kws"
)

// Client holds a pooled connection to agent-service plus the shared halt-word
// spotter (one per process; mints a per-call stream).
type Client struct {
	conn    *grpc.ClientConn
	cli     voicev1.VoiceBridgeClient
	spotter kws.Spotter
	log     *slog.Logger
}

// Dial connects to agent-service (plaintext; intra-cluster on the docker net).
// spotter may be nil (halt-word detection then disabled).
func Dial(target string, spotter kws.Spotter, log *slog.Logger) (*Client, error) {
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, cli: voicev1.NewVoiceBridgeClient(conn), spotter: spotter, log: log}, nil
}

// Close shuts the connection and the shared spotter.
func (c *Client) Close() error {
	if c.spotter != nil {
		c.spotter.Close()
	}
	return c.conn.Close()
}

// FrameHandler receives ServerFrames as the agent produces them.
type FrameHandler func(*voicev1.ServerFrame)

// Call is one open VoiceBridge stream.
type Call struct {
	stream voicev1.VoiceBridge_StreamClient
	ctx    context.Context // call context; carries the transport's call span
	sendMu sync.Mutex
	done   chan struct{}

	kwsStream  kws.Stream
	recitation atomic.Bool  // true while reciter audio plays (from the "recitation" ToolEvent)
	fedFrames  atomic.Int64 // mic frames fed to the spotter while the gate is open
	onFrame    FrameHandler // also used to surface local keyword hits to the UI
	log        *slog.Logger
}

// actionFor maps a spotted keyword (the @NAME from keywords.txt, e.g. "STOP")
// to a RecitationControl action. Unknown keywords map to "".
func actionFor(keyword string) string {
	switch strings.ToLower(strings.TrimSpace(keyword)) {
	case "stop":
		return "stop"
	case "again":
		return "again"
	case "next":
		return "next"
	case "previous":
		return "previous"
	}
	return ""
}

// OpenCall starts a stream, sends CallStart, and spawns a goroutine that
// delivers every inbound ServerFrame to onFrame until the stream closes. It also
// watches for the "recitation" control event to gate the halt-word spotter.
func (c *Client) OpenCall(ctx context.Context, start *voicev1.CallStart, onFrame FrameHandler) (*Call, error) {
	stream, err := c.cli.Stream(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&voicev1.ClientFrame{
		Payload: &voicev1.ClientFrame_CallStart{CallStart: start},
	}); err != nil {
		return nil, err
	}

	call := &Call{stream: stream, ctx: ctx, done: make(chan struct{}), onFrame: onFrame, log: c.log}
	if c.spotter != nil {
		call.kwsStream = c.spotter.NewStream()
	}

	go func() {
		defer close(call.done)
		for {
			f, err := stream.Recv()
			if errors.Is(err, io.EOF) || err != nil {
				return
			}
			// Track recitation on/off to gate the spotter. Only "start"/"stop"
			// toggle the gate ("verse" events carry UI metadata mid-recitation).
			// Still forward the frame so the transport (e.g. pion's mic-gate)
			// sees it too.
			if te := f.GetToolEvent(); te != nil && te.GetName() == "recitation" {
				switch te.GetStatus() {
				case "start":
					call.recitation.Store(true)
					if c.log != nil {
						c.log.Info("kws gate open", "spotter_active", call.kwsStream != nil)
					}
				case "stop":
					call.recitation.Store(false)
					if c.log != nil {
						c.log.Info("kws gate closed")
					}
				}
			}
			if onFrame != nil {
				onFrame(f)
			}
		}
	}()
	return call, nil
}

// SendAudio sends one frame of mic audio (PCM16 LE, 16 kHz, mono). While a
// recitation is playing it also feeds the frame to the control-word spotter
// and, on a hit, sends the matching RecitationControl to the agent.
func (c *Call) SendAudio(pcm16 []byte) error {
	if c.kwsStream != nil && c.recitation.Load() {
		if c.fedFrames.Add(1) == 1 && c.log != nil {
			c.log.Info("kws feeding mic frames")
		}
		if kw := c.kwsStream.Feed(pcm16); kw != "" {
			if action := actionFor(kw); action != "" {
				if c.log != nil {
					c.log.Info("control word detected", "keyword", kw, "action", action)
				}
				// Trace the detection on the call's timeline; the agent-side
				// agent.recitation_control span shows the reaction latency.
				_, sp := telemetry.Tracer("web").Start(c.ctx, "kws.control",
					trace.WithAttributes(
						attribute.String("keyword", kw),
						attribute.String("action", action)))
				_ = c.SendRecitationControl(action)
				sp.End()
				// Instant "heard you" feedback for the UI, without waiting for
				// the agent's next verse/stop event.
				if c.onFrame != nil {
					c.onFrame(&voicev1.ServerFrame{Payload: &voicev1.ServerFrame_ToolEvent{
						ToolEvent: &voicev1.ToolEvent{Name: "keyword", Status: action},
					}})
				}
			}
		}
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(&voicev1.ClientFrame{
		Payload: &voicev1.ClientFrame_AudioIn{AudioIn: pcm16},
	})
}

// SendRecitationControl tells agent-service to act on the current recitation
// (action: "stop" | "again" | "next" | "previous").
func (c *Call) SendRecitationControl(action string) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(&voicev1.ClientFrame{
		Payload: &voicev1.ClientFrame_RecitationControl{
			RecitationControl: &voicev1.RecitationControl{Action: action},
		},
	})
}

// Close sends CallEnd, half-closes the stream, and waits for the recv goroutine.
func (c *Call) Close(reason string) error {
	c.sendMu.Lock()
	_ = c.stream.Send(&voicev1.ClientFrame{
		Payload: &voicev1.ClientFrame_CallEnd{CallEnd: &voicev1.CallEnd{Reason: reason}},
	})
	err := c.stream.CloseSend()
	c.sendMu.Unlock()
	<-c.done
	if c.kwsStream != nil {
		c.kwsStream.Close()
	}
	return err
}

// Done is closed when the inbound stream ends.
func (c *Call) Done() <-chan struct{} { return c.done }
