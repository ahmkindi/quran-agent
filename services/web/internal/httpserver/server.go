// Package httpserver serves the browser UI (a chooser plus the /pion and
// /livekit client pages), manages the session cookie (scs), mounts the pion
// signaling WebSocket, and exposes the LiveKit token endpoint.
package httpserver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/ahmkindi/quran-agent/services/web/internal/livekit"
	"github.com/ahmkindi/quran-agent/services/web/internal/rtc"
	"github.com/ahmkindi/quran-agent/services/web/internal/signaling"
	"github.com/ahmkindi/quran-agent/services/web/internal/turncred"
	"github.com/ahmkindi/quran-agent/services/web/ui"
)

// Config configures the HTTP server.
type Config struct {
	Addr           string
	CookieSecure   bool
	OriginPatterns []string // extra allowed WS Origin hosts (same-origin always ok)
	RTC            RTCConfig
}

// RTCConfig is the ICE configuration handed to browsers via GET /rtc-config —
// the single source of ICE truth for both the pion and LiveKit pages.
type RTCConfig struct {
	StunURLs   []string // e.g. stun:stun.l.google.com:19302
	TurnURLs   []string // e.g. turn:turn.example.com:3478 (empty = no TURN)
	TurnSecret string   // coturn static-auth-secret for ephemeral creds
	ForceRelay bool     // force iceTransportPolicy=relay for ALL transports
	// LivekitForceRelay forces relay for the LiveKit page only
	// (?transport=livekit). Used where LiveKit can't resolve browser mDNS
	// candidates (same-LAN dev) while pion — which resolves mDNS — stays direct.
	LivekitForceRelay bool
}

// New builds the configured *http.Server. lk may be nil / disabled; the
// /livekit page then reports "LiveKit not configured".
func New(pion *rtc.Manager, lk *livekit.Manager, cfg Config, log *slog.Logger) *http.Server {
	sm := scs.New()
	sm.Lifetime = 12 * time.Hour
	sm.IdleTimeout = 30 * time.Minute
	sm.Cookie.Name = "quran_session"
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Secure = cfg.CookieSecure
	sm.Cookie.Path = "/"

	sessionID := func(r *http.Request) string { return sm.GetString(r.Context(), "sid") }
	sig := signaling.New(pion, cfg.OriginPatterns, sessionID, log)

	ensureSID := func(r *http.Request) string {
		sid := sm.GetString(r.Context(), "sid")
		if sid == "" {
			sid = randID()
			sm.Put(r.Context(), "sid", sid)
		}
		return sid
	}
	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ensureSID(r)
			serveAsset(w, name, "text/html; charset=utf-8", "no-cache")
		}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})

	// ICE configuration for both transports. TURN credentials are ephemeral
	// (coturn use-auth-secret scheme), minted per request, scoped to the session.
	mux.HandleFunc("GET /rtc-config", func(w http.ResponseWriter, r *http.Request) {
		type iceServer struct {
			URLs       []string `json:"urls"`
			Username   string   `json:"username,omitempty"`
			Credential string   `json:"credential,omitempty"`
		}
		var servers []iceServer
		if len(cfg.RTC.StunURLs) > 0 {
			servers = append(servers, iceServer{URLs: cfg.RTC.StunURLs})
		}
		if len(cfg.RTC.TurnURLs) > 0 && cfg.RTC.TurnSecret != "" {
			sid := ensureSID(r)
			user, cred := turncred.New(cfg.RTC.TurnSecret, sid[:8], time.Hour)
			servers = append(servers, iceServer{URLs: cfg.RTC.TurnURLs, Username: user, Credential: cred})
		}
		forceRelay := cfg.RTC.ForceRelay ||
			(r.URL.Query().Get("transport") == "livekit" && cfg.RTC.LivekitForceRelay)
		// Relay-only without a TURN server can never connect; fail open.
		if len(cfg.RTC.TurnURLs) == 0 || cfg.RTC.TurnSecret == "" {
			forceRelay = false
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store") // creds are ephemeral
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iceServers": servers,
			"forceRelay": forceRelay,
		})
	})

	// pion signaling.
	mux.Handle("/ws", sig)

	// Pages.
	mux.HandleFunc("GET /{$}", page("index.html"))
	mux.HandleFunc("GET /pion", page("pion.html"))
	mux.HandleFunc("GET /livekit", page("livekit.html"))

	// Static assets.
	mux.HandleFunc("GET /app.js", assetHandler("app.js", "application/javascript; charset=utf-8"))
	mux.HandleFunc("GET /livekit.js", assetHandler("livekit.js", "application/javascript; charset=utf-8"))
	mux.HandleFunc("GET /hud.js", assetHandler("hud.js", "application/javascript; charset=utf-8"))
	mux.HandleFunc("GET /star.js", assetHandler("star.js", "application/javascript; charset=utf-8"))
	mux.HandleFunc("GET /style.css", assetHandler("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /fonts/{file}", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, "fonts/"+r.PathValue("file"), "font/woff2", "public, max-age=31536000, immutable")
	})

	// LiveKit: create a room, spawn the bot, mint a browser token.
	mux.HandleFunc("GET /livekit/token", func(w http.ResponseWriter, r *http.Request) {
		if lk == nil || !lk.Enabled() {
			http.Error(w, "livekit disabled", http.StatusServiceUnavailable)
			return
		}
		sid := ensureSID(r)
		room := "lk-" + randID()[:12]
		if _, err := lk.StartBot(room); err != nil {
			log.Error("livekit start bot", "err", err)
			http.Error(w, "bot failed", http.StatusBadGateway)
			return
		}
		token, err := lk.Token("user-"+sid[:8], room)
		if err != nil {
			http.Error(w, "token failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"url": lk.BrowserURL(), "token": token, "room": room,
		})
	})

	return &http.Server{Addr: cfg.Addr, Handler: sm.LoadAndSave(mux)}
}

func assetHandler(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		serveAsset(w, name, contentType, "public, max-age=300")
	}
}

func serveAsset(w http.ResponseWriter, name, contentType, cache string) {
	b, err := ui.FS.ReadFile(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cache)
	_, _ = w.Write(b)
}

func randID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
