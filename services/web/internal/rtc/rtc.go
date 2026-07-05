// Package rtc terminates one browser WebRTC call (pion) and bridges its audio
// to agent-service:
//
//	mic Opus --decode 16k--> gRPC AudioIn --> agent (Gemini Live)
//	agent AudioOut 24k --frame--> Opus encode --> browser track
//
// Outbound audio is paced at 20 ms (time.Ticker) and the queue is flushed on
// barge-in (Interrupt) for snappy turn-taking.
package rtc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	voicev1 "github.com/ahmkindi/quran-agent/gen/voice/v1"
	"github.com/ahmkindi/quran-agent/pkg/telemetry"
	"github.com/ahmkindi/quran-agent/services/web/internal/audio"
	"github.com/ahmkindi/quran-agent/services/web/internal/bridge"
)

const frameDuration = audio.FrameMS * time.Millisecond

// primeFrames is the outbound jitter cushion: pace() waits until this many 20ms
// frames are buffered before starting a turn, absorbing Gemini delivery stalls.
const primeFrames = 3 // ~60ms

// Manager holds shared config and the agent bridge; it mints per-call Sessions.
type Manager struct {
	ice        []webrtc.ICEServer
	api        *webrtc.API // non-nil when a UDP port range is pinned
	bridge     *bridge.Client
	log        *slog.Logger
	halfDuplex bool          // suppress mic while agent speaks (kills self-echo)
	hangover   time.Duration // keep mic muted this long after agent stops
}

// NewManager builds a Manager. halfDuplex + hangover control acoustic-echo
// suppression: while the agent is speaking (and for `hangover` after), inbound
// mic audio is not forwarded, so the agent's own voice can't trigger barge-in.
func NewManager(ice []webrtc.ICEServer, br *bridge.Client, halfDuplex bool, hangover time.Duration, log *slog.Logger) *Manager {
	return &Manager{ice: ice, bridge: br, halfDuplex: halfDuplex, hangover: hangover, log: log}
}

// SetUDPPortRange pins ICE media sockets to [min, max] so a stateful firewall
// (e.g. ufw with default-deny INPUT) can open exactly that range. Without it,
// pion binds fully ephemeral ports and inbound connectivity checks from
// symmetric-NAT clients are dropped before pion can form prflx pairs.
func (m *Manager) SetUDPPortRange(min, max uint16) error {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		return err
	}
	se := webrtc.SettingEngine{}
	if err := se.SetEphemeralUDPPortRange(min, max); err != nil {
		return err
	}
	m.api = webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithInterceptorRegistry(ir),
		webrtc.WithSettingEngine(se),
	)
	return nil
}

// LocalICEFunc is called for each locally gathered ICE candidate (trickle).
type LocalICEFunc func(webrtc.ICECandidateInit)

// StateFunc is called when the peer connection state changes.
type StateFunc func(state string)

// TranscriptFunc forwards a live transcript segment to the UI.
type TranscriptFunc func(text string, isUser, final bool)

// ToolFunc forwards a tool event to the UI. detail is an optional JSON payload
// (e.g. the verse metadata of a recitation "verse" event).
type ToolFunc func(name, status, detail string)

// Session is one live WebRTC call.
type Session struct {
	mgr       *Manager
	callID    string
	sessionID string
	log       *slog.Logger

	pc    *webrtc.PeerConnection
	local *webrtc.TrackLocalStaticSample

	enc    *audio.Encoder
	dec    *audio.Decoder
	framer audio.Framer // bridge-callback goroutine only

	outMu          sync.Mutex
	outQ           [][]byte // 20ms 24kHz PCM frames awaiting send, paced out
	firstAudioAt   time.Time // start of the current outbound burst (playout timing)
	playoutPending bool      // a burst arrived, first frame not yet written
	callReady      chan struct{}

	callMu sync.Mutex
	call   *bridge.Call

	halfDuplex  bool
	hangover    time.Duration
	muteUntilNs atomic.Int64 // mic suppressed until this wall-clock (echo guard)
	recitation  atomic.Bool  // true while reciter audio plays: keep mic open for the stop word

	onLocalICE   LocalICEFunc
	onState      StateFunc
	onTranscript TranscriptFunc
	onTool       ToolFunc

	ctx       context.Context
	cancel    context.CancelFunc
	callSpan  trace.Span
	closeOnce sync.Once
}

// NewSession builds the PeerConnection, local Opus track, codecs, and starts
// the pacing goroutine. Callers must set OnLocalICE before HandleOffer.
func (m *Manager) NewSession(callID, sessionID string) (*Session, error) {
	newPC := webrtc.NewPeerConnection
	if m.api != nil {
		newPC = m.api.NewPeerConnection
	}
	pc, err := newPC(webrtc.Configuration{ICEServers: m.ice})
	if err != nil {
		return nil, err
	}

	local, err := webrtc.NewTrackLocalStaticSample(
		// Declare 2 channels to match the standard WebRTC Opus rtpmap
		// (opus/48000/2); the payload itself is mono, which Opus handles fine.
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "quran-agent",
	)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if _, err := pc.AddTrack(local); err != nil {
		_ = pc.Close()
		return nil, err
	}

	enc, err := audio.NewEncoder()
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	dec, err := audio.NewDecoder()
	if err != nil {
		_ = pc.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Call-level trace span; its context flows into bridge.OpenCall so the
	// agent's spans become children -> one cross-service trace, tagged pion.
	ctx, span := telemetry.Tracer("web").Start(ctx, "web.call", trace.WithAttributes(
		attribute.String("transport", "pion"),
		attribute.String("call_id", callID),
		attribute.Int("prime_frames", primeFrames),
		attribute.Int64("hangover_ms", m.hangover.Milliseconds()),
	))
	s := &Session{
		mgr: m, callID: callID, sessionID: sessionID,
		log:        m.log.With("call_id", callID),
		pc:         pc,
		local:      local,
		enc:        enc,
		dec:        dec,
		halfDuplex: m.halfDuplex,
		hangover:   m.hangover,
		callReady:  make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
		callSpan:   span,
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || s.onLocalICE == nil {
			return
		}
		s.onLocalICE(c.ToJSON())
	})
	pc.OnConnectionStateChange(s.onConnState)
	pc.OnTrack(s.onTrack)

	go s.pace()
	return s, nil
}

// OnLocalICE registers the trickle-ICE callback (web-service -> browser).
func (s *Session) OnLocalICE(f LocalICEFunc) { s.onLocalICE = f }

// OnState registers a connection-state callback.
func (s *Session) OnState(f StateFunc) { s.onState = f }

// OnTranscript registers a transcript callback.
func (s *Session) OnTranscript(f TranscriptFunc) { s.onTranscript = f }

// OnTool registers a tool-event callback.
func (s *Session) OnTool(f ToolFunc) { s.onTool = f }

// HandleOffer applies the browser offer and returns the SDP answer.
func (s *Session) HandleOffer(offerSDP string) (string, error) {
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		return "", err
	}
	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	return answer.SDP, nil
}

// AddRemoteICE adds a trickle candidate from the browser.
func (s *Session) AddRemoteICE(c webrtc.ICECandidateInit) error {
	return s.pc.AddICECandidate(c)
}

// SendRecitationControl forwards an on-screen control button action
// (stop/again/next/previous) to the agent, same path as the voice spotter.
func (s *Session) SendRecitationControl(action string) error {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	if s.call == nil {
		return errors.New("no active call")
	}
	return s.call.SendRecitationControl(action)
}

func (s *Session) onConnState(state webrtc.PeerConnectionState) {
	s.log.Info("pc state", "state", state.String())
	if s.onState != nil {
		s.onState(state.String())
	}
	switch state {
	case webrtc.PeerConnectionStateConnected:
		s.openCall()
	case webrtc.PeerConnectionStateFailed,
		webrtc.PeerConnectionStateClosed,
		webrtc.PeerConnectionStateDisconnected:
		s.Close()
	}
}

// openCall opens the agent bridge stream once the media path is up, so the
// agent's greeting isn't lost before the browser can hear it.
func (s *Session) openCall() {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	if s.call != nil {
		return
	}
	call, err := s.mgr.bridge.OpenCall(s.ctx, &voicev1.CallStart{
		CallId: s.callID, SessionId: s.sessionID,
	}, s.onAgentFrame)
	if err != nil {
		s.log.Error("open agent call failed", "err", err)
		s.Close()
		return
	}
	s.call = call
	close(s.callReady)
	go func() { // agent ended the stream -> tear down
		<-call.Done()
		s.Close()
	}()
}

// onTrack pumps mic Opus -> 16kHz PCM -> agent.
func (s *Session) onTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	s.log.Info("mic track", "codec", track.Codec().MimeType)

	select {
	case <-s.callReady:
	case <-s.ctx.Done():
		return
	}
	s.callMu.Lock()
	call := s.call
	s.callMu.Unlock()
	if call == nil {
		return
	}

	for {
		pkt, _, err := track.ReadRTP()
		if errors.Is(err, io.EOF) || err != nil {
			return
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		// Echo guard: while the agent is speaking (+hangover), don't forward mic
		// audio, so the agent's own voice leaking into the mic can't trigger a
		// barge-in and cut it off. Keep reading RTP to avoid backpressure.
		// Exception: during a Quran recitation the mic stays open so the driver
		// (who may recite along) can still be heard saying the stop word;
		// barge-in is suppressed server-side, so echo can't cut the recitation.
		if s.halfDuplex && !s.recitation.Load() && time.Now().UnixNano() < s.muteUntilNs.Load() {
			continue
		}
		pcm, err := s.dec.Decode(pkt.Payload)
		if err != nil {
			s.log.Warn("opus decode", "err", err)
			continue
		}
		if err := call.SendAudio(pcm); err != nil {
			return
		}
	}
}

// onAgentFrame handles ServerFrames (single goroutine, from the bridge recv loop).
func (s *Session) onAgentFrame(f *voicev1.ServerFrame) {
	switch p := f.Payload.(type) {
	case *voicev1.ServerFrame_AudioOut:
		// Gemini streams audio faster than real-time; buffer all of it and let
		// the pacer emit one frame per 20ms. Never drop mid-utterance (that was
		// the "cracking voice" bug). Turn length bounds the queue; flushed below.
		frames := s.framer.Push(p.AudioOut)
		if len(frames) > 0 {
			s.outMu.Lock()
			if len(s.outQ) == 0 && !s.playoutPending {
				s.firstAudioAt = time.Now() // start of a new outbound burst
				s.playoutPending = true
			}
			s.outQ = append(s.outQ, frames...)
			s.outMu.Unlock()
		}
	case *voicev1.ServerFrame_Interrupt:
		// Barge-in: flush everything queued so the agent stops talking now.
		s.framer.Reset()
		s.outMu.Lock()
		s.outQ = nil
		s.playoutPending = false
		s.outMu.Unlock()
	case *voicev1.ServerFrame_Transcript:
		if s.onTranscript != nil {
			s.onTranscript(p.Transcript.GetText(), p.Transcript.GetIsUser(), p.Transcript.GetFinal())
		}
	case *voicev1.ServerFrame_ToolEvent:
		// "recitation" doubles as a mic-gate control: while reciter audio plays
		// the mic stays open for the stop word. Only "start"/"stop" toggle the
		// gate ("verse" is UI metadata mid-recitation). All recitation events
		// are also forwarded so the UI can render the now-playing panel.
		if p.ToolEvent.GetName() == "recitation" {
			switch p.ToolEvent.GetStatus() {
			case "start":
				s.recitation.Store(true)
			case "stop":
				s.recitation.Store(false)
			}
		}
		if s.onTool != nil {
			s.onTool(p.ToolEvent.GetName(), p.ToolEvent.GetStatus(), p.ToolEvent.GetDetail())
		}
	case *voicev1.ServerFrame_Hangup:
		s.log.Info("agent hangup", "reason", p.Hangup.GetReason())
		s.Close()
	}
}

// pace writes one 20ms Opus frame to the browser track every 20ms. It keeps a
// small jitter cushion: it waits until `primeFrames` are buffered before
// starting a turn, so brief stalls in Gemini's delivery don't cause underruns
// (the "itchy" clicks). Since Gemini bursts audio faster than real-time, the
// cushion fills instantly and only adds ~60ms to first-audio.
func (s *Session) pace() {
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()
	playing := false
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.outMu.Lock()
			n := len(s.outQ)
			switch {
			case n == 0:
				playing = false
				s.outQ = nil // release backing array between turns
			case !playing && n >= primeFrames:
				playing = true
			}
			var frame []byte
			var firstOfBurst bool
			var burstStart time.Time
			if playing && n > 0 {
				frame = s.outQ[0]
				s.outQ = s.outQ[1:]
				if s.playoutPending {
					s.playoutPending = false
					firstOfBurst = true
					burstStart = s.firstAudioAt
				}
			}
			s.outMu.Unlock()
			if frame == nil {
				continue // priming or idle; stay silent
			}
			pkt, err := s.enc.EncodeFrame(frame)
			if err != nil {
				s.log.Warn("opus encode", "err", err)
				continue
			}
			if err := s.local.WriteSample(media.Sample{Data: pkt, Duration: frameDuration}); err != nil {
				s.log.Warn("write sample", "err", err)
			}
			if firstOfBurst {
				// Time from first agent audio received to first frame written =
				// our outbound jitter-buffer (prime) cost.
				d := time.Since(burstStart)
				s.log.Info("playout latency", "transport", "pion", "playout_ms", d.Milliseconds())
				_, sp := telemetry.Tracer("web").Start(s.ctx, "web.playout",
					trace.WithTimestamp(burstStart),
					trace.WithAttributes(
						attribute.String("transport", "pion"),
						attribute.Int64("playout_ms", d.Milliseconds()),
					),
				)
				sp.End()
			}
			// Agent is speaking now: mute the mic through the hangover window.
			if s.halfDuplex {
				s.muteUntilNs.Store(time.Now().Add(s.hangover).UnixNano())
			}
		}
	}
}

// Close tears down the call once.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		s.callMu.Lock()
		call := s.call
		s.callMu.Unlock()
		if call != nil {
			_ = call.Close("session closed")
		}
		_ = s.pc.Close()
		if s.callSpan != nil {
			s.callSpan.End()
		}
		s.log.Info("session closed")
	})
}
