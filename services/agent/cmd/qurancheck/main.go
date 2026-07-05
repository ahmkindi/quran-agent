// Command qurancheck verifies the recitation audio path WITHOUT Gemini or a
// browser: it fetches a verse's MP3, decodes+resamples it to 24 kHz mono PCM
// (the same path the play_* tools use), and writes a WAV you can listen to.
//
// Usage:
//
//	go run ./services/agent/cmd/qurancheck -ref 2:255 -reciter ar.husary -out ayah.wav
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ahmkindi/quran-agent/services/agent/internal/quran"
)

func main() {
	ref := flag.String("ref", "2:255", "verse reference surah:ayah")
	reciter := flag.String("reciter", "ar.mahermuaiqly", "recitation edition")
	bitrate := flag.Int("bitrate", 128, "audio bitrate (128 or 64)")
	out := flag.String("out", "ayah.wav", "output WAV path (24 kHz mono s16le)")
	audio := flag.Bool("audio", true, "fetch+decode the recitation to a WAV")
	lookup := flag.Bool("lookup", false, "also fetch translation + tafsir for -ref")
	search := flag.String("search", "", "search for verses containing this fragment")
	lang := flag.String("lang", "ar", "search language: ar or en")
	flag.Parse()

	surah, ayah, err := parseRef(*ref)
	if err != nil {
		fail(err)
	}
	qc := quran.New(quran.Options{Reciter: *reciter, Bitrate: *bitrate})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if *search != "" {
		if err := runSearch(ctx, qc, *search, *lang); err != nil {
			fail(err)
		}
	}
	if *lookup {
		if err := runLookup(ctx, qc, surah, ayah); err != nil {
			fail(err)
		}
	}
	if *audio {
		if err := run(*ref, *reciter, *bitrate, *out); err != nil {
			fail(err)
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "qurancheck:", err)
	os.Exit(1)
}

func runSearch(ctx context.Context, qc *quran.Client, query, lang string) error {
	matches, err := qc.Search(ctx, query, lang, 5)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	fmt.Printf("search %q (%s): %d match(es)\n", query, lang, len(matches))
	for _, m := range matches {
		fmt.Printf("  %s %d:%d — %s\n", m.SurahName, m.Surah, m.Ayah, snippet(m.Text, 80))
	}
	return nil
}

func runLookup(ctx context.Context, qc *quran.Client, surah, ayah int) error {
	tr, err := qc.Translation(ctx, surah, ayah, "")
	if err != nil {
		return fmt.Errorf("translation: %w", err)
	}
	fmt.Printf("translation %d:%d — %s\n", surah, ayah, snippet(tr, 200))
	tf, err := qc.Tafsir(ctx, surah, ayah, "")
	if err != nil {
		return fmt.Errorf("tafsir: %w", err)
	}
	fmt.Printf("tafsir %d:%d — %s\n", surah, ayah, snippet(tf, 200))
	return nil
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func run(ref, reciter string, bitrate int, out string) error {
	surah, ayah, err := parseRef(ref)
	if err != nil {
		return err
	}
	if !quran.Valid(surah, ayah) {
		return fmt.Errorf("no such verse %d:%d", surah, ayah)
	}

	qc := quran.New(quran.Options{Reciter: reciter, Bitrate: bitrate})
	fmt.Printf("fetching %s %s from %s\n", ref, quran.SurahName(surah), qc.AudioURL(reciter, surah, ayah))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pcm, err := qc.AyahPCM(ctx, reciter, surah, ayah)
	if err != nil {
		return err
	}
	if err := writeWAV(out, pcm, 24000); err != nil {
		return err
	}
	fmt.Printf("ok: %s %d:%d -> %s (%.1fs @24kHz, %d bytes)\n",
		quran.SurahName(surah), surah, ayah, out, float64(len(pcm))/2/24000, len(pcm))
	return nil
}

func parseRef(s string) (int, int, error) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad ref %q, want surah:ayah", s)
	}
	surah, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	ayah, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("bad ref %q, want surah:ayah", s)
	}
	return surah, ayah, nil
}

func writeWAV(path string, pcm []byte, sampleRate int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	const bitsPerSample, channels = 16, 1
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataLen := len(pcm)

	w := func(v any) { _ = binary.Write(f, binary.LittleEndian, v) }
	f.WriteString("RIFF")
	w(uint32(36 + dataLen))
	f.WriteString("WAVE")
	f.WriteString("fmt ")
	w(uint32(16))
	w(uint16(1)) // PCM
	w(uint16(channels))
	w(uint32(sampleRate))
	w(uint32(byteRate))
	w(uint16(blockAlign))
	w(uint16(bitsPerSample))
	f.WriteString("data")
	w(uint32(dataLen))
	_, err = f.Write(pcm)
	return err
}
