// Command webcheck is a headless browser emulator for end-to-end verification.
// It performs the real WebRTC signaling handshake against web-service /ws using
// pion (the same SDP/trickle-ICE flow a browser does), receives the agent's
// audio track, decodes it, and writes a WAV — proving the whole pipeline
// (web-service -> agent-service -> Gemini Live -> back) without a GUI browser.
//
// Requires the agent's greeting enabled (AGENT_GREETING) so audio flows without
// real mic input.
//
// Usage: go run ./services/web/cmd/webcheck -url ws://localhost:8088/ws -secs 8
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"layeh.com/gopus"
)

type msg struct {
	Type      string                   `json:"type"`
	SDP       string                   `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	State     string                   `json:"state,omitempty"`
	Text      string                   `json:"text,omitempty"`
	IsUser    bool                     `json:"isUser,omitempty"`
	Final     bool                     `json:"final,omitempty"`
	Name      string                   `json:"name,omitempty"`
	Status    string                   `json:"status,omitempty"`
}

func main() {
	wsURL := flag.String("url", "ws://localhost:8088/ws", "signaling WebSocket URL")
	secs := flag.Int("secs", 8, "seconds to listen for agent audio")
	out := flag.String("out", "webcheck.wav", "output WAV (24 kHz)")
	in := flag.String("in", "", "optional mic WAV to send (48 kHz mono s16le), emulating speech")
	var taps tapFlags
	flag.Var(&taps, "tap", "send a UI control at a time, e.g. -tap next@14 (repeatable)")
	flag.Parse()
	if err := run(*wsURL, *secs, *out, *in, taps); err != nil {
		fmt.Fprintln(os.Stderr, "webcheck:", err)
		os.Exit(1)
	}
}

// tapFlags parses repeated -tap action@secs values (UI control button presses).
type tapFlags []struct {
	action string
	at     time.Duration
}

func (t *tapFlags) String() string { return fmt.Sprintf("%d taps", len(*t)) }

func (t *tapFlags) Set(v string) error {
	action, secsStr, ok := strings.Cut(v, "@")
	if !ok {
		return errors.New("want action@secs, e.g. next@14")
	}
	secs, err := strconv.ParseFloat(secsStr, 64)
	if err != nil {
		return err
	}
	*t = append(*t, struct {
		action string
		at     time.Duration
	}{action, time.Duration(secs * float64(time.Second))})
	return nil
}

func run(wsURL string, secs int, outPath, inPath string, taps tapFlags) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs+15)*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.CloseNow()
	send := func(m msg) error { return wsjson.Write(ctx, conn, m) }

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		return err
	}
	defer pc.Close()

	// With -in we add a mic track (like a browser's getUserMedia); otherwise
	// receive-only, just listening to the agent's greeting.
	var micTrack *webrtc.TrackLocalStaticSample
	if inPath != "" {
		// Channels: 2 (opus/48000/2) or pion rejects the SDP; payload stays mono.
		micTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
		}, "audio", "webcheck-mic")
		if err != nil {
			return err
		}
		if _, err := pc.AddTrack(micTrack); err != nil {
			return err
		}
	} else if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return err
	}

	dec, err := gopus.NewDecoder(24000, 1)
	if err != nil {
		return err
	}
	var outPCM []byte
	var frames int64

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		fmt.Println("[recv track]", track.Codec().MimeType)
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if len(pkt.Payload) == 0 {
				continue
			}
			pcm, err := dec.Decode(pkt.Payload, 2880, false)
			if err != nil {
				continue
			}
			atomic.AddInt64(&frames, 1)
			b := make([]byte, len(pcm)*2)
			for i, s := range pcm {
				binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
			}
			outPCM = append(outPCM, b...)
		}
	})
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			ci := c.ToJSON()
			_ = send(msg{Type: "ice", Candidate: &ci})
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Println("[pc state]", s.String())
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	if err := send(msg{Type: "offer", SDP: offer.SDP}); err != nil {
		return err
	}

	// Stream the mic WAV as paced 20 ms Opus samples once connected, then keep
	// sending silence so server VAD/endpointing has a live feed.
	if micTrack != nil {
		pcm48, err := readWAV48k(inPath)
		if err != nil {
			return err
		}
		go func() {
			enc, err := gopus.NewEncoder(48000, 1, gopus.Voip)
			if err != nil {
				fmt.Println("[mic] encoder:", err)
				return
			}
			const frame = 960 // 20 ms @ 48 kHz
			samples := make([]int16, frame)
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			off := 0
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				for i := range samples {
					samples[i] = 0
				}
				if off < len(pcm48) {
					n := min(frame, len(pcm48)-off)
					copy(samples, pcm48[off:off+n])
					off += n
					if off >= len(pcm48) {
						fmt.Println("[mic] input fully sent; streaming silence")
					}
				}
				pkt, err := enc.Encode(samples, frame, 4000)
				if err != nil {
					continue
				}
				_ = micTrack.WriteSample(media.Sample{Data: pkt, Duration: 20 * time.Millisecond})
			}
		}()
	}

	// Scheduled UI control taps (the on-screen button path).
	for _, tp := range taps {
		go func(action string, at time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(at):
				fmt.Printf("[tap] %s\n", action)
				_ = send(msg{Type: "control", Status: action})
			}
		}(tp.action, tp.at)
	}

	// Read signaling until the listen window elapses.
	go func() {
		for {
			var m msg
			if err := wsjson.Read(ctx, conn, &m); err != nil {
				return
			}
			switch m.Type {
			case "answer":
				_ = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP})
			case "ice":
				if m.Candidate != nil {
					_ = pc.AddICECandidate(*m.Candidate)
				}
			case "transcript":
				who := "agent"
				if m.IsUser {
					who = "user"
				}
				fmt.Printf("[transcript %s%s] %s\n", who, mark(m.Final), m.Text)
			case "tool":
				fmt.Printf("[tool %s] %s\n", m.Status, m.Name)
			case "error":
				fmt.Println("[server error]", m.Text)
			}
		}
	}()

	time.Sleep(time.Duration(secs) * time.Second)
	_ = send(msg{Type: "bye"})

	if err := writeWAV(outPath, outPCM, 24000); err != nil {
		return err
	}
	fmt.Printf("\nresult: audio_frames=%d audio_bytes=%d -> %s (%.1fs @24kHz)\n",
		atomic.LoadInt64(&frames), len(outPCM), outPath, float64(len(outPCM))/2/24000)
	if len(outPCM) == 0 {
		return fmt.Errorf("no agent audio received over WebRTC")
	}
	return nil
}

// readWAV48k reads a canonical PCM16 mono WAV and returns its samples,
// requiring a 48 kHz rate (the WebRTC Opus clock).
func readWAV48k(path string) ([]int16, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF/WAVE file")
	}
	if rate := binary.LittleEndian.Uint32(b[24:28]); rate != 48000 {
		return nil, fmt.Errorf("want 48000 Hz mic WAV, got %d", rate)
	}
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		sz := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		if id == "data" {
			end := min(body+sz, len(b))
			out := make([]int16, (end-body)/2)
			for i := range out {
				out[i] = int16(binary.LittleEndian.Uint16(b[body+i*2:]))
			}
			return out, nil
		}
		off = body + sz + (sz & 1)
	}
	return nil, errors.New("no data chunk")
}

func mark(final bool) string {
	if final {
		return ""
	}
	return "…"
}

func writeWAV(path string, pcm []byte, rate int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := func(v any) { _ = binary.Write(f, binary.LittleEndian, v) }
	f.WriteString("RIFF")
	w(uint32(36 + len(pcm)))
	f.WriteString("WAVE")
	f.WriteString("fmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(1))
	w(uint32(rate))
	w(uint32(rate * 2))
	w(uint16(2))
	w(uint16(16))
	f.WriteString("data")
	w(uint32(len(pcm)))
	_, err = f.Write(pcm)
	return err
}
