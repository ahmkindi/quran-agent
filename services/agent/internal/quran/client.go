package quran

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"

	"github.com/ahmkindi/quran-agent/pkg/telemetry"
)

// Default API/CDN endpoints and content editions. All are public and require no
// auth. Overridable via Options for testing or swapping providers.
const (
	defaultAudioBase   = "https://cdn.islamic.network/quran/audio"
	defaultAPIBase     = "https://api.alquran.cloud/v1"
	defaultTafsirBase  = "https://raw.githubusercontent.com/spa5k/tafsir_api/main/tafsir"
	defaultReciter     = "ar.mahermuaiqly"
	defaultTranslation = "en.sahih"
	defaultTafsir      = "en-tafisr-ibn-kathir"
	defaultBitrate     = 128

	// searchEditionAr is fully undiacritized Arabic (no harakat) — matches a
	// spoken fragment transcribed without diacritics. searchEditionEn searches
	// the translation.
	searchEditionAr = "quran-simple-clean"
	searchEditionEn = "en.sahih"
)

// Options configure a Client. Zero values fall back to the defaults above.
type Options struct {
	HTTP        *http.Client
	AudioBase   string
	APIBase     string
	TafsirBase  string
	Reciter     string
	Translation string
	Tafsir      string
	Bitrate     int
	CacheBytes  int          // max decoded-PCM bytes kept in the cache (default 32 MiB)
	Log         *slog.Logger // cache hit/miss debug logs (default slog.Default())
}

// Client talks to the Quran content APIs and caches decoded recitation PCM.
type Client struct {
	http                           *http.Client
	audioBase, apiBase, tafsirBase string
	reciter, translation, tafsir   string
	bitrate                        int

	log *slog.Logger

	mu       sync.Mutex
	cache    map[string][]byte  // reciter:surah:ayah -> 24 kHz mono PCM16 LE
	order    []string           // insertion order for LRU eviction
	curBytes int                // total bytes currently cached
	maxBytes int                // byte budget; evict oldest until under it
	sf       singleflight.Group // collapses concurrent identical fetch+decode

	// Audio-cache counters for the per-call summary / demo.
	hits, misses int64

	// Text caches: small strings, keyed edition|surah:ayah, never evicted
	// (bounded by what the call actually asks for).
	textMu   sync.Mutex
	trCache  map[string]string
	tafCache map[string]string

	// Lazily-loaded full undiacritized Quran for local fuzzy Arabic search
	// (robust to a fragment or a mis-recited ayah). See fulltext.go.
	corpusOnce sync.Once
	corpus     []indexedAyah
	corpusErr  error

	infoMu    sync.Mutex
	infoCache map[Ref]AyahInfo // display metadata per verse (small; never evicted)
}

// New builds a Client with sane defaults.
func New(o Options) *Client {
	pick := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	c := &Client{
		http:        o.HTTP,
		audioBase:   pick(o.AudioBase, defaultAudioBase),
		apiBase:     pick(o.APIBase, defaultAPIBase),
		tafsirBase:  pick(o.TafsirBase, defaultTafsirBase),
		reciter:     pick(o.Reciter, defaultReciter),
		translation: pick(o.Translation, defaultTranslation),
		tafsir:      pick(o.Tafsir, defaultTafsir),
		bitrate:     o.Bitrate,
		cache:       make(map[string][]byte),
		maxBytes:    o.CacheBytes,
		infoCache:   make(map[Ref]AyahInfo),
		trCache:     make(map[string]string),
		tafCache:    make(map[string]string),
		log:         o.Log,
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.bitrate == 0 {
		c.bitrate = defaultBitrate
	}
	if c.maxBytes <= 0 {
		c.maxBytes = 32 << 20 // 32 MiB
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 15 * time.Second}
	}
	return c
}

// Reciter returns the configured default reciter edition (e.g. "ar.husary").
func (c *Client) Reciter() string { return c.reciter }

// CacheStats returns the audio cache hit/miss counters (for call summaries).
func (c *Client) CacheStats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// startSpan opens a child span only when ctx already carries a recorded trace
// (a tool call or a playback run). Background prefetch/warmup would otherwise
// litter Jaeger with single-span root traces.
func startSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if !trace.SpanFromContext(ctx).SpanContext().IsValid() {
		return ctx, trace.SpanFromContext(ctx) // no-op span
	}
	return telemetry.Tracer("quran").Start(ctx, name, trace.WithAttributes(attrs...))
}

// Match is one verse returned by Search.
type Match struct {
	Surah     int     `json:"surah"`
	Ayah      int     `json:"ayah"`
	SurahName string  `json:"surah_name"`
	Text      string  `json:"text"`
	Score     float64 `json:"score,omitempty"` // 0..1 fuzzy match confidence (Arabic search)
}

// --- audio ---

// AudioURL builds the direct MP3 URL for a verse without any API round-trip.
func (c *Client) AudioURL(reciter string, surah, ayah int) string {
	if reciter == "" {
		reciter = c.reciter
	}
	return fmt.Sprintf("%s/%d/%s/%d.mp3", c.audioBase, c.bitrate, reciter, GlobalNumber(surah, ayah))
}

// AyahPCM returns the recitation of a verse as 24 kHz mono PCM16 LE, ready to
// stream on the outbound audio path. Results are cached, so repeats and
// prefetch are cheap.
func (c *Client) AyahPCM(ctx context.Context, reciter string, surah, ayah int) ([]byte, error) {
	if reciter == "" {
		reciter = c.reciter
	}
	if !Valid(surah, ayah) {
		return nil, fmt.Errorf("invalid verse %d:%d", surah, ayah)
	}
	key := fmt.Sprintf("%s:%d:%d", reciter, surah, ayah)

	ctx, span := startSpan(ctx, "quran.audio",
		attribute.String("ref", fmt.Sprintf("%d:%d", surah, ayah)),
		attribute.String("reciter", reciter))
	defer span.End()

	c.mu.Lock()
	if pcm, ok := c.cache[key]; ok {
		c.hits++
		c.mu.Unlock()
		span.SetAttributes(attribute.Bool("cache_hit", true))
		c.log.Debug("audio cache", "key", key, "hit", true)
		return pcm, nil
	}
	c.misses++
	c.mu.Unlock()
	span.SetAttributes(attribute.Bool("cache_hit", false))
	c.log.Debug("audio cache", "key", key, "hit", false)

	// Collapse concurrent identical loads (e.g. a prefetch racing the play, or
	// many users hitting the same verse cold) into one fetch+decode. DoChan +
	// select so a cancelled CALLER unblocks immediately (a stop word must never
	// wait out a hanging CDN fetch — that wedged a whole call once); the shared
	// fetch itself runs on its own context, bounded by the HTTP client timeout,
	// and still completes for other waiters / the cache.
	ch := c.sf.DoChan(key, func() (any, error) {
		c.mu.Lock()
		if pcm, ok := c.cache[key]; ok { // filled while we waited
			c.mu.Unlock()
			return pcm, nil
		}
		c.mu.Unlock()

		t0 := time.Now()
		mp3, err := c.getBytes(context.Background(), c.AudioURL(reciter, surah, ayah))
		if err != nil {
			return nil, fmt.Errorf("fetch audio %d:%d: %w", surah, ayah, err)
		}
		downloadMs := time.Since(t0).Milliseconds()
		t1 := time.Now()
		pcm, err := decodeMP3ToPCM24kMono(mp3)
		if err != nil {
			return nil, fmt.Errorf("decode audio %d:%d: %w", surah, ayah, err)
		}
		span.SetAttributes(
			attribute.Int64("download_ms", downloadMs),
			attribute.Int64("decode_ms", time.Since(t1).Milliseconds()),
			attribute.Int("pcm_bytes", len(pcm)))
		c.mu.Lock()
		c.putLocked(key, pcm)
		c.mu.Unlock()
		return pcm, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.([]byte), nil
	}
}

// putLocked inserts pcm and evicts oldest entries until the byte budget holds.
// PCM sizes vary hugely per verse, so a byte budget (not a count) bounds RAM.
func (c *Client) putLocked(key string, pcm []byte) {
	if _, ok := c.cache[key]; ok {
		return
	}
	sz := len(pcm)
	for c.curBytes+sz > c.maxBytes && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if b, ok := c.cache[oldest]; ok {
			c.curBytes -= len(b)
			delete(c.cache, oldest)
		}
	}
	c.cache[key] = pcm
	c.order = append(c.order, key)
	c.curBytes += sz
}

// --- ayah display info ---

// AyahInfo is display metadata for one verse, sourced from the quran-uthmani
// edition (which carries the surah header fields alongside the Arabic text).
type AyahInfo struct {
	Surah            int    `json:"surah"`
	Ayah             int    `json:"ayah"`
	TextAr           string `json:"textAr"`           // Uthmani script with harakat
	SurahNameAr      string `json:"surahNameAr"`      // e.g. "سُورَةُ البَقَرَةِ"
	SurahNameEn      string `json:"surahNameEn"`      // e.g. "Al-Baqara"
	SurahTranslation string `json:"surahTranslation"` // e.g. "The Cow"
	RevelationType   string `json:"revelationType"`   // "Meccan" | "Medinan"
	Juz              int    `json:"juz"`
	AyahCount        int    `json:"ayahCount"` // ayat in the surah
}

// AyahInfo fetches (and caches) the verse text + surah metadata used by the UI's
// now-playing panel. One API call returns everything.
func (c *Client) AyahInfo(ctx context.Context, surah, ayah int) (AyahInfo, error) {
	if !Valid(surah, ayah) {
		return AyahInfo{}, fmt.Errorf("invalid verse %d:%d", surah, ayah)
	}
	ref := Ref{Surah: surah, Ayah: ayah}

	c.infoMu.Lock()
	if info, ok := c.infoCache[ref]; ok {
		c.infoMu.Unlock()
		return info, nil
	}
	c.infoMu.Unlock()

	u := fmt.Sprintf("%s/ayah/%d:%d/quran-uthmani", c.apiBase, surah, ayah)
	code, data, err := c.getAPI(ctx, u)
	if err != nil {
		return AyahInfo{}, err
	}
	if code != 200 {
		return AyahInfo{}, fmt.Errorf("no ayah info for %d:%d", surah, ayah)
	}
	var d struct {
		Text  string `json:"text"`
		Juz   int    `json:"juz"`
		Surah struct {
			Name                   string `json:"name"`
			EnglishName            string `json:"englishName"`
			EnglishNameTranslation string `json:"englishNameTranslation"`
			RevelationType         string `json:"revelationType"`
			NumberOfAyahs          int    `json:"numberOfAyahs"`
		} `json:"surah"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Text == "" {
		return AyahInfo{}, fmt.Errorf("no ayah info for %d:%d", surah, ayah)
	}
	info := AyahInfo{
		Surah:            surah,
		Ayah:             ayah,
		TextAr:           d.Text,
		SurahNameAr:      d.Surah.Name,
		SurahNameEn:      d.Surah.EnglishName,
		SurahTranslation: d.Surah.EnglishNameTranslation,
		RevelationType:   d.Surah.RevelationType,
		Juz:              d.Juz,
		AyahCount:        d.Surah.NumberOfAyahs,
	}
	c.infoMu.Lock()
	c.infoCache[ref] = info
	c.infoMu.Unlock()
	return info, nil
}

// --- translation ---

// Translation returns the translation text of a verse. edition="" uses the
// configured default (Saheeh International).
func (c *Client) Translation(ctx context.Context, surah, ayah int, edition string) (string, error) {
	if edition == "" {
		edition = c.translation
	}
	key := fmt.Sprintf("%s|%d:%d", edition, surah, ayah)

	ctx, span := startSpan(ctx, "quran.translation",
		attribute.String("ref", fmt.Sprintf("%d:%d", surah, ayah)),
		attribute.String("edition", edition))
	defer span.End()

	c.textMu.Lock()
	if txt, ok := c.trCache[key]; ok {
		c.textMu.Unlock()
		span.SetAttributes(attribute.Bool("cache_hit", true))
		c.log.Debug("translation cache", "key", key, "hit", true)
		return txt, nil
	}
	c.textMu.Unlock()
	span.SetAttributes(attribute.Bool("cache_hit", false))
	c.log.Debug("translation cache", "key", key, "hit", false)

	u := fmt.Sprintf("%s/ayah/%d:%d/%s", c.apiBase, surah, ayah, edition)
	code, data, err := c.getAPI(ctx, u)
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("no translation for %d:%d (%s)", surah, ayah, edition)
	}
	var d struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Text == "" {
		return "", fmt.Errorf("no translation for %d:%d (%s)", surah, ayah, edition)
	}
	c.textMu.Lock()
	c.trCache[key] = d.Text
	c.textMu.Unlock()
	return d.Text, nil
}

// --- tafsir ---

// Tafsir returns the tafsir (exegesis) text of a verse. edition="" uses the
// configured default (Ibn Kathir). The agent reads this text; it does not
// generate its own.
func (c *Client) Tafsir(ctx context.Context, surah, ayah int, edition string) (string, error) {
	if edition == "" {
		edition = c.tafsir
	}
	key := fmt.Sprintf("%s|%d:%d", edition, surah, ayah)

	ctx, span := startSpan(ctx, "quran.tafsir",
		attribute.String("ref", fmt.Sprintf("%d:%d", surah, ayah)),
		attribute.String("edition", edition))
	defer span.End()

	c.textMu.Lock()
	if txt, ok := c.tafCache[key]; ok {
		c.textMu.Unlock()
		span.SetAttributes(attribute.Bool("cache_hit", true))
		c.log.Debug("tafsir cache", "key", key, "hit", true)
		return txt, nil
	}
	c.textMu.Unlock()
	span.SetAttributes(attribute.Bool("cache_hit", false))
	c.log.Debug("tafsir cache", "key", key, "hit", false)

	u := fmt.Sprintf("%s/%s/%d/%d.json", c.tafsirBase, edition, surah, ayah)
	body, err := c.getBytes(ctx, u)
	if err != nil {
		return "", err
	}
	var resp struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || strings.TrimSpace(resp.Text) == "" {
		return "", fmt.Errorf("no tafsir for %d:%d (%s)", surah, ayah, edition)
	}
	c.textMu.Lock()
	c.tafCache[key] = resp.Text
	c.textMu.Unlock()
	return resp.Text, nil
}

// --- search ---

// Search finds verses matching the given fragment. It routes by SCRIPT, not the
// lang hint: if the query contains Arabic letters it runs a local fuzzy
// token-overlap search over the undiacritized Quran (tolerant of a partial or
// mis-recited ayah); otherwise it does an English substring search over the
// translation. (Routing by script avoids the common failure where an English
// word like "milk" is sent with lang="ar" and the Arabic tokenizer yields
// nothing.) Returns at most limit matches, best first.
func (c *Client) Search(ctx context.Context, query, lang string, limit int) (matches []Match, err error) {
	ctx, span := startSpan(ctx, "quran.search", attribute.String("lang_hint", lang))
	defer func() {
		span.SetAttributes(attribute.Int("count", len(matches)))
		span.End()
	}()

	if containsArabic(query) {
		span.SetAttributes(attribute.String("route", "ar_local_fuzzy"))
		return c.fuzzySearchArabic(ctx, query, limit)
	}
	span.SetAttributes(attribute.String("route", "en_api"))

	edition := searchEditionEn
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("empty search query")
	}
	u := fmt.Sprintf("%s/search/%s/all/%s", c.apiBase, url.PathEscape(q), edition)

	code, data, err := c.getAPI(ctx, u)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, nil // no matches: alquran.cloud returns 404 + a string body
	}
	var d struct {
		Count   int `json:"count"`
		Matches []struct {
			NumberInSurah int    `json:"numberInSurah"`
			Text          string `json:"text"`
			Surah         struct {
				Number int `json:"number"`
			} `json:"surah"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, nil // data was a string ("no matches")
	}
	out := make([]Match, 0, len(d.Matches))
	for _, m := range d.Matches {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, Match{
			Surah:     m.Surah.Number,
			Ayah:      m.NumberInSurah,
			SurahName: SurahName(m.Surah.Number),
			Text:      m.Text,
		})
	}
	return out, nil
}

// --- http helpers ---

func (c *Client) getBytes(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", u, res.StatusCode)
	}
	return io.ReadAll(res.Body)
}

// getAPI fetches an alquran.cloud endpoint and returns its envelope "code" and
// raw "data". On a miss the API replies 404 with data as a string, so data is
// left as RawMessage for the caller to unmarshal only when code == 200.
func (c *Client) getAPI(ctx context.Context, u string) (int, json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, nil, err
	}
	var top struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return res.StatusCode, nil, nil // non-JSON body; fall back to HTTP status
	}
	if top.Code == 0 {
		top.Code = res.StatusCode
	}
	return top.Code, top.Data, nil
}

// stripArabicDiacritics removes harakat, tatweel and superscript alef so a
// transcribed spoken fragment matches the undiacritized quran-simple edition.
func stripArabicDiacritics(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 0x064B && r <= 0x0652: // fathatan..sukun
			continue
		case r == 0x0670: // superscript alef
			continue
		case r == 0x0640: // tatweel
			continue
		case r == 0x0653 || r == 0x0654 || r == 0x0655: // maddah/hamza above/below
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
