package quran

import (
	"reflect"
	"testing"
)

func TestTokenizeArabic(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "strips diacritics and punctuation",
			in:   "الْحَيُّ الْقَيُّومُ ۚ",
			want: []string{"الحي", "القيوم"},
		},
		{
			name: "folds alef/ya/ta-marbuta/hamza variants",
			in:   "إنّ اللهَ عَلىٰ رحمةٌ", // إ->ا, ى->ي, ة->ه
			want: []string{"ان", "الله", "علي", "رحمه"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tokenizeArabic(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("tokenizeArabic(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
