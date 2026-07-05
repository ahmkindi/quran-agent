package grpcsrv_test

import (
	"context"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	voicev1 "github.com/ahmkindi/quran-agent/gen/voice/v1"
	"github.com/ahmkindi/quran-agent/services/agent/internal/echoengine"
	"github.com/ahmkindi/quran-agent/services/agent/internal/grpcsrv"
)

// TestEchoRoundTrip proves the gRPC bidi contract: frames sent by a client are
// echoed back by the echo Engine through the server.
func TestEchoRoundTrip(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	voicev1.RegisterVoiceBridgeServer(gs, grpcsrv.NewServer(echoengine.New(), slog.Default()))
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := voicev1.NewVoiceBridgeClient(conn).Stream(ctx)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if err := stream.Send(&voicev1.ClientFrame{
		Payload: &voicev1.ClientFrame_CallStart{CallStart: &voicev1.CallStart{CallId: "t1"}},
	}); err != nil {
		t.Fatalf("send start: %v", err)
	}

	const frames, frameBytes = 5, 640
	var got int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := stream.Recv()
			if err != nil {
				return
			}
			if a := f.GetAudioOut(); a != nil {
				if atomic.AddInt64(&got, int64(len(a))) == frames*frameBytes {
					return
				}
			}
		}
	}()

	for i := 0; i < frames; i++ {
		if err := stream.Send(&voicev1.ClientFrame{
			Payload: &voicev1.ClientFrame_AudioIn{AudioIn: make([]byte, frameBytes)},
		}); err != nil {
			t.Fatalf("send audio: %v", err)
		}
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out; echoed %d of %d bytes", atomic.LoadInt64(&got), frames*frameBytes)
	}
	if got := atomic.LoadInt64(&got); got != frames*frameBytes {
		t.Fatalf("echoed %d bytes, want %d", got, frames*frameBytes)
	}
}
