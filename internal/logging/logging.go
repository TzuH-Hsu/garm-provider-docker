// Package logging builds the provider's slog logger. Logs go to stderr because
// stdout carries the JSON GARM parses.
//
// Credentials must never be logged. Keep them out of log call arguments, pass
// derived error text through Redact, and pass a runner container's own output
// through SafeLogLines.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// LevelEnvVar selects the log level. It is an environment variable so an operator
// can debug one invocation without editing the shared config file.
const LevelEnvVar = "GARM_PROVIDER_DOCKER_LOG_LEVEL"

// New returns a text logger that writes to stderr at the LevelFromEnv level.
func New() *slog.Logger {
	return NewAt(LevelFromEnv())
}

// NewAt is New with an explicit level, for the detached status watcher, which
// starts with an empty environment.
func NewAt(level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	}))
}

// LevelFromEnv returns ParseLevel of LevelEnvVar.
func LevelFromEnv() slog.Level {
	return ParseLevel(os.Getenv(LevelEnvVar))
}

// ParseLevel maps debug, info, warn, warning or error, in any case, to a
// slog.Level. Anything else means LevelInfo: a typo should neither silence output
// nor crash the provider.
func ParseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
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

const redactedPlaceholder = "[REDACTED]"

// Redact returns s with every occurrence of secret replaced by a placeholder. An
// empty secret leaves s unchanged.
func Redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, redactedPlaceholder)
}
