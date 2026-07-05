package bridge

import "testing"

func TestActionFor(t *testing.T) {
	cases := map[string]string{
		"STOP":      "stop",
		"stop":      "stop",
		" Again ":   "again",
		"NEXT":      "next",
		"PREVIOUS":  "previous",
		"":          "",
		"UNKNOWN":   "",
		"NEXT WORD": "",
	}
	for kw, want := range cases {
		if got := actionFor(kw); got != want {
			t.Errorf("actionFor(%q) = %q, want %q", kw, got, want)
		}
	}
}
