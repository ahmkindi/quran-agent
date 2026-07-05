// Package livekit is the LiveKit transport: an alternative front-door to the
// same agent-service, for A/B comparison against the raw-pion transport.
//
// Per call a Go "bot" participant joins the room, subscribes to the browser's
// mic (LiveKit decodes Opus + resamples to 16 kHz for us), bridges it to
// agent-service over the existing gRPC seam, and publishes the agent's 24 kHz
// audio back (LiveKit encodes Opus + resamples + paces — its native media path,
// which is exactly what we're comparing against our hand-rolled pion pacer).
//
// The brain (agent-service, Gemini + Quran) is unchanged; recitation audio
// flows through this path identically to the pion one.
package livekit

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	media "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/auth"
	plog "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
	"github.com/pion/webrtc/v4"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	voicev1 "github.com/ahmkindi/quran-agent/gen/voice/v1"
	"github.com/ahmkindi/quran-agent/pkg/telemetry"
	"github.com/ahmkindi/quran-agent/services/web/internal/bridge"
)

const (
	geminiInRate  = 16000 // LiveKit resamples the mic to this for Gemini
	geminiOutRate = 24000 // agent audio rate; LiveKit resamples up to 48k Opus
)

// Manager mints browser tokens and spawns per-room bots. It reuses the shared
// gRPC bridge to agent-service.
type Manager struct {
	botURL    string // ws URL the bot uses to reach LiveKit (server side)
	browser   string // ws URL the browser uses to reach LiveKit
	apiKey    string
	apiSecret string
	br        *bridge.Client
	log       *slog.Logger
}

// NewManager builds a Manager. If apiKey/secret/botURL are empty the LiveKit
// path is disabled (see Enabled).
func NewManager(botURL, browserURL, apiKey, apiSecret string, br *bridge.Client, log *slog.Logger) *Manager {
	return &Manager{botURL: botURL, browser: browserURL, apiKey: apiKey, apiSecret: apiSecret, br: br, log: log}
}

// Enabled reports whether LiveKit is configured.
func (m *Manager) Enabled() bool {
	return m.apiKey != "" && m.apiSecret != "" && m.botURL != ""
}

// BrowserURL is the ws URL the frontend should connect to.
func (m *Manager) BrowserURL() string { return m.browser }

// Token mints a browser JWT granting join/publish/subscribe on room.
func (m *Manager) Token(identity, room string) (string, error) {
	t := true
	return auth.NewAccessToken(m.apiKey, m.apiSecret).
		SetIdentity(identity).
		SetVideoGrant(&auth.VideoGrant{
			RoomJoin:     true,
			Room:         room,
			CanPublish:   &t,
			CanSubscribe: &t,
		}).
		SetValidFor(time.Hour).
		ToJWT()
}

// StartBot connects the agent bot to room and bridges audio to agent-service.
// It returns once connected + publishing; the agent call opens when the browser
// starts sending mic audio (so the greeting isn't lost).
func (m *Manager) StartBot(room string) (*Bot, error) {
	ctx, cancel := context.WithCancel(context.Background())
	// Call-level trace span; its context flows into bridge.OpenCall so the
	// agent's spans become children -> one cross-service trace, tagged livekit.
	ctx, span := telemetry.Tracer("web").Start(ctx, "web.call", trace.WithAttributes(
		attribute.String("transport", "livekit"),
		attribute.String("room", room),
	))
	b := &Bot{mgr: m, name: room, log: m.log.With("room", room), ctx: ctx, cancel: cancel, callSpan: span}

	cb := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: b.onTrackSubscribed,
			OnDataPacket:      b.onDataPacket,
		},
		OnDisconnected: func() { b.Close() },
	}

	r, err := lksdk.ConnectToRoom(m.botURL, lksdk.ConnectInfo{
		APIKey:              m.apiKey,
		APISecret:           m.apiSecret,
		RoomName:            room,
		ParticipantIdentity: "agent",
	}, cb)
	if err != nil {
		cancel()
		return nil, err
	}
	b.room = r

	// Publish an outbound PCM track; LiveKit encodes/resamples/paces it.
	pub, err := lkmedia.NewPCMLocalTrack(geminiOutRate, 1, plog.GetLogger())
	if err != nil {
		r.Disconnect()
		cancel()
		return nil, err
	}
	if _, err := r.LocalParticipant.PublishTrack(pub, &lksdk.TrackPublicationOptions{Name: "agent"}); err != nil {
		r.Disconnect()
		cancel()
		return nil, err
	}
	b.pub = pub

	b.log.Info("livekit bot joined")
	return b, nil
}

// Bot is one LiveKit call bridged to agent-service.
type Bot struct {
	mgr  *Manager
	name string
	log  *slog.Logger

	room   *lksdk.Room
	pub    *lkmedia.PCMLocalTrack
	remote *lkmedia.PCMRemoteTrack

	ctx      context.Context
	cancel   context.CancelFunc
	callSpan trace.Span

	callMu   sync.Mutex
	call     *bridge.Call
	callOnce sync.Once

	closeOnce sync.Once
}

// onTrackSubscribed fires when the browser's mic arrives. LiveKit gives us the
// remote Opus track; we wrap it in a PCMRemoteTrack that decodes+resamples to
// 16 kHz and calls our writer per sample.
func (b *Bot) onTrackSubscribed(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	b.log.Info("browser mic subscribed", "codec", track.Codec().MimeType)

	// Open the agent call now that the browser is present (greeting-safe).
	b.callOnce.Do(b.openCall)

	rt, err := lkmedia.NewPCMRemoteTrack(track, &micWriter{bot: b},
		lkmedia.WithTargetSampleRate(geminiInRate),
		lkmedia.WithTargetChannels(1),
	)
	if err != nil {
		b.log.Error("pcm remote track", "err", err)
		return
	}
	b.remote = rt
}

func (b *Bot) openCall() {
	call, err := b.mgr.br.OpenCall(b.ctx, &voicev1.CallStart{CallId: b.name, SessionId: b.name}, b.onAgentFrame)
	if err != nil {
		b.log.Error("open agent call", "err", err)
		b.Close()
		return
	}
	b.callMu.Lock()
	b.call = call
	b.callMu.Unlock()
	go func() { // agent ended the stream -> tear down
		<-call.Done()
		b.Close()
	}()
}

func (b *Bot) getCall() *bridge.Call {
	b.callMu.Lock()
	defer b.callMu.Unlock()
	return b.call
}

// onDataPacket handles browser -> bot data messages: the on-screen recitation
// control buttons ({type:"control", status:action}), mirroring pion's WS path.
func (b *Bot) onDataPacket(data lksdk.DataPacket, _ lksdk.DataReceiveParams) {
	ud, ok := data.(*lksdk.UserDataPacket)
	if !ok {
		return
	}
	var m struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(ud.Payload, &m); err != nil || m.Type != "control" {
		return
	}
	switch m.Status {
	case "stop", "again", "next", "previous":
		b.log.Info("recitation control (ui)", "action", m.Status)
		if call := b.getCall(); call != nil {
			if err := call.SendRecitationControl(m.Status); err != nil {
				b.log.Warn("ui recitation control failed", "action", m.Status, "err", err)
			}
		}
	default:
		b.log.Warn("unknown control action", "action", m.Status)
	}
}

// onAgentFrame handles ServerFrames from agent-service.
func (b *Bot) onAgentFrame(f *voicev1.ServerFrame) {
	switch p := f.Payload.(type) {
	case *voicev1.ServerFrame_AudioOut:
		if b.pub != nil {
			_ = b.pub.WriteSample(bytesToPCM16(p.AudioOut))
		}
	case *voicev1.ServerFrame_Interrupt:
		// Barge-in: drop LiveKit's queued playout so the agent stops now.
		if b.pub != nil {
			b.pub.ClearQueue()
		}
	case *voicev1.ServerFrame_Transcript:
		b.publishUI(uiEvent{
			Type: "transcript", Text: p.Transcript.GetText(),
			IsUser: p.Transcript.GetIsUser(), Final: p.Transcript.GetFinal(),
		})
	case *voicev1.ServerFrame_ToolEvent:
		b.publishUI(uiEvent{
			Type: "tool", Name: p.ToolEvent.GetName(),
			Status: p.ToolEvent.GetStatus(), Detail: p.ToolEvent.GetDetail(),
		})
	case *voicev1.ServerFrame_Hangup:
		b.log.Info("agent hangup", "reason", p.Hangup.GetReason())
		b.Close()
	}
}

// uiEvent mirrors the pion signaling WS envelope so the browser renders both
// transports with the same code; on LiveKit it rides the room data channel.
type uiEvent struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	IsUser bool   `json:"isUser,omitempty"`
	Final  bool   `json:"final,omitempty"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// publishUI sends a transcript/tool event to the browser over LiveKit's
// reliable data channel.
func (b *Bot) publishUI(ev uiEvent) {
	if b.room == nil {
		return
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := b.room.LocalParticipant.PublishDataPacket(lksdk.UserData(payload), lksdk.WithDataPublishReliable(true)); err != nil {
		b.log.Warn("publish ui event", "err", err)
	}
}

// Close tears the bot down once.
func (b *Bot) Close() {
	b.closeOnce.Do(func() {
		b.cancel()
		if call := b.getCall(); call != nil {
			_ = call.Close("livekit bot closed")
		}
		if b.remote != nil {
			b.remote.Close()
		}
		if b.pub != nil {
			b.pub.ClearQueue()
			_ = b.pub.Close()
		}
		if b.room != nil {
			b.room.Disconnect()
		}
		if b.callSpan != nil {
			b.callSpan.End()
		}
		b.log.Info("livekit bot closed")
	})
}

// micWriter receives 16 kHz PCM16 samples from LiveKit and forwards them to
// agent-service as little-endian bytes.
type micWriter struct{ bot *Bot }

func (w *micWriter) WriteSample(s media.PCM16Sample) error {
	call := w.bot.getCall()
	if call == nil {
		return nil
	}
	return call.SendAudio(pcm16ToBytes(s))
}

func (w *micWriter) Close() error { return nil }

func pcm16ToBytes(s media.PCM16Sample) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func bytesToPCM16(b []byte) media.PCM16Sample {
	s := make(media.PCM16Sample, len(b)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return s
}
