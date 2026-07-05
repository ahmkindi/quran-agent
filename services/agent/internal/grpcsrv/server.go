package grpcsrv

import (
	"errors"
	"io"
	"log/slog"
	"sync"

	voicev1 "github.com/ahmkindi/quran-agent/gen/voice/v1"
)

// Server implements the VoiceBridge gRPC service. It owns the per-call stream
// lifecycle and delegates the actual audio handling to an Engine.
type Server struct {
	voicev1.UnimplementedVoiceBridgeServer
	engine Engine
	log    *slog.Logger
}

// NewServer wires an Engine into the gRPC service.
func NewServer(engine Engine, log *slog.Logger) *Server {
	return &Server{engine: engine, log: log}
}

// Stream handles one bidirectional call: recv loop on this goroutine, while the
// Engine pushes ServerFrames back through a serialized sink.
func (s *Server) Stream(stream voicev1.VoiceBridge_StreamServer) error {
	ctx := stream.Context()

	// The first frame must be CallStart.
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetCallStart()
	if start == nil {
		return errors.New("first frame must be call_start")
	}
	log := s.log.With("call_id", start.GetCallId(), "session_id", start.GetSessionId())
	log.Info("call started", "lang_hint", start.GetLangHint())

	// gRPC ServerStream.Send is not safe for concurrent use; serialize it.
	var sendMu sync.Mutex
	sink := func(f *voicev1.ServerFrame) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(f)
	}

	call, err := s.engine.StartCall(ctx, start, sink)
	if err != nil {
		log.Error("engine start failed", "err", err)
		return err
	}
	defer func() {
		if err := call.Close(); err != nil {
			log.Warn("call close error", "err", err)
		}
		log.Info("call ended")
	}()

	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch p := frame.Payload.(type) {
		case *voicev1.ClientFrame_AudioIn:
			if err := call.PushAudio(p.AudioIn); err != nil {
				log.Warn("push audio failed", "err", err)
			}
		case *voicev1.ClientFrame_RecitationControl:
			if err := call.ControlPlayback(p.RecitationControl.GetAction()); err != nil {
				log.Warn("recitation control failed", "action", p.RecitationControl.GetAction(), "err", err)
			}
		case *voicev1.ClientFrame_CallEnd:
			log.Info("client ended call", "reason", p.CallEnd.GetReason())
			return nil
		case *voicev1.ClientFrame_CallStart:
			log.Warn("unexpected second call_start; ignoring")
		}
	}
}
