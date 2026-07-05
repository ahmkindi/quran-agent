package adkengine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/agent"
	"google.golang.org/genai"

	voicev1 "github.com/ahmkindi/quran-agent/gen/voice/v1"
	"github.com/ahmkindi/quran-agent/services/agent/internal/quran"
)

const (
	// Outbound audio frame: 24 kHz mono PCM16, 20 ms == 480 samples == 960 bytes.
	// Matches web-service's frame size so it Opus-encodes injected audio as-is.
	outFrameBytes = 960
	frameInterval = 20 * time.Millisecond
	// Safety bounds on tool arguments.
	maxVerses = 500 // consecutive verses per play_next/play_previous
	maxRepeat = 100 // loops of a selection
	// playoutPad covers what the playout estimate can't see (web prime cushion,
	// Opus/network jitter) when deciding the recitation has finished in the
	// browser, before reopening suppression and nudging the model.
	playoutPad = 500 * time.Millisecond
)

// waitPlayout blocks until the browser has (estimated) played everything sent
// so far, plus pad. Returns false if ctx is cancelled first. Without this the
// agent acts on its own send-clock: the model generates speech much faster
// than real time, web-service queues it, and every downstream decision fires
// seconds before the driver actually hears the audio.
func (c *call) waitPlayout(ctx context.Context, pad time.Duration) bool {
	for {
		rem := c.playoutRemaining() + pad // playoutRemaining goes negative past the head
		if rem <= 0 {
			return true
		}
		if rem > 100*time.Millisecond {
			rem = 100 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(rem):
		}
	}
}

// startPlayback plays refs (repeat times; repeat 0 = loop forever until
// stopped), replacing any current playback. It returns immediately; audio
// streams from a paced goroutine so the tool call — and Gemini's short spoken
// confirmation — complete right away.
//
// playbackActive is set synchronously so emit() begins suppressing Gemini audio
// and barge-in before the tool returns. Because the model is prompted to speak
// its confirmation BEFORE calling the tool, that confirmation is already queued
// and is not suppressed; only new Gemini audio during the recitation is.
func (c *call) startPlayback(refs []quran.Ref, repeat int) {
	if len(refs) == 0 {
		return
	}
	if repeat < 0 {
		repeat = 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	c.playMu.Lock()
	c.playGen++
	gen := c.playGen
	oldCancel, oldDone := c.playCancel, c.playDone
	c.playCancel, c.playDone = cancel, done
	c.cur = refs[len(refs)-1]
	c.playbackActive.Store(true)
	c.playMu.Unlock()

	// Stop and flush a recitation already in progress (replace case).
	if oldCancel != nil {
		oldCancel()
		<-oldDone
		c.flush()
		c.resetPlayout()
	}

	c.log.Info("playback start", "from", refs[0].String(), "to", refs[len(refs)-1].String(),
		"verses", len(refs), "repeat", repeat, "gen", gen)

	// Tell web-service recitation is active so it keeps the mic open (the driver
	// may recite along and must still be heard saying the stop word). Barge-in is
	// already suppressed server-side, so an open mic can't cut the recitation.
	c.emitRecitation(true)

	// Warm the head of the selection so multi-verse starts don't stall between
	// early verses; singleflight collapses these with runPlayback's own fetches.
	head := refs
	if len(head) > 3 {
		head = head[:3]
	}
	c.eng.prefetch(head...)

	go c.runPlayback(ctx, gen, refs, repeat, done)

	// Warm the neighbours of the last verse so "next"/"previous" are instant.
	c.eng.warmAround(refs[len(refs)-1])
}

// stopPlayback cancels any recitation and flushes the outbound queue so the
// caller hears silence immediately. This is the stop-word target.
func (c *call) stopPlayback() {
	c.playMu.Lock()
	c.playGen++
	oldCancel, oldDone := c.playCancel, c.playDone
	c.playCancel, c.playDone = nil, nil
	c.playbackActive.Store(false)
	c.playMu.Unlock()

	if oldCancel != nil {
		oldCancel()
		<-oldDone
		c.flush()
		c.resetPlayout()
		c.emitRecitation(false)
	}
}

// runPlayback fetches (cache-backed) and paces each verse's PCM to the sink.
func (c *call) runPlayback(ctx context.Context, gen int, refs []quran.Ref, repeat int, done chan struct{}) {
	// Recitation block on the call's trace timeline. The span context rides the
	// cancellable playback ctx so quran.audio child spans nest under it.
	_, span := c.eng.tracer.Start(c.spanCtx, "agent.playback", trace.WithAttributes(
		attribute.String("from", refs[0].String()),
		attribute.String("to", refs[len(refs)-1].String()),
		attribute.Int("verses", len(refs)),
		attribute.Int("repeat", repeat),
		attribute.Bool("loop", repeat == 0),
		attribute.String("reciter", c.eng.settings.Reciter),
	))
	ctx = trace.ContextWithSpan(ctx, span)
	defer func() {
		reason := "finished"
		if ctx.Err() != nil {
			reason = "stopped" // stop word / stop_recitation / replaced / call end
		}
		span.SetAttributes(attribute.String("reason", reason))
		span.End()
	}()
	defer func() {
		// Clear active only if we're still the current playback (a replace/stop
		// bumped playGen and already took ownership otherwise).
		c.playMu.Lock()
		wasCurrent := c.playGen == gen
		if wasCurrent {
			c.playbackActive.Store(false)
			c.playCancel, c.playDone = nil, nil
		}
		last := c.cur
		c.playMu.Unlock()
		if wasCurrent {
			// Natural end of this recitation: web-service can resume mic gating.
			c.emitRecitation(false)
			// Only a natural finish (not stop/replace, which cancel ctx) nudges
			// the model, AFTER the suppression flag is cleared so its reply audio
			// isn't swallowed. Without this the call goes dead until the driver
			// speaks again.
			if ctx.Err() == nil {
				c.notifyPlaybackDone(last)
			}
		}
		close(done)
	}()

	// Let the browser finish playing the model's spoken confirmation (queued in
	// web-service) before the verse card + audio go out, so what the driver sees
	// and hears stays in sync and the outbound queue stays shallow.
	if !c.waitPlayout(ctx, 0) {
		return
	}

	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()

	for r := 0; repeat == 0 || r < repeat; r++ {
		for i, ref := range refs {
			if ctx.Err() != nil {
				return
			}
			// Track the verse being heard (not the queue tail) so the "again"/
			// "next"/"previous" control words and the play_next/play_previous
			// tools navigate relative to it.
			c.playMu.Lock()
			if c.playGen == gen {
				c.cur = ref
			}
			c.playMu.Unlock()
			// Fetch the display metadata in parallel with the audio; both are
			// cached so repeats and revisits are instant.
			detailCh := make(chan verseDetail, 1)
			go func(ref quran.Ref, i int) {
				detailCh <- c.verseDetail(ctx, ref, i+1, len(refs), r+1, repeat)
			}(ref, i)

			pcm, err := c.eng.qc.AyahPCM(ctx, c.eng.settings.Reciter, ref.Surah, ref.Ayah)
			if err != nil {
				c.log.Warn("recitation fetch/decode failed", "ref", ref.String(), "err", err)
				continue
			}
			c.playbackPCM.Add(int64(len(pcm)))
			// Prefetch the following verse while this one plays, so "next" is instant.
			if nref, ok := quran.Next(ref.Surah, ref.Ayah); ok {
				go func(n quran.Ref) {
					_, _ = c.eng.qc.AyahPCM(context.Background(), c.eng.settings.Reciter, n.Surah, n.Ayah)
				}(nref)
			}
			// Tell the UI which verse is starting, in stream order (before its audio).
			select {
			case d := <-detailCh:
				c.emitVerse(d)
			case <-ctx.Done():
				return
			}
			if !c.streamPCM(ctx, ticker, pcm) {
				return
			}
		}
	}

	// All frames sent, but the browser is still playing the tail (jitter
	// cushion + queue). Hold suppression and the finished-nudge until the
	// driver has actually heard the end, else the reopened mic picks up the
	// still-playing verse from the speakers and the resulting barge-in flush
	// cuts it off.
	c.waitPlayout(ctx, playoutPad)
}

// verseDetail is the JSON payload of a recitation "verse" ToolEvent, rendered
// by the browser's now-playing panel.
type verseDetail struct {
	Surah            int    `json:"surah"`
	Ayah             int    `json:"ayah"`
	Ref              string `json:"ref"` // "2:255"
	SurahNameAr      string `json:"surahNameAr,omitempty"`
	SurahNameEn      string `json:"surahNameEn"`
	SurahTranslation string `json:"surahTranslation,omitempty"`
	RevelationType   string `json:"revelationType,omitempty"`
	Juz              int    `json:"juz,omitempty"`
	AyahCount        int    `json:"ayahCount,omitempty"`
	TextAr           string `json:"textAr,omitempty"`
	TranslationEn    string `json:"translationEn,omitempty"`
	VerseIndex       int    `json:"verseIndex"`
	VerseCount       int    `json:"verseCount"`
	RepeatIndex      int    `json:"repeatIndex"`
	RepeatCount      int    `json:"repeatCount"`
}

// verseDetail gathers display metadata for one verse. Content fetches are
// bounded and non-fatal: on failure the panel still gets the reference and the
// baked surah name.
func (c *call) verseDetail(ctx context.Context, ref quran.Ref, vi, vc, ri, rc int) verseDetail {
	d := verseDetail{
		Surah: ref.Surah, Ayah: ref.Ayah, Ref: ref.String(),
		SurahNameEn: quran.SurahName(ref.Surah), AyahCount: quran.AyahCount(ref.Surah),
		VerseIndex: vi, VerseCount: vc, RepeatIndex: ri, RepeatCount: rc,
	}
	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if info, err := c.eng.qc.AyahInfo(fctx, ref.Surah, ref.Ayah); err == nil {
		d.TextAr = info.TextAr
		d.SurahNameAr = info.SurahNameAr
		d.SurahNameEn = info.SurahNameEn
		d.SurahTranslation = info.SurahTranslation
		d.RevelationType = info.RevelationType
		d.Juz = info.Juz
		d.AyahCount = info.AyahCount
	} else {
		c.log.Warn("ayah info fetch failed", "ref", ref.String(), "err", err)
	}
	if txt, err := c.eng.qc.Translation(fctx, ref.Surah, ref.Ayah, ""); err == nil {
		d.TranslationEn = txt
	}
	return d
}

// emitVerse announces the verse now being recited (ToolEvent "recitation" /
// "verse"). The web-service must treat only "start"/"stop" as spotter gates.
func (c *call) emitVerse(d verseDetail) {
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	_ = c.sink(&voicev1.ServerFrame{Payload: &voicev1.ServerFrame_ToolEvent{
		ToolEvent: &voicev1.ToolEvent{Name: "recitation", Status: "verse", Detail: string(b)},
	}})
}

// streamPCM sends pcm in 20 ms frames, one per tick. Returns false if cancelled.
func (c *call) streamPCM(ctx context.Context, ticker *time.Ticker, pcm []byte) bool {
	for off := 0; off < len(pcm); off += outFrameBytes {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		end := off + outFrameBytes
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := c.sink(&voicev1.ServerFrame{Payload: &voicev1.ServerFrame_AudioOut{AudioOut: pcm[off:end]}}); err != nil {
			return false
		}
		c.trackPlayout(end - off)
	}
	return true
}

// notifyPlaybackDone tells the model a recitation finished naturally so the
// conversation continues without the driver having to speak first. Sent as a
// text turn; the AFTER A RECITATION ENDS prompt rule decides what happens
// (auto-continue with play_next, or one short offer).
func (c *call) notifyPlaybackDone(last quran.Ref) {
	c.callSpan.AddEvent("playback-finished nudge")
	c.log.Info("playback finished; nudging model", "last", last.String())
	msg := fmt.Sprintf(
		"(system note: the recitation of %s %s has just finished playing. Follow the AFTER A RECITATION ENDS rule. "+
			"If the driver asked to stop, or did not explicitly ask for continuous playback, do not start more playback.)",
		quran.SurahName(last.Surah), last.String())
	if err := c.live.Send(agent.LiveRequest{
		Content: genai.NewContentFromText(msg, genai.RoleUser),
	}); err != nil {
		c.log.Warn("playback-done notify failed", "err", err)
	}
}

// flush tells web-service to drop everything queued (barge-in path).
func (c *call) flush() {
	_ = c.sink(&voicev1.ServerFrame{Payload: &voicev1.ServerFrame_Interrupt{Interrupt: &voicev1.Interrupt{}}})
}

// emitRecitation signals recitation start/stop to web-service over the existing
// ToolEvent channel (name "recitation"). Web keeps the mic open while active.
func (c *call) emitRecitation(active bool) {
	status := "stop"
	if active {
		status = "start"
	}
	_ = c.sink(&voicev1.ServerFrame{Payload: &voicev1.ServerFrame_ToolEvent{
		ToolEvent: &voicev1.ToolEvent{Name: "recitation", Status: status},
	}})
}
