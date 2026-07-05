package adkengine

import "testing"

func TestEffRepeat(t *testing.T) {
	cases := []struct {
		repeat int
		loop   bool
		want   int
	}{
		{repeat: 5, loop: false, want: 5},
		{repeat: 0, loop: false, want: 1},   // omitted → default 1, never infinite
		{repeat: -3, loop: false, want: 1},  // defensive
		{repeat: 250, loop: false, want: maxRepeat},
		{repeat: 5, loop: true, want: 0},    // loop wins → infinite sentinel
		{repeat: 0, loop: true, want: 0},
	}
	for _, c := range cases {
		if got := effRepeat(c.repeat, c.loop); got != c.want {
			t.Errorf("effRepeat(%d, %v) = %d, want %d", c.repeat, c.loop, got, c.want)
		}
	}
}
