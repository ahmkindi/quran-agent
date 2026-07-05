package quran

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

func TestPageStartsTable(t *testing.T) {
	if PageCount() != 604 {
		t.Fatalf("PageCount = %d, want 604", PageCount())
	}
	if pageStarts[0] != 1 {
		t.Fatalf("page 1 starts at global %d, want 1", pageStarts[0])
	}
	for i := 1; i < len(pageStarts); i++ {
		if pageStarts[i] <= pageStarts[i-1] {
			t.Fatalf("pageStarts not strictly increasing at page %d: %d <= %d", i+1, pageStarts[i], pageStarts[i-1])
		}
	}
	if last := int(pageStarts[603]); last > 6236 {
		t.Fatalf("page 604 starts at global %d, beyond 6236", last)
	}
}

func TestFromGlobalRoundTrip(t *testing.T) {
	n := 0
	for s := 1; s <= 114; s++ {
		for a := 1; a <= AyahCount(s); a++ {
			n++
			if got := GlobalNumber(s, a); got != n {
				t.Fatalf("GlobalNumber(%d,%d) = %d, want %d", s, a, got, n)
			}
			ref, ok := FromGlobal(n)
			if !ok || ref.Surah != s || ref.Ayah != a {
				t.Fatalf("FromGlobal(%d) = %v,%v, want %d:%d", n, ref, ok, s, a)
			}
		}
	}
	if n != 6236 {
		t.Fatalf("total ayat = %d, want 6236", n)
	}
	if _, ok := FromGlobal(0); ok {
		t.Fatal("FromGlobal(0) should fail")
	}
	if _, ok := FromGlobal(6237); ok {
		t.Fatal("FromGlobal(6237) should fail")
	}
}

func TestPageRange(t *testing.T) {
	start, end, ok := PageRange(1)
	if !ok || start != (Ref{1, 1}) || end != (Ref{1, 7}) {
		t.Fatalf("PageRange(1) = %v..%v,%v, want 1:1..1:7", start, end, ok)
	}
	start, end, ok = PageRange(604)
	if !ok || start != (Ref{112, 1}) || end != (Ref{114, 6}) {
		t.Fatalf("PageRange(604) = %v..%v,%v, want 112:1..114:6", start, end, ok)
	}
	// Every page's end must be exactly the verse before the next page's start.
	for p := 1; p < 604; p++ {
		_, end, _ := PageRange(p)
		nextStart, _, _ := PageRange(p + 1)
		after, ok := Next(end.Surah, end.Ayah)
		if !ok || after != nextStart {
			t.Fatalf("page %d ends %v but page %d starts %v", p, end, p+1, nextStart)
		}
	}
	if _, _, ok := PageRange(0); ok {
		t.Fatal("PageRange(0) should fail")
	}
	if _, _, ok := PageRange(605); ok {
		t.Fatal("PageRange(605) should fail")
	}
}

func TestRefsBetween(t *testing.T) {
	// Same-surah range.
	refs, err := RefsBetween(Ref{2, 255}, Ref{2, 257}, 500)
	if err != nil || len(refs) != 3 || refs[0] != (Ref{2, 255}) || refs[2] != (Ref{2, 257}) {
		t.Fatalf("RefsBetween(2:255,2:257) = %v, %v", refs, err)
	}
	// Cross-surah range.
	refs, err = RefsBetween(Ref{2, 285}, Ref{3, 2}, 500)
	want := []Ref{{2, 285}, {2, 286}, {3, 1}, {3, 2}}
	if err != nil || len(refs) != len(want) {
		t.Fatalf("RefsBetween(2:285,3:2) = %v, %v", refs, err)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("RefsBetween(2:285,3:2)[%d] = %v, want %v", i, refs[i], want[i])
		}
	}
	// Whole Al-Baqarah.
	refs, err = RefsBetween(Ref{2, 1}, Ref{2, 286}, 500)
	if err != nil || len(refs) != 286 {
		t.Fatalf("Al-Baqarah = %d verses, %v; want 286", len(refs), err)
	}
	// Reversed range.
	if _, err = RefsBetween(Ref{3, 1}, Ref{2, 1}, 500); err == nil {
		t.Fatal("reversed range should error")
	}
	// Over max.
	if _, err = RefsBetween(Ref{1, 1}, Ref{5, 1}, 500); err == nil {
		t.Fatal("over-max range should error")
	}
	// Invalid ends.
	if _, err = RefsBetween(Ref{2, 300}, Ref{2, 301}, 500); err == nil {
		t.Fatal("invalid start should error")
	}
	if _, err = RefsBetween(Ref{2, 1}, Ref{115, 1}, 500); err == nil {
		t.Fatal("invalid end should error")
	}
}

// TestPageStartsLive cross-checks the baked table against the alquran.cloud
// meta endpoint. Run with QURAN_LIVE_TEST=1.
func TestPageStartsLive(t *testing.T) {
	if os.Getenv("QURAN_LIVE_TEST") == "" {
		t.Skip("set QURAN_LIVE_TEST=1 to run")
	}
	resp, err := http.Get("https://api.alquran.cloud/v1/meta")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Code != 200 {
		t.Fatalf("meta API code %d", env.Code)
	}
	var data struct {
		Pages struct {
			References []struct {
				Surah int `json:"surah"`
				Ayah  int `json:"ayah"`
			} `json:"references"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	refs := data.Pages.References
	if len(refs) != 604 {
		t.Fatalf("meta has %d pages, want 604", len(refs))
	}
	for i, r := range refs {
		if g := GlobalNumber(r.Surah, r.Ayah); g != int(pageStarts[i]) {
			t.Errorf("page %d: baked %d, live %d (%d:%d)", i+1, pageStarts[i], g, r.Surah, r.Ayah)
		}
	}
}
