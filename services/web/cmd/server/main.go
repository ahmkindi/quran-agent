// Command web-service is the browser gateway: serves the UI, manages the
// session cookie, runs WebRTC signaling + media (pion), and bridges audio to
// agent-service over gRPC.
package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/ahmkindi/quran-agent/pkg/config"
	"github.com/ahmkindi/quran-agent/pkg/logging"
	"github.com/ahmkindi/quran-agent/pkg/telemetry"
	"github.com/ahmkindi/quran-agent/services/web/internal/bridge"
	"github.com/ahmkindi/quran-agent/services/web/internal/httpserver"
	"github.com/ahmkindi/quran-agent/services/web/internal/kws"
	"github.com/ahmkindi/quran-agent/services/web/internal/livekit"
	"github.com/ahmkindi/quran-agent/services/web/internal/rtc"
)

func main() {
	log := logging.Setup("web",
		config.Get("LOG_LEVEL", "info"),
		config.Get("LOG_FORMAT", "text"),
	)

	shutdown, otelOn, err := telemetry.Init(context.Background(), "web")
	if err != nil {
		log.Warn("telemetry init failed", "err", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	log.Info("telemetry", "tracing_enabled", otelOn)

	// Halt-word spotter (stops a recitation without the LLM). Real engine only
	// under the `sherpa` build tag; otherwise a no-op. Runs in the shared bridge
	// so both transports get it. Model dir/keywords/threshold come from env.
	var kwsThreshold float64
	if v, err := strconv.ParseFloat(config.Get("KWS_THRESHOLD", ""), 32); err == nil {
		kwsThreshold = v
	}
	spotter := kws.New(kws.Config{
		ModelDir:     config.Get("KWS_MODEL_DIR", ""),
		KeywordsFile: config.Get("KWS_KEYWORDS_FILE", ""),
		Threshold:    float32(kwsThreshold),
		NumThreads:   config.GetInt("KWS_THREADS", 1),
		Log:          log,
	})

	agentTarget := config.Get("AGENT_GRPC_TARGET", "localhost:9090")
	br, err := bridge.Dial(agentTarget, spotter, log)
	if err != nil {
		log.Error("bridge dial failed", "target", agentTarget, "err", err)
		os.Exit(1)
	}
	defer br.Close()

	pion := rtc.NewManager(iceServers(), br,
		config.GetBool("HALF_DUPLEX", true),
		time.Duration(config.GetInt("MIC_HANGOVER_MS", 300))*time.Millisecond,
		log)
	// Pin ICE media to a fixed UDP range ("min-max") so the server firewall can
	// open exactly those ports; required behind default-deny INPUT (see PROD.md).
	if r := config.Get("PION_UDP_PORT_RANGE", ""); r != "" {
		lo, hi, ok := strings.Cut(r, "-")
		min, err1 := strconv.Atoi(strings.TrimSpace(lo))
		max, err2 := strconv.Atoi(strings.TrimSpace(hi))
		if !ok || err1 != nil || err2 != nil || min < 1 || max > 65535 || min > max {
			log.Error("invalid PION_UDP_PORT_RANGE (want e.g. 50700-50900)", "value", r)
			os.Exit(1)
		}
		if err := pion.SetUDPPortRange(uint16(min), uint16(max)); err != nil {
			log.Error("set udp port range failed", "err", err)
			os.Exit(1)
		}
		log.Info("pion udp port range pinned", "range", r)
	}

	// LiveKit transport (optional; enabled when LIVEKIT_* is set).
	lk := livekit.NewManager(
		config.Get("LIVEKIT_HOST", ""),   // bot -> livekit (server side)
		config.Get("LIVEKIT_WS_URL", ""), // browser -> livekit
		config.Get("LIVEKIT_API_KEY", ""),
		config.Get("LIVEKIT_API_SECRET", ""),
		br, log)
	if lk.Enabled() {
		log.Info("livekit transport enabled", "browser_url", lk.BrowserURL())
	} else {
		log.Info("livekit transport disabled (set LIVEKIT_* to enable)")
	}

	srv := httpserver.New(pion, lk, httpserver.Config{
		Addr:           config.Get("WEB_HTTP_ADDR", "0.0.0.0:8080"),
		CookieSecure:   config.GetBool("COOKIE_SECURE", false),
		OriginPatterns: originHosts(config.GetList("ALLOWED_ORIGINS", nil)),
		RTC: httpserver.RTCConfig{
			StunURLs:          config.GetList("STUN_SERVERS", nil),
			TurnURLs:          config.GetList("TURN_URLS", nil),
			TurnSecret:        config.Get("TURN_SECRET", ""),
			ForceRelay:        config.GetBool("FORCE_RELAY", false),
			LivekitForceRelay: config.GetBool("LIVEKIT_FORCE_RELAY", false),
		},
	}, log)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Info("web-service listening", "addr", srv.Addr, "agent_target", agentTarget)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("serve failed", "err", err)
		os.Exit(1)
	}
}

// iceServers builds the ICE config from env (STUN + optional TURN).
func iceServers() []webrtc.ICEServer {
	var s []webrtc.ICEServer
	for _, u := range config.GetList("STUN_SERVERS", nil) {
		s = append(s, webrtc.ICEServer{URLs: []string{u}})
	}
	if turn := config.Get("TURN_URL", ""); turn != "" {
		s = append(s, webrtc.ICEServer{
			URLs:       []string{turn},
			Username:   config.Get("TURN_USERNAME", ""),
			Credential: config.Get("TURN_PASSWORD", ""),
		})
	}
	return s
}

// originHosts converts configured Origin URLs to host[:port] patterns for the
// WebSocket Origin allowlist.
func originHosts(origins []string) []string {
	var hosts []string
	for _, o := range origins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			hosts = append(hosts, u.Host)
		} else if o != "" {
			hosts = append(hosts, strings.TrimSpace(o))
		}
	}
	return hosts
}
