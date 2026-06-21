// Package log provides a structured JSON logger backed by log/slog.
// Log level is controlled by the LOG_LEVEL environment variable
// (debug, info, warn, error; default: info).
package log

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON logger that writes to stderr at the level specified by
// the LOG_LEVEL environment variable.
func New() *slog.Logger {
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(os.Getenv("LOG_LEVEL"))})
	return slog.New(h)
}

// Discard returns a logger that silently discards all output.
// Suitable for use in tests where log noise is unwanted.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
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

