// Package logging sets up a process-wide structured logger (slog).
// Dev uses human-readable text; prod uses JSON.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Setup builds a slog.Logger from level ("debug|info|warn|error") and
// format ("text|json"), sets it as the default, and returns it.
func Setup(service, level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.ToLower(format) == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}

	logger := slog.New(h).With("service", service)
	slog.SetDefault(logger)
	return logger
}
