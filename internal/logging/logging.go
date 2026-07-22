// Package logging builds this provider's structured logger (M3-W2). All
// operational logging goes through log/slog exclusively, writing to STDERR:
// GARM captures the provider's stderr for its own diagnostics, while stdout
// is reserved for the ProviderInstance/exit-value JSON contract (research.md
// §1.E) and must never be polluted by a log line. Every call site in
// internal/provider and internal/topology logs through the process-wide
// slog.Default() (configured once, here, from main.go) rather than a
// per-package logger — this repo's one-shot subprocess model (ADR-004) means
// there is exactly one logical "session" per invocation anyway, so a single
// global logger carries no cross-call state risk and keeps constructor
// signatures (provider.New, topology.New) unchanged, which is why it is
// implemented this way rather than as an injected dependency.
//
// Level is controlled by the GARM_PROVIDER_DOCKER_LOG_LEVEL environment
// variable (debug|info|warn|error, case-insensitive), defaulting to info
// when unset or unrecognized. This is deliberately an env knob, not a
// config.toml field: GARM launches the provider as a short-lived subprocess
// per command, and an operator debugging one specific invocation can set the
// env var for that one run (or via [provider.external] environment_variables
// passthrough) without editing the shared config file every managed
// instance reads.
//
// REDACTION (CRITICAL): no call site anywhere in this codebase may log
// credential contents — the GARM instance-token, JIT config file bytes, or
// any metadata bearer value. See Redact and the package doc on why this is
// enforced structurally (no logger ever receives those values in the first
// place) rather than by scrubbing after the fact.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// LevelEnvVar is the environment variable that selects the log level.
const LevelEnvVar = "GARM_PROVIDER_DOCKER_LOG_LEVEL"

// New builds the provider's slog.Logger: a text-format handler writing to
// STDERR at the level LevelEnvVar names (default info). main.go installs the
// result as the process-wide default via slog.SetDefault before doing
// anything else, so every subsequent slog.Info/Warn/Error call in
// internal/provider and internal/topology is already correctly leveled and
// routed to stderr.
func New() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: LevelFromEnv(),
	}))
}

// LevelFromEnv reads LevelEnvVar and maps it to a slog.Level, defaulting to
// LevelInfo when the variable is unset or holds an unrecognized value (fail
// soft: a typo in an operator's env knob should not silence useful output
// entirely, nor should it crash the provider).
func LevelFromEnv() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(LevelEnvVar))) {
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
// placeholder, when secret is non-empty. It exists as defense-in-depth for
// the rare call site that must log a value DERIVED from a request containing
// a secret (e.g. an upstream error string a hostile or misbehaving metadata
// endpoint could theoretically echo a token into) — not because any current
// call site needs it, but so a future one has a single, audited helper to
// reach for instead of hand-rolling string surgery. Every credential-bearing
// value in this codebase (the instance token, fetched JIT/registration
// credential bytes) is instead kept out of every log call's arguments
// entirely, which is the primary and preferred control; Redact is the
// secondary one.
func Redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, redactedPlaceholder)
}
