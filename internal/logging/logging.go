// Package logging configures the process-wide structured logger.
package logging

import (
	"log/slog"
	"os"
	"strings"

	"github.com/esaiaswestberg/imapped/internal/config"
)

// New builds a logger from configuration. Format "text" is meant for a terminal
// during development; "json" is the default and what production should ship.
func New(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: ParseLevel(cfg.LogLevel)}

	var handler slog.Handler
	if strings.EqualFold(cfg.LogFormat, "text") {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}

// ParseLevel maps a configured level name to a slog level, defaulting to info
// for anything unrecognised. Validate rejects bad names at startup, so this
// fallback only matters for callers that bypass validation.
func ParseLevel(name string) slog.Level {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Discard returns a logger that drops everything, for tests that exercise code
// paths whose log output is not the thing under test.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
