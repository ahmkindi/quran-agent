package quran

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// indexedAyah is one verse prepared for fuzzy matching: normalized whitespace
// text plus its unique token set.
type indexedAyah struct {
	surah  int
	ayah   int
	text   string
	tokens map[string]struct{}
}

// fuzzySearchArabic finds the verses whose text best overlaps the (possibly
// partial or mis-recited) query, by token overlap over the full undiacritized
// Quran. This tolerates wrong/missing words far better than substring search:
// a long ayah read with a few mistakes still shares most of its tokens with the
// true verse, so it ranks first — no need to re-search halves manually.
func (c *Client) fuzzySearchArabic(ctx context.Context, query string, limit int) ([]Match, error) {
	if err := c.loadCorpus(ctx); err != nil {
		return nil, err
	}
	qTokens := tokenizeArabic(query)
	if len(qTokens) == 0 {
		return nil, fmt.Errorf("empty search query")
	}
	qSet := make(map[string]struct{}, len(qTokens))
	for _, t := range qTokens {
		qSet[t] = struct{}{}
	}

	if limit <= 0 {
		limit = 5
	}

	type scored struct {
		m    Match
		cov  float64
		hits int
	}
	var results []scored
	for i := range c.corpus {
		a := &c.corpus[i]
		hits := 0
		for t := range qSet {
			if _, ok := a.tokens[t]; ok {
				hits++
			}
		}
		if hits == 0 {
			continue
		}
		cov := float64(hits) / float64(len(qSet)) // fraction of the query found in this ayah
		// Require strong coverage; short queries must match almost fully to avoid
		// matching on a single common word like "الله".
		minHits := 2
		if len(qSet) <= 2 {
			minHits = len(qSet)
		}
		if hits < minHits || cov < 0.5 {
			continue
		}
		results = append(results, scored{
			m: Match{
				Surah: a.surah, Ayah: a.ayah, SurahName: SurahName(a.surah),
				Text: a.text, Score: round2(cov),
			},
			cov:  cov,
			hits: hits,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].cov != results[j].cov {
			return results[i].cov > results[j].cov
		}
		if results[i].hits != results[j].hits {
			return results[i].hits > results[j].hits
		}
		// Prefer the earlier verse for a stable order.
		if results[i].m.Surah != results[j].m.Surah {
			return results[i].m.Surah < results[j].m.Surah
		}
		return results[i].m.Ayah < results[j].m.Ayah
	})

	out := make([]Match, 0, limit)
	for i := 0; i < len(results) && i < limit; i++ {
		out = append(out, results[i].m)
	}
	return out, nil
}

// loadCorpus lazily fetches the full undiacritized Quran once and indexes it.
func (c *Client) loadCorpus(ctx context.Context) error {
	c.corpusOnce.Do(func() {
		u := fmt.Sprintf("%s/quran/%s", c.apiBase, searchEditionAr)
		body, err := c.getBytes(ctx, u)
		if err != nil {
			c.corpusErr = fmt.Errorf("load quran corpus: %w", err)
			return
		}
		var resp struct {
			Data struct {
				Surahs []struct {
					Number int `json:"number"`
					Ayahs  []struct {
						NumberInSurah int    `json:"numberInSurah"`
						Text          string `json:"text"`
					} `json:"ayahs"`
				} `json:"surahs"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			c.corpusErr = fmt.Errorf("parse quran corpus: %w", err)
			return
		}
		var idx []indexedAyah
		for _, s := range resp.Data.Surahs {
			for _, a := range s.Ayahs {
				toks := tokenizeArabic(a.Text)
				set := make(map[string]struct{}, len(toks))
				for _, t := range toks {
					set[t] = struct{}{}
				}
				idx = append(idx, indexedAyah{
					surah: s.Number, ayah: a.NumberInSurah,
					text: strings.TrimSpace(a.Text), tokens: set,
				})
			}
		}
		if len(idx) == 0 {
			c.corpusErr = fmt.Errorf("empty quran corpus")
			return
		}
		c.corpus = idx
	})
	return c.corpusErr
}

// tokenizeArabic normalizes and splits Arabic text into comparable tokens:
// strips diacritics/tatweel, folds alef/ya/ta-marbuta/hamza variants, drops
// punctuation, and splits on whitespace.
func tokenizeArabic(s string) []string {
	s = stripArabicDiacritics(s)
	var b strings.Builder
	for _, r := range s {
		switch r {
		case 'أ', 'إ', 'آ', 'ٱ':
			b.WriteRune('ا')
		case 'ى':
			b.WriteRune('ي')
		case 'ؤ':
			b.WriteRune('و')
		case 'ئ':
			b.WriteRune('ي')
		case 'ة':
			b.WriteRune('ه')
		case 'ء':
			// drop bare hamza
		default:
			if isArabicLetter(r) || r == ' ' {
				b.WriteRune(r)
			} else if r == '\t' || r == '\n' || r == '\r' {
				b.WriteRune(' ')
			}
			// all other punctuation/symbols dropped
		}
	}
	return strings.Fields(b.String())
}

func isArabicLetter(r rune) bool {
	return r >= 0x0621 && r <= 0x064A
}

// containsArabic reports whether s has any Arabic letter (used to route search
// to the fuzzy Arabic path vs the English translation path).
func containsArabic(s string) bool {
	for _, r := range s {
		if isArabicLetter(r) {
			return true
		}
	}
	return false
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
