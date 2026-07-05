// Package adkengine is the real agent brain: an ADK-Go agent driving the Gemini
// Live API (native speech-to-speech). It implements grpcsrv.Engine.
//
// Per call it opens a Gemini Live session via runner.RunLive, feeds 16 kHz mic
// PCM in, and streams the model's 24 kHz audio + transcripts + barge-in
// (interrupt) events back through the sink. VAD/endpointing/interruption are
// handled server-side by Gemini.
//
// For the Quran companion it also injects real reciter audio: the play_* tools
// fetch an MP3, decode it to 24 kHz PCM, and stream it on the same outbound
// audio path (see playback.go, quran_tools.go). While that recitation plays the
// driver may recite along, so Gemini's own audio and barge-in are suppressed —
// only the configured stop word ends playback.
package adkengine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genai"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"

	voicev1 "github.com/ahmkindi/quran-agent/gen/voice/v1"
	"github.com/ahmkindi/quran-agent/pkg/telemetry"
	"github.com/ahmkindi/quran-agent/services/agent/internal/grpcsrv"
	"github.com/ahmkindi/quran-agent/services/agent/internal/quran"
)

const (
	// Gemini Live audio formats (fixed by the API).
	inputAudioMIME = "audio/pcm;rate=16000"
)

// Settings configure the engine. Most come from environment.
type Settings struct {
	Model          string // e.g. gemini-3.1-flash-live-preview
	Voice          string // prebuilt voice name; empty = model default
	Instruction    string // system prompt / persona
	Greeting       string // optional: text turn sent on connect to elicit a spoken greeting
	SilenceMs      int    // end-of-speech silence threshold (endpointing); 0 = model default
	EndSensitivity string // end-of-speech sensitivity: "high" | "low" | "" = model default
	APIKey         string // Gemini API key (dev). Empty -> Vertex AI via Backend/Project/Location.
	UseVertex      bool
	VertexProj     string
	VertexRegion   string

	// Quran companion settings.
	Reciter         string // recitation audio edition, e.g. "ar.mahermuaiqly"
	TranslationEd   string // default translation edition, e.g. "en.sahih"
	TafsirEd        string // default tafsir edition, e.g. "en-tafisr-ibn-kathir"
	AudioBitrate    int    // recitation MP3 bitrate (128 or 64)
	AudioCacheBytes int    // decoded-PCM cache budget in bytes (0 = default 32 MiB)
}

// Engine holds one Runner shared across all calls, plus the shared Quran client
// (HTTP + decoded-PCM cache) and a registry of live calls so tools can reach the
// call that invoked them.
type Engine struct {
	runner   *runner.Runner
	settings Settings
	qc       *quran.Client
	log      *slog.Logger
	tracer   trace.Tracer

	// prefetchSem bounds concurrent background decodes so warming can't spike CPU
	// or transient memory on the small (2 GB) deploy box.
	prefetchSem chan struct{}

	calls sync.Map // sessionID -> *call
}

// New builds the model, agent (with tools), and runner.
func New(ctx context.Context, s Settings, log *slog.Logger) (*Engine, error) {
	e := &Engine{
		settings:    s,
		log:         log,
		tracer:      telemetry.Tracer("agent"),
		prefetchSem: make(chan struct{}, 2),
		qc: quran.New(quran.Options{
			Reciter:     s.Reciter,
			Translation: s.TranslationEd,
			Tafsir:      s.TafsirEd,
			Bitrate:     s.AudioBitrate,
			CacheBytes:  s.AudioCacheBytes,
			Log:         log,
		}),
	}

	clientCfg := &genai.ClientConfig{}
	if s.UseVertex {
		clientCfg.Backend = genai.BackendVertexAI
		clientCfg.Project = s.VertexProj
		clientCfg.Location = s.VertexRegion
	} else {
		clientCfg.APIKey = s.APIKey
	}

	model, err := gemini.NewModel(ctx, s.Model, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("create model %q: %w", s.Model, err)
	}

	a, err := llmagent.New(llmagent.Config{
		Name:        "al_kindi",
		Model:       model,
		Description: "Hands-free multilingual Quran companion for drivers.",
		Instruction: s.Instruction,
		Tools:       e.quranTools(),
	})
	if err != nil {
		return nil, fmt.Errorf("create agent: %w", err)
	}

	r, err := runner.New(runner.Config{
		AppName:           "quran-agent",
		Agent:             a,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create runner: %w", err)
	}
	e.runner = r

	// Warm the cache with a few common verses so the first play is instant
	// (async; never blocks startup).
	go e.warmup()

	return e, nil
}

// liveRunConfig builds the per-call Live configuration.
func (e *Engine) liveRunConfig() agent.LiveRunConfig {
	cfg := agent.LiveRunConfig{
		// Audio out (speech-to-speech).
		ResponseModalities: []genai.Modality{genai.ModalityAudio},
		// This is a Quran companion for Arabic recitation + English/Arabic talk, so
		// bias ASR to those two languages (fewer wrong-language detections). Output
		// language stays auto (SpeechConfig.LanguageCode empty) so the agent can use
		// either; the prompt keeps it to English/Arabic.
		InputAudioTranscription:  &genai.AudioTranscriptionConfig{LanguageHints: &genai.LanguageHints{LanguageCodes: []string{"ar", "en"}}},
		OutputAudioTranscription: &genai.AudioTranscriptionConfig{LanguageAuto: &genai.LanguageAuto{}},
	}
	if e.settings.Voice != "" {
		cfg.SpeechConfig = &genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: e.settings.Voice},
			},
			// LanguageCode intentionally empty -> multilingual auto-switch.
		}
	}
	// Less trigger-happy endpointing: require clearer speech to start a turn, so
	// residual acoustic echo of the agent's own voice is less likely to barge-in.
	// End-of-speech is the biggest controllable latency lever: the server default
	// silence threshold is ~800 ms, which alone exceeds the latency budget.
	aad := &genai.AutomaticActivityDetection{
		StartOfSpeechSensitivity: genai.StartSensitivityLow,
	}
	if e.settings.SilenceMs > 0 {
		ms := int32(e.settings.SilenceMs)
		aad.SilenceDurationMs = &ms
	}
	switch strings.ToLower(e.settings.EndSensitivity) {
	case "high":
		aad.EndOfSpeechSensitivity = genai.EndSensitivityHigh
	case "low":
		aad.EndOfSpeechSensitivity = genai.EndSensitivityLow
	}
	cfg.RealtimeInputConfig = &genai.RealtimeInputConfig{AutomaticActivityDetection: aad}

	// Survive network blips: ADK auto-reconnects with the resumption handle when
	// this is set. Sliding-window compression lifts the ~15 min hard limit on
	// audio-only sessions (field exists via the third_party/adk patch).
	cfg.SessionResumption = &genai.SessionResumptionConfig{}
	cfg.ContextWindowCompression = &genai.ContextWindowCompressionConfig{
		SlidingWindow: &genai.SlidingWindow{},
	}
	return cfg
}

// StartCall opens a Gemini Live session and pumps events back through sink.
func (e *Engine) StartCall(ctx context.Context, start *voicev1.CallStart, sink grpcsrv.Sink) (grpcsrv.Call, error) {
	userID := start.GetSessionId()
	if userID == "" {
		userID = "anon"
	}
	sessionID := start.GetCallId()

	liveSession, events, err := e.runner.RunLive(ctx, userID, sessionID, e.liveRunConfig())
	if err != nil {
		return nil, fmt.Errorf("run live: %w", err)
	}

	c := &call{
		eng:       e,
		sessionID: sessionID,
		live:      liveSession,
		sink:      sink,
		log:       e.log.With("call_id", sessionID),
		done:      make(chan struct{}),
	}
	// Call-level trace span; per-turn spans hang off spanCtx. Child of the
	// context propagated from web-service via the gRPC otel handler.
	c.spanCtx, c.callSpan = e.tracer.Start(ctx, "agent.call", trace.WithAttributes(
		attribute.String("call_id", sessionID),
		attribute.String("model", e.settings.Model),
	))
	e.calls.Store(sessionID, c)

	// Pump model events -> ServerFrames.
	go c.pump(events)

	// Optionally nudge the agent to greet first.
	if e.settings.Greeting != "" {
		if err := liveSession.Send(agent.LiveRequest{
			Content: genai.NewContentFromText(e.settings.Greeting, genai.RoleUser),
		}); err != nil {
			c.log.Warn("greeting send failed", "err", err)
		} else {
			c.callSpan.AddEvent("greeting sent")
		}
	}

	return c, nil
}

// lookup returns the call that owns the given tool invocation, via sessionID.
func (e *Engine) lookup(sessionID string) (*call, bool) {
	v, ok := e.calls.Load(sessionID)
	if !ok {
		return nil, false
	}
	return v.(*call), true
}

type call struct {
	eng       *Engine
	sessionID string
	live      agent.LiveSession
	sink      grpcsrv.Sink
	log       *slog.Logger
	done      chan struct{}
	closeOne  sync.Once

	// latency instrumentation (touched only by the pump goroutine + Close,
	// except lastSpeechNano/turnHadTool which other goroutines read/write)
	spanCtx         context.Context
	callSpan        trace.Span
	lastUserInput   time.Time // most recent input-transcription event of the turn
	awaitFirstAudio bool
	turnHadTool     atomic.Bool
	turnID          int
	turnLatMs       []int64 // per-turn gemini_ms, for the call summary
	toolCalls       int
	// lastSpeechNano is when mic audio last exceeded the speech RMS threshold
	// (unix nanos; written by the gRPC recv goroutine in PushAudio). It anchors
	// gemini_ms at the true end of speech: Gemini 3.1 delivers input
	// transcriptions (partial AND final) too late to time against.
	lastSpeechNano  atomic.Int64
	lastAnnounceFix atomic.Int64 // unix nanos of the last announce-without-act nudge

	// playbackPCM totals recitation bytes streamed (24 kHz mono PCM16), for the
	// call summary. Written by the playback goroutine.
	playbackPCM atomic.Int64

	// playback state (see playback.go)
	playbackActive atomic.Bool
	playMu         sync.Mutex
	playGen        int
	playCancel     context.CancelFunc
	playDone       chan struct{}
	cur            quran.Ref // last verse targeted, for next/previous/repeat

	// playout estimate: web-service queues outbound audio and paces it at
	// exactly 20 ms per frame, so the browser lags the agent by the length of
	// everything queued. playoutHead estimates when the browser finishes
	// playing all audio sent so far; playback timing decisions (verse start,
	// suppression clear, end-of-recitation notify) key off it, not off when
	// the agent finished *sending*. See playback.go.
	playoutMu   sync.Mutex
	playoutHead time.Time
}

// trackPlayout advances the browser-playout estimate by the duration of n
// bytes of 24 kHz mono PCM16 just sent to the sink.
func (c *call) trackPlayout(n int) {
	d := time.Duration(n/2) * time.Second / 24000
	c.playoutMu.Lock()
	if now := time.Now(); c.playoutHead.Before(now) {
		c.playoutHead = now
	}
	c.playoutHead = c.playoutHead.Add(d)
	c.playoutMu.Unlock()
}

// playoutRemaining returns how much sent audio the browser (estimated) still
// has to play.
func (c *call) playoutRemaining() time.Duration {
	c.playoutMu.Lock()
	defer c.playoutMu.Unlock()
	return time.Until(c.playoutHead)
}

// resetPlayout zeroes the estimate after the web-service queue is flushed
// (barge-in / stop): whatever was queued will never be played.
func (c *call) resetPlayout() {
	c.playoutMu.Lock()
	c.playoutHead = time.Time{}
	c.playoutMu.Unlock()
}

// speechRMSThreshold marks a 20 ms mic frame as speech (int16 RMS units).
// Loose on purpose: it timestamps end-of-speech for latency metrics only.
const speechRMSThreshold = 300

// silentFrame substitutes the mic payload while a recitation plays (one 20 ms
// 16 kHz frame of zeros; sliced to the incoming length). Read-only.
var silentFrame = make([]byte, 640)

// PushAudio forwards one 16 kHz mic frame to Gemini. While a recitation plays
// the frame is replaced with silence at the same cadence: the driver recites
// along and Gemini must not hear it (no transcriptions, no suppressed
// generations, no context pollution) — the web-side keyword spotter and the UI
// buttons are the only controllers then. Constant cadence keeps the Live
// session fed. Outside recitation it also tracks when the driver last audibly
// spoke (cheap RMS), anchoring per-turn latency.
func (c *call) PushAudio(pcm16 []byte) error {
	if c.playbackActive.Load() {
		data := silentFrame
		if len(pcm16) < len(data) {
			data = data[:len(pcm16)]
		} else if len(pcm16) > len(data) {
			data = make([]byte, len(pcm16))
		}
		return c.live.Send(agent.LiveRequest{
			RealtimeInput: &genai.Blob{Data: data, MIMEType: inputAudioMIME},
		})
	}
	var sum int64
	for i := 0; i+1 < len(pcm16); i += 2 {
		v := int64(int16(uint16(pcm16[i]) | uint16(pcm16[i+1])<<8))
		sum += v * v
	}
	if n := len(pcm16) / 2; n > 0 {
		if rms := math.Sqrt(float64(sum / int64(n))); rms > speechRMSThreshold {
			c.lastSpeechNano.Store(time.Now().UnixNano())
		}
	}
	return c.live.Send(agent.LiveRequest{
		RealtimeInput: &genai.Blob{Data: pcm16, MIMEType: inputAudioMIME},
	})
}

// ControlPlayback acts on the in-progress recitation (keyword spotter -> web
// -> RecitationControl frame -> here). Navigation is single-verse and linear
// from the verse currently being heard; at a Quran boundary or with nothing
// playing it is a no-op.
func (c *call) ControlPlayback(action string) error {
	_, sp := c.eng.tracer.Start(c.spanCtx, "agent.recitation_control",
		trace.WithAttributes(attribute.String("action", action)))
	defer sp.End()

	if action == "stop" {
		c.stopPlayback()
		return nil
	}
	if !c.playbackActive.Load() {
		sp.SetAttributes(attribute.String("outcome", "ignored_not_playing"))
		c.log.Info("recitation control ignored; nothing playing", "action", action)
		return nil
	}
	c.playMu.Lock()
	cur := c.cur
	c.playMu.Unlock()
	target, ok := navTarget(cur, action)
	if !ok {
		sp.SetAttributes(attribute.String("outcome", "no_target"))
		c.log.Info("recitation control has no target", "action", action, "cur", cur.String())
		return nil
	}
	sp.SetAttributes(
		attribute.String("from", cur.String()),
		attribute.String("to", target.String()))
	c.log.Info("recitation control", "action", action, "from", cur.String(), "to", target.String())
	c.startPlayback([]quran.Ref{target}, 1)
	return nil
}

// navTarget resolves a control action to the verse to play from cur.
func navTarget(cur quran.Ref, action string) (quran.Ref, bool) {
	if !quran.Valid(cur.Surah, cur.Ayah) {
		return quran.Ref{}, false
	}
	switch action {
	case "again":
		return cur, true
	case "next":
		return quran.Next(cur.Surah, cur.Ayah)
	case "previous":
		return quran.Prev(cur.Surah, cur.Ayah)
	}
	return quran.Ref{}, false
}

func (c *call) Close() error {
	var err error
	c.closeOne.Do(func() {
		c.stopPlayback()
		c.eng.calls.Delete(c.sessionID)
		c.logCallSummary()
		if c.callSpan != nil {
			c.callSpan.End()
		}
		err = c.live.Close()
	})
	<-c.done
	return err
}

// logCallSummary emits one per-call health line + attributes on agent.call:
// turn count, gemini_ms p50/p95, tool calls, recitation seconds streamed, and
// the (engine-global) audio cache hit ratio.
func (c *call) logCallSummary() {
	p50, p95 := percentiles(c.turnLatMs)
	playbackSecs := c.playbackPCM.Load() / 2 / 24000
	hits, misses := c.eng.qc.CacheStats()
	ratio := 0.0
	if hits+misses > 0 {
		ratio = float64(hits) / float64(hits+misses)
	}
	c.log.Info("call summary",
		"turns", len(c.turnLatMs),
		"gemini_ms_p50", p50,
		"gemini_ms_p95", p95,
		"tool_calls", c.toolCalls,
		"playback_secs", playbackSecs,
		"audio_cache_hit_ratio", fmt.Sprintf("%.2f", ratio),
	)
	if c.callSpan != nil {
		c.callSpan.SetAttributes(
			attribute.Int("turns", len(c.turnLatMs)),
			attribute.Int64("gemini_ms_p50", p50),
			attribute.Int64("gemini_ms_p95", p95),
			attribute.Int("tool_calls", c.toolCalls),
			attribute.Int64("playback_secs", playbackSecs),
			attribute.Float64("audio_cache_hit_ratio", ratio),
		)
	}
}

// percentiles returns the p50 and p95 of ms (0 when empty).
func percentiles(ms []int64) (p50, p95 int64) {
	if len(ms) == 0 {
		return 0, 0
	}
	s := append([]int64(nil), ms...)
	slices.Sort(s)
	idx := func(p float64) int64 {
		i := int(p*float64(len(s))) - 1
		if i < 0 {
			i = 0
		}
		return s[i]
	}
	return idx(0.50), idx(0.95)
}

// pump iterates model events and emits frames until the session ends.
func (c *call) pump(events func(func(*session.Event, error) bool)) {
	defer close(c.done)
	for event, err := range events {
		if err != nil {
			c.log.Warn("live event error", "err", err)
			return
		}
		if event == nil {
			continue
		}
		c.emit(event)
	}
}

func (c *call) emit(event *session.Event) {
	// Barge-in: user interrupted; tell web-service to flush its output queue.
	// Suppressed while recitation plays — the driver reciting along must NOT
	// cancel playback; only the stop word (-> stop_recitation) ends it.
	if event.Interrupted && !c.playbackActive.Load() {
		c.callSpan.AddEvent("barge-in")
		_ = c.sink(&voicev1.ServerFrame{Payload: &voicev1.ServerFrame_Interrupt{Interrupt: &voicev1.Interrupt{}}})
		c.resetPlayout() // web flushed its queue; that audio will never play
	}

	// Transcripts (optional; for UI/debug).
	if t := event.OutputTranscription; t != nil && t.Text != "" {
		if !event.Partial {
			c.log.Info("transcript", "who", "agent", "text", t.Text,
				"during_playback", c.playbackActive.Load())
			c.watchAnnouncedPlayback(t.Text)
		}
		_ = c.sink(&voicev1.ServerFrame{Payload: &voicev1.ServerFrame_Transcript{
			Transcript: &voicev1.Transcript{Text: t.Text, IsUser: false, Final: !event.Partial},
		}})
	}
	if t := event.InputTranscription; t != nil && t.Text != "" {
		if !event.Partial {
			c.log.Info("transcript", "who", "user", "text", t.Text,
				"during_playback", c.playbackActive.Load())
		}
		// Turn clock anchor: the most recent input-transcription event before the
		// response's first audio ≈ end of the user's speech. Gemini 3.1 often
		// delivers the FINAL input transcription only at end of generation (after
		// the audio), so anchoring on finals alone misses most turns — partials
		// arrive roughly in real time while the user speaks. Skip while a
		// recitation is playing: the driver reciting along fires transcriptions but
		// the agent stays silent, which would measure the recitation, not a turn.
		if !c.playbackActive.Load() {
			c.lastUserInput = time.Now()
			if !c.awaitFirstAudio {
				c.awaitFirstAudio = true
				c.turnHadTool.Store(false)
				c.turnID++
			}
		}
		_ = c.sink(&voicev1.ServerFrame{Payload: &voicev1.ServerFrame_Transcript{
			Transcript: &voicev1.Transcript{Text: t.Text, IsUser: true, Final: !event.Partial},
		}})
	}

	if event.Content == nil {
		return
	}
	for _, part := range event.Content.Parts {
		switch {
		case part.InlineData != nil && strings.HasPrefix(part.InlineData.MIMEType, "audio/"):
			// Suppress Gemini's own audio while a recitation is playing so the
			// model can't talk over the reciter.
			if c.playbackActive.Load() {
				continue
			}
			// 24 kHz PCM16 from Gemini -> web-service.
			c.recordFirstAudio()
			_ = c.sink(&voicev1.ServerFrame{Payload: &voicev1.ServerFrame_AudioOut{AudioOut: part.InlineData.Data}})
			c.trackPlayout(len(part.InlineData.Data))
		case part.FunctionCall != nil:
			// Tool timing comes from ADK's own `execute_tool` spans; we only note
			// that this turn involved a tool (its reply won't be pure endpointing).
			c.turnHadTool.Store(true)
			c.toolCalls++
			c.callSpan.AddEvent("tool:" + part.FunctionCall.Name)
			c.log.Info("tool call", "name", part.FunctionCall.Name,
				"args", compactJSON(part.FunctionCall.Args, 300))
			_ = c.sink(&voicev1.ServerFrame{Payload: &voicev1.ServerFrame_ToolEvent{
				ToolEvent: &voicev1.ToolEvent{Name: part.FunctionCall.Name, Status: "start"},
			}})
		case part.FunctionResponse != nil:
			c.log.Info("tool result", "name", part.FunctionResponse.Name,
				"result", compactJSON(part.FunctionResponse.Response, 400))
			_ = c.sink(&voicev1.ServerFrame{Payload: &voicev1.ServerFrame_ToolEvent{
				ToolEvent: &voicev1.ToolEvent{Name: part.FunctionResponse.Name, Status: "ok"},
			}})
		}
	}
}

// announceRe matches the agent's own pre-play phrases ("One moment, playing
// Al-Fath…"), which per the prompt MUST be followed by a play tool call.
var announceRe = regexp.MustCompile(`(?i)\b(one moment|playing|getting)\b|لحظة|سأشغل|تشغيل`)

// watchAnnouncedPlayback catches the model announcing playback and then ending
// its turn without any tool call (observed repeatedly with 3.1: "One moment,
// playing X" → silence, driver has to nudge). After a short grace period, if no
// tool ran and nothing is playing, a system note tells it to act.
func (c *call) watchAnnouncedPlayback(text string) {
	txt := strings.TrimSpace(text)
	if c.playbackActive.Load() || c.turnHadTool.Load() ||
		strings.HasSuffix(txt, "?") || !announceRe.MatchString(txt) {
		return
	}
	now := time.Now().UnixNano()
	if last := c.lastAnnounceFix.Load(); now-last < int64(10*time.Second) {
		return
	}
	c.lastAnnounceFix.Store(now)
	go func() {
		time.Sleep(2 * time.Second) // grace: the tool call may be in flight
		if c.turnHadTool.Load() || c.playbackActive.Load() {
			return
		}
		c.log.Info("announce without tool call; nudging model")
		c.callSpan.AddEvent("announce-without-act nudge")
		if err := c.live.Send(agent.LiveRequest{
			Content: genai.NewContentFromText(
				"(system note: you told the driver you would play or fetch something but no tool was called. Call the right tool NOW in this turn, or tell the driver briefly that you could not.)",
				genai.RoleUser),
		}); err != nil {
			c.log.Warn("announce-fix notify failed", "err", err)
		}
	}()
}

// compactJSON renders v as one-line JSON for debug logs, truncated to max.
func compactJSON(v any, max int) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := string(b)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// maxTurnGap discards bogus turn measurements: a gap longer than this means the
// window spanned a recitation or idle silence, not a real conversational turn.
const maxTurnGap = 6 * time.Second

// recordFirstAudio closes the current turn's latency measurement when the first
// agent audio of a response arrives: gemini_ms = first-audio − last user input
// transcription. Turns that involved a tool (had_tool=true) include tool time —
// filter them in Jaeger to isolate pure endpointing+model; ADK's execute_tool
// spans give the tool cost separately.
func (c *call) recordFirstAudio() {
	if !c.awaitFirstAudio || c.lastUserInput.IsZero() {
		return
	}
	c.awaitFirstAudio = false
	// Prefer the RMS end-of-speech timestamp (real); the transcription-event
	// time is a poor fallback (3.1 delivers transcriptions late).
	anchor := c.lastUserInput
	if ns := c.lastSpeechNano.Load(); ns > 0 {
		if t := time.Unix(0, ns); t.Before(time.Now()) {
			anchor = t
		}
	}
	d := time.Since(anchor)
	if d > maxTurnGap || d < 50*time.Millisecond {
		return // spanned a recitation/idle gap, or a timing artifact; not a real turn
	}
	hadTool := c.turnHadTool.Load()
	c.turnLatMs = append(c.turnLatMs, d.Milliseconds())
	c.log.Info("turn latency", "turn", c.turnID, "gemini_ms", d.Milliseconds(), "had_tool", hadTool)
	_, sp := c.eng.tracer.Start(c.spanCtx, "agent.turn",
		trace.WithTimestamp(anchor),
		trace.WithAttributes(
			attribute.Int("turn", c.turnID),
			attribute.Int64("gemini_ms", d.Milliseconds()),
			attribute.Bool("had_tool", hadTool),
			attribute.String("model", c.eng.settings.Model),
			attribute.Int("vad_silence_ms", c.eng.settings.SilenceMs),
		),
	)
	sp.End()
}
