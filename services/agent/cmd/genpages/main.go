// Command genpages generates the baked mushaf page table for quran/pages.go.
// It fetches alquran.cloud metadata (604 Madani page start references) and
// prints the pageStarts Go literal to stdout. Run once and paste the output;
// keep this command around for regeneration.
//
//	go run ./services/agent/cmd/genpages
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/ahmkindi/quran-agent/services/agent/internal/quran"
)

const metaURL = "https://api.alquran.cloud/v1/meta"

func main() {
	resp, err := http.Get(metaURL)
	if err != nil {
		fatal("fetch meta: %v", err)
	}
	defer resp.Body.Close()

	// alquran.cloud returns `data` as a plain string on errors, so decode it
	// lazily and only unmarshal the typed struct when the envelope says 200.
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		fatal("decode envelope: %v", err)
	}
	if env.Code != 200 {
		fatal("meta API returned code %d", env.Code)
	}
	var data struct {
		Pages struct {
			Count      int `json:"count"`
			References []struct {
				Surah int `json:"surah"`
				Ayah  int `json:"ayah"`
			} `json:"references"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		fatal("decode meta data: %v", err)
	}
	refs := data.Pages.References
	if len(refs) != 604 {
		fatal("expected 604 page references, got %d", len(refs))
	}

	fmt.Println("var pageStarts = [604]uint16{")
	for i := 0; i < len(refs); i += 10 {
		fmt.Print("\t")
		for j := i; j < i+10 && j < len(refs); j++ {
			r := refs[j]
			if !quran.Valid(r.Surah, r.Ayah) {
				fatal("page %d has invalid start %d:%d", j+1, r.Surah, r.Ayah)
			}
			fmt.Printf("%d, ", quran.GlobalNumber(r.Surah, r.Ayah))
		}
		fmt.Println()
	}
	fmt.Println("}")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genpages: "+format+"\n", args...)
	os.Exit(1)
}
