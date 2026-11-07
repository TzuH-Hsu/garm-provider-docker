// Package logging builds the provider's slog logger. Logs go to stderr, which
// GARM captures for diagnostics; stdout carries the JSON GARM parses and must
// stay free of log lines. Each invocation is a one-shot subprocess (ADR-004), so
// call sites use slog.Default, which main.go sets once, rather than an injected
// logger.
//
// GARM_PROVIDER_DOCKER_LOG_LEVEL sets the level. It is an environment variable
// rather than a config field so an operator can debug one invocation without
// editing the config file every managed instance reads.
//
// Credentials (the instance token, JIT config bytes, metadata bearer values)
// must never be logged. The main control is keeping them out of log call
// arguments; Redact scrubs them from derived error text. A runner container's
// own output is not ours to control, so it goes through SafeLogLines first.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// LevelEnvVar is the environment variable that selects the log level.
const LevelEnvVar = "GARM_PROVIDER_DOCKER_LOG_LEVEL"

// New returns a text logger that writes to stderr at the level LevelEnvVar names
// (default info). main.go installs it with slog.SetDefault.
func New() *slog.Logger {
	return NewAt(LevelFromEnv())
}

// NewAt is New with an explicit level instead of the environment's. The
// detached status watcher uses it: it starts with an empty environment, so the
// level the provider resolved travels in its request.
func NewAt(level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	}))
}

// LevelFromEnv maps LevelEnvVar to a slog.Level. An unset or unrecognized value
// means LevelInfo: a typo should neither silence output nor crash the provider.
func LevelFromEnv() slog.Level {
	return ParseLevel(os.Getenv(LevelEnvVar))
}

// ParseLevel maps a level name (debug|info|warn|warning|error, any case,
// surrounding space ignored) to a slog.Level, defaulting to LevelInfo for
// anything else. slog.Level.String() output for the four standard levels
// round-trips through it.
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

// redactedPlaceholder replaces a secret value Redact is asked to scrub.
const redactedPlaceholder = "[REDACTED]"

// Redact returns s with every occurrence of secret replaced by a fixed
// placeholder, when secret is non-empty. It covers text derived from a request
// that held a secret, such as a transport error from an endpoint that echoed a
// token into a redirect URL.
func Redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, redactedPlaceholder)
}
