package log_test

import (
	"context"
	"log/slog"
	"testing"

	pkglog "github.com/bkaznowski/epochd/pkg/log"
)

func TestNew(t *testing.T) {
	l := pkglog.New()
	if l == nil {
		t.Fatal("New returned nil")
	}
}

func TestDiscard(t *testing.T) {
	l := pkglog.Discard()
	if l == nil {
		t.Fatal("Discard returned nil")
	}
	l.Info("should not panic", "key", "value")
	l.Error("also fine", "err", "none")
}

func TestNewLevels(t *testing.T) {
	tests := []struct {
		env       string
		wantLevel slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"bogus", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
	}
	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", tc.env)
			l := pkglog.New()
			if l == nil {
				t.Fatalf("LOG_LEVEL=%q: New returned nil", tc.env)
			}
			ctx := context.Background()
			if !l.Enabled(ctx, tc.wantLevel) {
				t.Errorf("LOG_LEVEL=%q: expected level %v to be enabled", tc.env, tc.wantLevel)
			}
			if tc.wantLevel > slog.LevelDebug && l.Enabled(ctx, slog.LevelDebug) {
				t.Errorf("LOG_LEVEL=%q: debug should not be enabled at level %v", tc.env, tc.wantLevel)
			}
		})
	}
}
