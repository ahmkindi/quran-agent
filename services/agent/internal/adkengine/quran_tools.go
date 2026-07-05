package adkengine

import (
	"fmt"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/ahmkindi/quran-agent/services/agent/internal/quran"
)

// --- tool argument / result types ---

type playAyahArgs struct {
	Surah  int  `json:"surah" jsonschema:"Surah (chapter) number, 1-114"`
	Ayah   int  `json:"ayah" jsonschema:"Ayah (verse) number within the surah, 1-based"`
	Repeat int  `json:"repeat,omitempty" jsonschema:"How many times to repeat the verse. Default 1. Ignored when loop is true"`
	Loop   bool `json:"loop,omitempty" jsonschema:"true = repeat the verse forever until the driver says STOP. Use when they say 'on repeat', 'keep playing it' or 'until I say stop'"`
}

type playRelativeArgs struct {
	Count  int  `json:"count,omitempty" jsonschema:"How many consecutive verses to play from the current position. Default 1"`
	Repeat int  `json:"repeat,omitempty" jsonschema:"How many times to repeat the whole selection. Default 1. Ignored when loop is true"`
	Loop   bool `json:"loop,omitempty" jsonschema:"true = repeat the selection forever until the driver says STOP. Use when they say 'on repeat', 'keep playing it' or 'until I say stop'"`
}

type playSurahArgs struct {
	Surah  int  `json:"surah" jsonschema:"Surah (chapter) number, 1-114"`
	Repeat int  `json:"repeat,omitempty" jsonschema:"How many times to play the whole surah. Default 1. Ignored when loop is true"`
	Loop   bool `json:"loop,omitempty" jsonschema:"true = repeat the surah forever until the driver says STOP. Use when they say 'on repeat', 'keep playing it' or 'until I say stop'"`
}

type playPageArgs struct {
	Page   int  `json:"page" jsonschema:"Mushaf (standard Madani) page number, 1-604"`
	Repeat int  `json:"repeat,omitempty" jsonschema:"How many times to play the whole page. Default 1. Ignored when loop is true"`
	Loop   bool `json:"loop,omitempty" jsonschema:"true = repeat the page forever until the driver says STOP. Use when they say 'on repeat', 'keep playing it' or 'until I say stop'"`
}

type playRangeArgs struct {
	Surah    int  `json:"surah" jsonschema:"Starting surah number, 1-114"`
	Ayah     int  `json:"ayah" jsonschema:"Starting ayah number within the starting surah"`
	EndSurah int  `json:"end_surah" jsonschema:"Ending surah number (same as surah when the range stays within one surah)"`
	EndAyah  int  `json:"end_ayah" jsonschema:"Ending ayah number, inclusive"`
	Repeat   int  `json:"repeat,omitempty" jsonschema:"How many times to play the whole range. Default 1. Ignored when loop is true"`
	Loop     bool `json:"loop,omitempty" jsonschema:"true = repeat the range forever until the driver says STOP. Use when they say 'on repeat', 'keep playing it' or 'until I say stop'"`
}

type playResult struct {
	Playing   bool   `json:"playing"`
	Reference string `json:"reference"`
	Surah     int    `json:"surah"`
	Ayah      int    `json:"ayah"`
	EndSurah  int    `json:"end_surah,omitempty"`
	EndAyah   int    `json:"end_ayah,omitempty"`
	Verses    int    `json:"verses"`
	Repeat    int    `json:"repeat"`
	Loop      bool   `json:"loop,omitempty"`
}

type lookupArgs struct {
	Surah   int    `json:"surah" jsonschema:"Surah (chapter) number, 1-114"`
	Ayah    int    `json:"ayah" jsonschema:"Ayah (verse) number within the surah"`
	Edition string `json:"edition,omitempty" jsonschema:"Optional edition id to override the default"`
}

type textResult struct {
	Reference string `json:"reference"`
	Text      string `json:"text"`
}

type stopArgs struct{}

type stopResult struct {
	Stopped bool   `json:"stopped"`
	Note    string `json:"note,omitempty"`
}

type searchArgs struct {
	Query    string `json:"query" jsonschema:"The whole thing the user recited or described (may be long or contain mistakes; pass it all)"`
	Language string `json:"language,omitempty" jsonschema:"Must be exactly 'ar' or 'en'. 'ar' fuzzy-searches the Arabic text (default); 'en' searches the English translation"`
}

type searchResult struct {
	Count   int           `json:"count"`
	Matches []quran.Match `json:"matches"`
}

// quranTools builds the tool set exposed to Gemini. Playback tools resolve the
// live call via the session registry; content tools use the shared Quran client.
func (e *Engine) quranTools() []tool.Tool {
	var tools []tool.Tool
	add := func(t tool.Tool, err error) {
		if err != nil {
			e.log.Error("build quran tool", "err", err)
			return
		}
		tools = append(tools, t)
	}

	add(functiontool.New(functiontool.Config{
		Name: "play_ayah",
		Description: "Play the reciter's audio of a specific verse, optionally repeated. " +
			"Use this when the user names a surah and verse to play, or picks a search result.",
	}, func(ctx tool.Context, a playAyahArgs) (playResult, error) {
		c, ok := e.lookup(ctx.SessionID())
		if !ok {
			return playResult{}, fmt.Errorf("no active call")
		}
		if err := c.guardNotPlaying(); err != nil {
			return playResult{}, err
		}
		if !quran.Valid(a.Surah, a.Ayah) {
			return playResult{}, fmt.Errorf("no such verse %d:%d", a.Surah, a.Ayah)
		}
		repeat := effRepeat(a.Repeat, a.Loop)
		c.startPlayback([]quran.Ref{{Surah: a.Surah, Ayah: a.Ayah}}, repeat)
		return playResult{
			Playing:   true,
			Reference: reference(a.Surah, a.Ayah),
			Surah:     a.Surah, Ayah: a.Ayah, Verses: 1, Repeat: repeat, Loop: a.Loop,
		}, nil
	}))

	add(functiontool.New(functiontool.Config{
		Name: "play_surah",
		Description: "Play an entire surah from its first verse to its last with one call. " +
			"Use whenever the user names a surah without a specific verse ('play Surah " +
			"Al-Mulk', 'recite Ya-Sin'). Do NOT compute verse counts or chain other play calls.",
	}, func(ctx tool.Context, a playSurahArgs) (playResult, error) {
		if quran.AyahCount(a.Surah) == 0 {
			return playResult{}, fmt.Errorf("no such surah %d; there are 114", a.Surah)
		}
		start := quran.Ref{Surah: a.Surah, Ayah: 1}
		end := quran.Ref{Surah: a.Surah, Ayah: quran.AyahCount(a.Surah)}
		return e.playRange(ctx, start, end, a.Repeat, a.Loop)
	}))

	add(functiontool.New(functiontool.Config{
		Name: "play_page",
		Description: "Play everything on one page of the standard (Madani) mushaf, pages " +
			"1-604, with one call. Use when the user asks by page number ('play page 50').",
	}, func(ctx tool.Context, a playPageArgs) (playResult, error) {
		start, end, ok := quran.PageRange(a.Page)
		if !ok {
			return playResult{}, fmt.Errorf("no such page %d; the mushaf has %d pages", a.Page, quran.PageCount())
		}
		return e.playRange(ctx, start, end, a.Repeat, a.Loop)
	}))

	add(functiontool.New(functiontool.Config{
		Name: "play_range",
		Description: "Play a continuous range of verses from a starting surah:ayah through an " +
			"ending surah:ayah (inclusive), even across surah boundaries, in one call. Use for " +
			"requests like 'play Al-Baqarah verses one to twenty'. Never split a range into " +
			"multiple play calls.",
	}, func(ctx tool.Context, a playRangeArgs) (playResult, error) {
		start := quran.Ref{Surah: a.Surah, Ayah: a.Ayah}
		end := quran.Ref{Surah: a.EndSurah, Ayah: a.EndAyah}
		return e.playRange(ctx, start, end, a.Repeat, a.Loop)
	}))

	add(functiontool.New(functiontool.Config{
		Name: "play_next",
		Description: "Play the next verse(s) after the one currently/last playing. " +
			"count = how many verses forward; repeat = times to loop the selection.",
	}, func(ctx tool.Context, a playRelativeArgs) (playResult, error) {
		return e.playRelative(ctx, a, true)
	}))

	add(functiontool.New(functiontool.Config{
		Name: "play_previous",
		Description: "Play the previous verse(s) before the one currently/last playing. " +
			"count = how many verses back; repeat = times to loop the selection.",
	}, func(ctx tool.Context, a playRelativeArgs) (playResult, error) {
		return e.playRelative(ctx, a, false)
	}))

	add(functiontool.New(functiontool.Config{
		Name: "stop_recitation",
		Description: "Stop the recitation that is currently playing. Use this ONLY when the " +
			"driver asks to stop and a recitation is still playing (their spoken STOP is " +
			"normally handled automatically; this is the fallback). Safe to call anytime.",
	}, func(ctx tool.Context, _ stopArgs) (stopResult, error) {
		c, ok := e.lookup(ctx.SessionID())
		if !ok {
			return stopResult{}, fmt.Errorf("no active call")
		}
		if !c.playbackActive.Load() {
			return stopResult{Stopped: false, Note: "nothing was playing"}, nil
		}
		c.stopPlayback()
		return stopResult{Stopped: true}, nil
	}))

	add(functiontool.New(functiontool.Config{
		Name: "search_ayah",
		Description: "Find the verse(s) that best match what the user recited (Arabic) or " +
			"described (English meaning). Pass the WHOLE thing they said even if long or " +
			"possibly mis-recited — Arabic search is fuzzy and tolerates wrong/missing " +
			"words. Returns ranked candidates (best first, with a match score) to confirm " +
			"before playing.",
	}, func(ctx tool.Context, a searchArgs) (searchResult, error) {
		// Search routes by script itself; language is only a hint. Never hard-fail
		// on no-match — return count 0 so the model can retry a smarter way
		// (shorter/rephrased query) instead of just giving up.
		matches, err := e.qc.Search(ctx, a.Query, a.Language, 5)
		if err != nil {
			e.log.Warn("search_ayah", "query", a.Query, "err", err)
			return searchResult{Count: 0}, nil
		}
		// Warm the top candidates in the background so confirm->play is instant
		// whichever one the driver picks.
		var top []quran.Ref
		for i := 0; i < len(matches) && i < 3; i++ {
			top = append(top, quran.Ref{Surah: matches[i].Surah, Ayah: matches[i].Ayah})
		}
		e.prefetch(top...)
		return searchResult{Count: len(matches), Matches: matches}, nil
	}))

	add(functiontool.New(functiontool.Config{
		Name: "get_translation",
		Description: "Fetch the translation text of a verse. Use this when the user asks what " +
			"a verse says or means, or asks to translate it. Read the returned text verbatim; " +
			"never translate from your own knowledge.",
	}, func(ctx tool.Context, a lookupArgs) (textResult, error) {
		if !quran.Valid(a.Surah, a.Ayah) {
			return textResult{}, fmt.Errorf("no such verse %d:%d", a.Surah, a.Ayah)
		}
		txt, err := e.qc.Translation(ctx, a.Surah, a.Ayah, a.Edition)
		if err != nil {
			return textResult{}, err
		}
		return textResult{Reference: reference(a.Surah, a.Ayah), Text: txt}, nil
	}))

	add(functiontool.New(functiontool.Config{
		Name: "get_tafsir",
		Description: "Fetch the tafsir (scholarly commentary) of a verse. Use this when the user " +
			"asks for explanation, commentary or deeper meaning. Never invent commentary. It can " +
			"be long — offer to summarize from the returned text.",
	}, func(ctx tool.Context, a lookupArgs) (textResult, error) {
		if !quran.Valid(a.Surah, a.Ayah) {
			return textResult{}, fmt.Errorf("no such verse %d:%d", a.Surah, a.Ayah)
		}
		txt, err := e.qc.Tafsir(ctx, a.Surah, a.Ayah, a.Edition)
		if err != nil {
			return textResult{}, err
		}
		return textResult{Reference: reference(a.Surah, a.Ayah), Text: txt}, nil
	}))

	return tools
}

// playRelative handles play_next / play_previous relative to the call's current
// verse, walking the surah boundaries via the baked table.
func (e *Engine) playRelative(ctx tool.Context, a playRelativeArgs, forward bool) (playResult, error) {
	c, ok := e.lookup(ctx.SessionID())
	if !ok {
		return playResult{}, fmt.Errorf("no active call")
	}
	if err := c.guardNotPlaying(); err != nil {
		return playResult{}, err
	}
	c.playMu.Lock()
	cur := c.cur
	c.playMu.Unlock()
	if !quran.Valid(cur.Surah, cur.Ayah) {
		return playResult{}, fmt.Errorf("no verse is playing yet; ask the user which verse to start from")
	}

	count := clamp(a.Count, 1, maxVerses)
	repeat := effRepeat(a.Repeat, a.Loop)

	var refs []quran.Ref
	r := cur
	for i := 0; i < count; i++ {
		var n quran.Ref
		var ok bool
		if forward {
			n, ok = quran.Next(r.Surah, r.Ayah)
		} else {
			n, ok = quran.Prev(r.Surah, r.Ayah)
		}
		if !ok {
			break
		}
		refs = append(refs, n)
		r = n
	}
	if len(refs) == 0 {
		if forward {
			return playResult{}, fmt.Errorf("already at the end of the Quran")
		}
		return playResult{}, fmt.Errorf("already at the beginning of the Quran")
	}
	// For "previous", play in reading order (Prev walks backwards).
	if !forward {
		for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
			refs[i], refs[j] = refs[j], refs[i]
		}
	}
	c.startPlayback(refs, repeat)
	first := refs[0]
	return playResult{
		Playing:   true,
		Reference: reference(first.Surah, first.Ayah),
		Surah:     first.Surah, Ayah: first.Ayah, Verses: len(refs), Repeat: repeat, Loop: a.Loop,
	}, nil
}

// playRange handles play_surah / play_page / play_range: materialize the
// inclusive verse range and start playback. RefsBetween's errors are written
// for the model and returned verbatim.
func (e *Engine) playRange(ctx tool.Context, start, end quran.Ref, repeat int, loop bool) (playResult, error) {
	c, ok := e.lookup(ctx.SessionID())
	if !ok {
		return playResult{}, fmt.Errorf("no active call")
	}
	if err := c.guardNotPlaying(); err != nil {
		return playResult{}, err
	}
	refs, err := quran.RefsBetween(start, end, maxVerses)
	if err != nil {
		return playResult{}, err
	}
	rep := effRepeat(repeat, loop)
	c.startPlayback(refs, rep)
	return playResult{
		Playing:   true,
		Reference: reference(start.Surah, start.Ayah),
		Surah:     start.Surah, Ayah: start.Ayah,
		EndSurah: end.Surah, EndAyah: end.Ayah,
		Verses: len(refs), Repeat: rep, Loop: loop,
	}, nil
}

// guardNotPlaying rejects play tools while a recitation is in progress. During
// playback the driver's spoken control words (stop/again/next/previous) are
// handled by the web-service keyword spotter; if the model also acts on the
// same words its play call silently REPLACES the spotter's action (observed:
// "again" → model called play_previous(10) → jumped to the previous surah).
// One controller during playback: the spotter. The error text instructs the
// model, since tool errors are returned to it verbatim.
func (c *call) guardNotPlaying() error {
	if c.playbackActive.Load() {
		return fmt.Errorf("a recitation is currently playing; the driver's spoken words stop/again/next/previous control it automatically. Do not call play tools or speak until you receive the system note that it finished")
	}
	return nil
}

// effRepeat maps tool repeat/loop args to the internal repeat count, where 0
// means loop forever (see startPlayback).
func effRepeat(repeat int, loop bool) int {
	if loop {
		return 0
	}
	return clamp(repeat, 1, maxRepeat)
}

// clamp defaults v to lo when unset (<1) and caps it at hi.
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func reference(surah, ayah int) string {
	return fmt.Sprintf("%s %d:%d", quran.SurahName(surah), surah, ayah)
}
