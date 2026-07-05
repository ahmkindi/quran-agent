// Package signaling exposes the WebRTC signaling WebSocket. The browser sends
// an SDP offer + trickle ICE candidates; the server replies with an answer +
// its own candidates, and pushes state/transcript/tool updates for the UI.
package signaling

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/webrtc/v4"

	"github.com/ahmkindi/quran-agent/services/web/internal/rtc"
)

// SessionIDFunc extracts the browser session id (cookie) from a request.
type SessionIDFunc func(r *http.Request) string

// Handler serves the signaling WebSocket.
type Handler struct {
	mgr            *rtc.Manager
	originPatterns []string
	sessionID      SessionIDFunc
	log            *slog.Logger
}

// New builds a signaling handler. originPatterns are extra allowed Origin hosts
// (same-origin is always allowed); sessionID maps a request to its cookie id.
func New(mgr *rtc.Manager, originPatterns []string, sessionID SessionIDFunc, log *slog.Logger) *Handler {
	return &Handler{mgr: mgr, originPatterns: originPatterns, sessionID: sessionID, log: log}
}

// msg is the JSON envelope exchanged over the socket (both directions).
type msg struct {
	Type      string                    `json:"type"`
	SDP       string                    `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit  `json:"candidate,omitempty"`
	State     string                    `json:"state,omitempty"`
	Text      string                    `json:"text,omitempty"`
	IsUser    bool                      `json:"isUser,omitempty"`
	Final     bool                      `json:"final,omitempty"`
	Name      string                    `json:"name,omitempty"`
	Status    string                    `json:"status,omitempty"`
	Detail    string                    `json:"detail,omitempty"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.originPatterns})
	if err != nil {
		h.log.Warn("ws accept failed", "err", err)
		return
	}
	defer conn.CloseNow()

	// One context for the whole socket lifetime.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sessionID := h.sessionID(r)
	callID := randID()
	log := h.log.With("call_id", callID, "session_id", sessionID)

	// Single writer goroutine: all server->browser messages funnel through out.
	out := make(chan msg, 64)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-out:
				if err := wsjson.Write(ctx, conn, m); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	send := func(m msg) {
		select {
		case out <- m:
		case <-ctx.Done():
		}
	}

	sess, err := h.mgr.NewSession(callID, sessionID)
	if err != nil {
		log.Error("new rtc session", "err", err)
		send(msg{Type: "error", Text: "failed to start call"})
		return
	}
	defer sess.Close()

	sess.OnLocalICE(func(c webrtc.ICECandidateInit) { send(msg{Type: "ice", Candidate: &c}) })
	sess.OnState(func(state string) { send(msg{Type: "state", State: state}) })
	sess.OnTranscript(func(text string, isUser, final bool) {
		send(msg{Type: "transcript", Text: text, IsUser: isUser, Final: final})
	})
	sess.OnTool(func(name, status, detail string) {
		send(msg{Type: "tool", Name: name, Status: status, Detail: detail})
	})

	log.Info("signaling open")
	for {
		var m msg
		if err := wsjson.Read(ctx, conn, &m); err != nil {
			log.Info("signaling closed", "err", err)
			return
		}
		switch m.Type {
		case "offer":
			answer, err := sess.HandleOffer(m.SDP)
			if err != nil {
				log.Error("handle offer", "err", err)
				send(msg{Type: "error", Text: "offer failed"})
				return
			}
			send(msg{Type: "answer", SDP: answer})
		case "ice":
			if m.Candidate != nil {
				if err := sess.AddRemoteICE(*m.Candidate); err != nil {
					log.Warn("add remote ice", "err", err)
				}
			}
		case "control":
			// On-screen recitation control button (same actions as the voice
			// keyword spotter): forward to the agent as a RecitationControl.
			switch m.Status {
			case "stop", "again", "next", "previous":
				log.Info("recitation control (ui)", "action", m.Status)
				if err := sess.SendRecitationControl(m.Status); err != nil {
					log.Warn("ui recitation control failed", "action", m.Status, "err", err)
				}
			default:
				log.Warn("unknown control action", "action", m.Status)
			}
		case "bye":
			_ = conn.Close(websocket.StatusNormalClosure, "bye")
			return
		}
	}
}

func randID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
