// Throwaway: feeds a 16 kHz WAV into the kws spotter (build with -tags sherpa)
// and prints any detected keyword. Verifies the dev-container spotter offline.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/ahmkindi/quran-agent/services/web/internal/kws"
)

func main() {
	in := flag.String("in", "", "16 kHz mono s16le WAV")
	flag.Parse()

	var thr float64
	if v := os.Getenv("KWS_THRESHOLD"); v != "" {
		fmt.Sscanf(v, "%f", &thr)
	}
	sp := kws.New(kws.Config{
		ModelDir:     os.Getenv("KWS_MODEL_DIR"),
		KeywordsFile: os.Getenv("KWS_KEYWORDS_FILE"),
		Threshold:    float32(thr),
		Log:          slog.Default(),
	})
	if sp == nil {
		fmt.Println("no spotter")
		os.Exit(1)
	}
	defer sp.Close()
	st := sp.NewStream()
	defer st.Close()

	b, err := os.ReadFile(*in)
	if err != nil {
		panic(err)
	}
	pcm := b[44:] // naive: skip canonical WAV header
	const frame = 640
	hits := 0
	for off := 0; off+frame <= len(pcm); off += frame {
		if kw := st.Feed(pcm[off : off+frame]); kw != "" {
			fmt.Printf("DETECTED %q at %.2fs\n", kw, float64(off)/2/16000)
			hits++
		}
	}
	fmt.Printf("hits=%d\n", hits)
	if hits == 0 {
		os.Exit(1)
	}
}

var _ = binary.LittleEndian
