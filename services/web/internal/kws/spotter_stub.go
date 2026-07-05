//go:build !sherpa

package kws

// New returns a no-op spotter. Build with `-tags sherpa` (and the native
// sherpa-onnx libs + a KWS model) to enable real halt-word detection.
func New(cfg Config) Spotter {
	if cfg.Log != nil {
		cfg.Log.Info("kws: halt-word spotter disabled (built without 'sherpa' tag)")
	}
	return noopSpotter{}
}
