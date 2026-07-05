package adkengine

import (
	"testing"

	"github.com/ahmkindi/quran-agent/services/agent/internal/quran"
)

func TestNavTarget(t *testing.T) {
	tests := []struct {
		name   string
		cur    quran.Ref
		action string
		want   quran.Ref
		ok     bool
	}{
		{"again", quran.Ref{Surah: 2, Ayah: 255}, "again", quran.Ref{Surah: 2, Ayah: 255}, true},
		{"next within surah", quran.Ref{Surah: 2, Ayah: 255}, "next", quran.Ref{Surah: 2, Ayah: 256}, true},
		{"next across surah", quran.Ref{Surah: 1, Ayah: 7}, "next", quran.Ref{Surah: 2, Ayah: 1}, true},
		{"previous within surah", quran.Ref{Surah: 2, Ayah: 2}, "previous", quran.Ref{Surah: 2, Ayah: 1}, true},
		{"previous across surah", quran.Ref{Surah: 2, Ayah: 1}, "previous", quran.Ref{Surah: 1, Ayah: 7}, true},
		{"next at end of quran", quran.Ref{Surah: 114, Ayah: 6}, "next", quran.Ref{}, false},
		{"previous at start of quran", quran.Ref{Surah: 1, Ayah: 1}, "previous", quran.Ref{}, false},
		{"invalid cur", quran.Ref{}, "again", quran.Ref{}, false},
		{"unknown action", quran.Ref{Surah: 2, Ayah: 255}, "shuffle", quran.Ref{}, false},
	}
	for _, tt := range tests {
		got, ok := navTarget(tt.cur, tt.action)
		if ok != tt.ok {
			t.Errorf("%s: navTarget(%v, %q) ok = %v, want %v", tt.name, tt.cur, tt.action, ok, tt.ok)
			continue
		}
		// The ref only means something on ok.
		if ok && got != tt.want {
			t.Errorf("%s: navTarget(%v, %q) = %v, want %v", tt.name, tt.cur, tt.action, got, tt.want)
		}
	}
}
