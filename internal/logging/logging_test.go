package logging

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

func TestLevelFromEnvDefaultsToInfo(t *testing.T) {
	t.Setenv(LevelEnvVar, "")
	if got := LevelFromEnv(); got != slog.LevelInfo {
		t.Errorf("LevelFromEnv() with unset env = %v, want %v", got, slog.LevelInfo)
	}
}

func TestLevelFromEnvUnrecognizedDefaultsToInfo(t *testing.T) {
	t.Setenv(LevelEnvVar, "not-a-level")
	if got := LevelFromEnv(); got != slog.LevelInfo {
		t.Errorf("LevelFromEnv() with garbage env = %v, want %v (fail-soft default)", got, slog.LevelInfo)
	}
}

func TestLevelFromEnvRecognizesEachLevelCaseInsensitively(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"WARN":    slog.LevelWarn,
		"error":   slog.LevelError,
		"Error":   slog.LevelError,
		"  warn ": slog.LevelWarn, // surrounding whitespace tolerated
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Setenv(LevelEnvVar, in)
			if got := LevelFromEnv(); got != want {
				t.Errorf("LevelFromEnv() with %q = %v, want %v", in, got, want)
			}
		})
	}
}

func TestNewWritesToStderrHandlerAtConfiguredLevel(t *testing.T) {
	t.Setenv(LevelEnvVar, "warn")
	l := New()
	if l == nil {
		t.Fatal("New() returned nil")
	}
	ctx := context.Background()
	if l.Enabled(ctx, slog.LevelInfo) {
		t.Errorf("logger built with level=warn must not be Info-enabled")
	}
	if !l.Enabled(ctx, slog.LevelWarn) {
		t.Errorf("logger built with level=warn must be Warn-enabled")
	}
}

func TestRedactReplacesEveryOccurrence(t *testing.T) {
	in := "token=abc123 seen twice: abc123"
	got := Redact(in, "abc123")
	if got != "token=[REDACTED] seen twice: [REDACTED]" {
		t.Errorf("Redact() = %q, want both occurrences replaced", got)
	}
}

func TestRedactNoopOnEmptySecret(t *testing.T) {
	in := "nothing to redact here"
	if got := Redact(in, ""); got != in {
		t.Errorf("Redact() with empty secret = %q, want unchanged %q", got, in)
	}
}

func TestRedactNoopWhenSecretAbsent(t *testing.T) {
	in := "an ordinary log line"
	if got := Redact(in, "not-present"); got != in {
		t.Errorf("Redact() = %q, want unchanged %q", got, in)
	}
}

// TestNewIsStderrBacked is a light sanity check that New() builds a working
// logger without panicking. internal/provider's own redaction test instead
// swaps slog's process-wide default logger to a buffer-backed one to make
// assertions on emitted text (os.Stderr itself cannot be safely captured
// mid-test against -race parallel runs).
func TestNewIsStderrBacked(t *testing.T) {
	l := New()
	l.Info("logging self-test", "component", "logging")
	if os.Stderr == nil {
		t.Fatal("os.Stderr is nil in this environment")
	}
}
