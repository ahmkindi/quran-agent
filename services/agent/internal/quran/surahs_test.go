package quran

import "testing"

func TestAyahCountsTotal(t *testing.T) {
	total := 0
	for s := 1; s <= 114; s++ {
		total += AyahCount(s)
	}
	if total != 6236 {
		t.Fatalf("total ayat = %d, want 6236", total)
	}
}

func TestTableLengths(t *testing.T) {
	if len(ayahCounts) != 114 {
		t.Fatalf("ayahCounts len = %d, want 114", len(ayahCounts))
	}
	if len(surahNames) != 114 {
		t.Fatalf("surahNames len = %d, want 114", len(surahNames))
	}
	for i, n := range surahNames {
		if n == "" {
			t.Errorf("surah %d has empty name", i+1)
		}
	}
}

func TestGlobalNumber(t *testing.T) {
	cases := []struct {
		s, a, want int
	}{
		{1, 1, 1},
		{1, 7, 7},
		{2, 1, 8},
		{2, 255, 262}, // Ayat al-Kursi — matches alquran.cloud data.number
		{114, 6, 6236},
	}
	for _, c := range cases {
		if got := GlobalNumber(c.s, c.a); got != c.want {
			t.Errorf("GlobalNumber(%d,%d) = %d, want %d", c.s, c.a, got, c.want)
		}
	}
	if got := GlobalNumber(2, 300); got != 0 {
		t.Errorf("GlobalNumber(2,300) = %d, want 0 (invalid)", got)
	}
}

func TestNextPrevRollover(t *testing.T) {
	if r, ok := Next(1, 7); !ok || r != (Ref{2, 1}) {
		t.Errorf("Next(1,7) = %v,%v want {2 1},true", r, ok)
	}
	if r, ok := Next(2, 5); !ok || r != (Ref{2, 6}) {
		t.Errorf("Next(2,5) = %v,%v want {2 6},true", r, ok)
	}
	if _, ok := Next(114, 6); ok {
		t.Errorf("Next(114,6) should be false (end of Quran)")
	}
	if r, ok := Prev(2, 1); !ok || r != (Ref{1, 7}) {
		t.Errorf("Prev(2,1) = %v,%v want {1 7},true", r, ok)
	}
	if _, ok := Prev(1, 1); ok {
		t.Errorf("Prev(1,1) should be false (start of Quran)")
	}
}

func TestValid(t *testing.T) {
	if !Valid(2, 255) {
		t.Error("2:255 should be valid")
	}
	if Valid(2, 287) {
		t.Error("2:287 should be invalid (Al-Baqarah has 286)")
	}
	if Valid(115, 1) {
		t.Error("surah 115 should be invalid")
	}
	if Valid(0, 1) {
		t.Error("surah 0 should be invalid")
	}
}

func TestStripArabicDiacritics(t *testing.T) {
	// "اللَّهُ" (with shadda + harakat) -> "الله"
	in := "اللَّهُ"
	want := "الله"
	if got := stripArabicDiacritics(in); got != want {
		t.Errorf("stripArabicDiacritics = %q, want %q", got, want)
	}
}
