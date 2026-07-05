//go:build sherpa

package kws

import (
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-linux"
)

// New builds a real sherpa-onnx keyword spotter. If the model dir / keywords
// file are missing or the engine fails to init, it logs a warning and returns
// the no-op spotter so the stack still boots.
func New(cfg Config) Spotter {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	enc := firstGlob(cfg.ModelDir, "encoder*.onnx")
	dec := firstGlob(cfg.ModelDir, "decoder*.onnx")
	joi := firstGlob(cfg.ModelDir, "joiner*.onnx")
	tokens := filepath.Join(cfg.ModelDir, "tokens.txt")
	keywords := cfg.KeywordsFile
	if keywords == "" {
		keywords = filepath.Join(cfg.ModelDir, "keywords.txt")
	}
	if enc == "" || dec == "" || joi == "" || !exists(tokens) || !exists(keywords) {
		log.Warn("kws: model or keywords missing; halt-word spotter disabled",
			"model_dir", cfg.ModelDir, "keywords", keywords)
		return noopSpotter{}
	}

	numThreads := cfg.NumThreads
	if numThreads < 1 {
		numThreads = 1
	}

	c := &sherpa.KeywordSpotterConfig{}
	c.FeatConfig = sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80}
	c.ModelConfig.Transducer = sherpa.OnlineTransducerModelConfig{Encoder: enc, Decoder: dec, Joiner: joi}
	c.ModelConfig.Tokens = tokens
	c.ModelConfig.NumThreads = numThreads
	c.ModelConfig.Provider = "cpu"
	c.ModelConfig.ModelingUnit = "bpe"
	c.KeywordsFile = keywords
	if cfg.Threshold > 0 {
		c.KeywordsThreshold = cfg.Threshold
	}
	if cfg.Score > 0 {
		c.KeywordsScore = cfg.Score
	}

	sp := sherpa.NewKeywordSpotter(c)
	if sp == nil {
		log.Warn("kws: sherpa-onnx failed to init; halt-word spotter disabled")
		return noopSpotter{}
	}
	log.Info("kws: halt-word spotter ready", "model_dir", cfg.ModelDir, "keywords", keywords)
	return &realSpotter{sp: sp, log: log}
}

type realSpotter struct {
	sp  *sherpa.KeywordSpotter
	log *slog.Logger
}

func (r *realSpotter) NewStream() Stream {
	return &realStream{sp: r.sp, s: sherpa.NewKeywordStream(r.sp)}
}

func (r *realSpotter) Close() { sherpa.DeleteKeywordSpotter(r.sp) }

type realStream struct {
	sp *sherpa.KeywordSpotter
	s  *sherpa.OnlineStream
}

func (r *realStream) Feed(pcm16 []byte) string {
	n := len(pcm16) / 2
	if n == 0 {
		return ""
	}
	samples := make([]float32, n)
	for i := 0; i < n; i++ {
		samples[i] = float32(int16(binary.LittleEndian.Uint16(pcm16[i*2:]))) / 32768.0
	}
	r.s.AcceptWaveform(16000, samples)
	for r.sp.IsReady(r.s) {
		r.sp.Decode(r.s)
	}
	if res := r.sp.GetResult(r.s); res != nil && res.Keyword != "" {
		r.sp.Reset(r.s)
		return res.Keyword
	}
	return ""
}

func (r *realStream) Close() { sherpa.DeleteOnlineStream(r.s) }

func firstGlob(dir, pattern string) string {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
