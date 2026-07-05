// Package quran provides Quran data access for the agent: per-ayah recitation
// audio (fetched + decoded to 24 kHz PCM for the outbound stream), translations,
// tafsir, and full-text verse search. All content comes from public APIs — the
// agent never generates translations or tafsir itself.
package quran

import "fmt"

// ayahCounts[i] is the number of ayat in surah (i+1). Standard Hafs numbering;
// the 114 values sum to 6236. Used for bounds (next/previous), validation, and
// computing the global ayah number (1..6236) that the islamic.network audio CDN
// keys on.
var ayahCounts = [114]int{
	7, 286, 200, 176, 120, 165, 206, 75, 129, 109, 123, 111, 43, 52, 99, 128,
	111, 110, 98, 135, 112, 78, 118, 64, 77, 227, 93, 88, 69, 60, 34, 30, 73,
	54, 45, 83, 182, 88, 75, 85, 54, 53, 89, 59, 37, 35, 38, 29, 18, 45, 60,
	49, 62, 55, 78, 96, 29, 22, 24, 13, 14, 11, 11, 18, 12, 12, 30, 52, 52, 44,
	28, 28, 20, 56, 40, 31, 50, 40, 46, 42, 29, 19, 36, 25, 22, 17, 19, 26, 30,
	20, 15, 21, 11, 8, 8, 19, 5, 8, 8, 11, 11, 8, 3, 9, 5, 4, 7, 3, 6, 3, 5, 4,
	5, 6,
}

// surahNames[i] is the common English transliterated name of surah (i+1), used
// for spoken confirmations ("Surah Al-Baqarah, verse 255").
var surahNames = [114]string{
	"Al-Fatihah", "Al-Baqarah", "Aal-E-Imran", "An-Nisa", "Al-Ma'idah",
	"Al-An'am", "Al-A'raf", "Al-Anfal", "At-Tawbah", "Yunus", "Hud", "Yusuf",
	"Ar-Ra'd", "Ibrahim", "Al-Hijr", "An-Nahl", "Al-Isra", "Al-Kahf", "Maryam",
	"Ta-Ha", "Al-Anbiya", "Al-Hajj", "Al-Mu'minun", "An-Nur", "Al-Furqan",
	"Ash-Shu'ara", "An-Naml", "Al-Qasas", "Al-Ankabut", "Ar-Rum", "Luqman",
	"As-Sajdah", "Al-Ahzab", "Saba", "Fatir", "Ya-Sin", "As-Saffat", "Sad",
	"Az-Zumar", "Ghafir", "Fussilat", "Ash-Shura", "Az-Zukhruf", "Ad-Dukhan",
	"Al-Jathiyah", "Al-Ahqaf", "Muhammad", "Al-Fath", "Al-Hujurat", "Qaf",
	"Adh-Dhariyat", "At-Tur", "An-Najm", "Al-Qamar", "Ar-Rahman", "Al-Waqi'ah",
	"Al-Hadid", "Al-Mujadila", "Al-Hashr", "Al-Mumtahanah", "As-Saff",
	"Al-Jumu'ah", "Al-Munafiqun", "At-Taghabun", "At-Talaq", "At-Tahrim",
	"Al-Mulk", "Al-Qalam", "Al-Haqqah", "Al-Ma'arij", "Nuh", "Al-Jinn",
	"Al-Muzzammil", "Al-Muddaththir", "Al-Qiyamah", "Al-Insan", "Al-Mursalat",
	"An-Naba", "An-Nazi'at", "Abasa", "At-Takwir", "Al-Infitar", "Al-Mutaffifin",
	"Al-Inshiqaq", "Al-Buruj", "At-Tariq", "Al-A'la", "Al-Ghashiyah", "Al-Fajr",
	"Al-Balad", "Ash-Shams", "Al-Layl", "Ad-Duha", "Ash-Sharh", "At-Tin",
	"Al-Alaq", "Al-Qadr", "Al-Bayyinah", "Az-Zalzalah", "Al-Adiyat",
	"Al-Qari'ah", "At-Takathur", "Al-Asr", "Al-Humazah", "Al-Fil", "Quraysh",
	"Al-Ma'un", "Al-Kawthar", "Al-Kafirun", "An-Nasr", "Al-Masad", "Al-Ikhlas",
	"Al-Falaq", "An-Nas",
}

// Ref is a verse reference (surah:ayah), 1-indexed.
type Ref struct {
	Surah int
	Ayah  int
}

func (r Ref) String() string { return fmt.Sprintf("%d:%d", r.Surah, r.Ayah) }

// SurahCount returns the number of surahs (114).
func SurahCount() int { return 114 }

// AyahCount returns the number of ayat in the given surah, or 0 if out of range.
func AyahCount(surah int) int {
	if surah < 1 || surah > 114 {
		return 0
	}
	return ayahCounts[surah-1]
}

// SurahName returns the English name of the surah, or "" if out of range.
func SurahName(surah int) string {
	if surah < 1 || surah > 114 {
		return ""
	}
	return surahNames[surah-1]
}

// Valid reports whether surah:ayah is a real verse.
func Valid(surah, ayah int) bool {
	return surah >= 1 && surah <= 114 && ayah >= 1 && ayah <= ayahCounts[surah-1]
}

// GlobalNumber returns the 1..6236 running ayah number used by the
// islamic.network audio CDN, or 0 if the reference is invalid.
func GlobalNumber(surah, ayah int) int {
	if !Valid(surah, ayah) {
		return 0
	}
	n := 0
	for i := 0; i < surah-1; i++ {
		n += ayahCounts[i]
	}
	return n + ayah
}

// Next returns the verse after (surah, ayah), rolling into the next surah. The
// second return is false at the very end of the Quran (114:6).
func Next(surah, ayah int) (Ref, bool) {
	if !Valid(surah, ayah) {
		return Ref{}, false
	}
	if ayah < ayahCounts[surah-1] {
		return Ref{surah, ayah + 1}, true
	}
	if surah < 114 {
		return Ref{surah + 1, 1}, true
	}
	return Ref{surah, ayah}, false
}

// Prev returns the verse before (surah, ayah), rolling into the previous surah.
// The second return is false at the very start of the Quran (1:1).
func Prev(surah, ayah int) (Ref, bool) {
	if !Valid(surah, ayah) {
		return Ref{}, false
	}
	if ayah > 1 {
		return Ref{surah, ayah - 1}, true
	}
	if surah > 1 {
		return Ref{surah - 1, ayahCounts[surah-2]}, true
	}
	return Ref{surah, ayah}, false
}
