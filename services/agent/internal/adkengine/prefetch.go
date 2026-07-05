package adkengine

import (
	"context"

	"github.com/ahmkindi/quran-agent/services/agent/internal/quran"
)

// prefetch warms the decoded-PCM cache for the given verses in the background,
// so the likely-next play is instant. Fire-and-forget with a bounded semaphore
// (prefetchSem) and background ctx — a warm is never cancelled by barge-in/stop,
// and the shared client's singleflight collapses any duplicate with a live play.
func (e *Engine) prefetch(refs ...quran.Ref) {
	for _, ref := range refs {
		if !quran.Valid(ref.Surah, ref.Ayah) {
			continue
		}
		ref := ref
		go func() {
			// Acquire a slot inside the goroutine so the caller never blocks and
			// no ref is dropped; at most len(prefetchSem) decode concurrently.
			e.prefetchSem <- struct{}{}
			defer func() { <-e.prefetchSem }()
			if _, err := e.qc.AyahPCM(context.Background(), e.settings.Reciter, ref.Surah, ref.Ayah); err != nil {
				e.log.Debug("prefetch failed", "ref", ref.String(), "err", err)
			}
		}()
	}
}

// warmAround prefetches the neighbours of ref so "next" and "previous" are
// instant after any play.
func (e *Engine) warmAround(ref quran.Ref) {
	var refs []quran.Ref
	if n, ok := quran.Next(ref.Surah, ref.Ayah); ok {
		refs = append(refs, n)
	}
	if p, ok := quran.Prev(ref.Surah, ref.Ayah); ok {
		refs = append(refs, p)
	}
	e.prefetch(refs...)
}

// warmupVerses is a small curated set decoded at startup so the first demo play
// is instant. Kept short (tiny PCM) to stay well under the cache budget.
var warmupVerses = []quran.Ref{
	{Surah: 1, Ayah: 1}, {Surah: 1, Ayah: 2}, {Surah: 1, Ayah: 3}, {Surah: 1, Ayah: 4},
	{Surah: 1, Ayah: 5}, {Surah: 1, Ayah: 6}, {Surah: 1, Ayah: 7}, // Al-Fatihah
	{Surah: 2, Ayah: 255},                                               // Ayat al-Kursi
	{Surah: 108, Ayah: 1}, {Surah: 108, Ayah: 2}, {Surah: 108, Ayah: 3}, // Al-Kawthar
	{Surah: 112, Ayah: 1}, {Surah: 113, Ayah: 1}, {Surah: 114, Ayah: 1}, // Ikhlas/Falaq/Nas openers
}

func (e *Engine) warmup() { e.prefetch(warmupVerses...) }
