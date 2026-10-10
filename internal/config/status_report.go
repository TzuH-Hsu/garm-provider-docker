package config

import (
	"fmt"
	"time"
)

// Status-report bounds. The default wait comfortably covers a warm runner
// container reaching "Listening for Jobs" (a few seconds after credential
// delivery on the reference host) and a cold DinD start; the ceiling keeps
// CreateInstance well inside GARM's runner_bootstrap_timeout (20 minutes by
// default), whose reaper is the safety net when the wait expires.
const (
	DefaultStatusReportTimeoutSeconds = 90
	MaxStatusReportTimeoutSeconds     = 600
)

// StatusReport is the [status_report] table: provider-side runner status
// reporting to GARM's instance callback (ADR-002 amendment). After delivering
// credentials, CreateInstance reports the runner "installing" and returns; a
// detached watcher process then waits up to timeout_seconds for the runner to
// log that it is listening for jobs and reports it "idle". GARM v0.2.1 scale sets need this: their job-started
// handler moves a runner to "active" with a checked transition that only
// "idle" (or "offline") may take, so a runner left at GARM's initial "pending"
// wedges the scale-set listener until the runner is removed.
type StatusReport struct {
	// TimeoutSeconds bounds the wait for the runner to come online. Omitted
	// means DefaultStatusReportTimeoutSeconds; 0 disables status reporting
	// entirely (no callback is ever made); negative values and values above
	// MaxStatusReportTimeoutSeconds are rejected. A pointer so an omitted key
	// is distinguishable from an explicit 0.
	TimeoutSeconds *int `toml:"timeout_seconds"`
}

// Enabled reports whether status reporting is on.
func (s StatusReport) Enabled() bool {
	return s.TimeoutSeconds == nil || *s.TimeoutSeconds > 0
}

// Timeout is the effective bound on the wait for the runner to come online.
// It is meaningful only when Enabled.
func (s StatusReport) Timeout() time.Duration {
	if s.TimeoutSeconds == nil {
		return DefaultStatusReportTimeoutSeconds * time.Second
	}
	return time.Duration(*s.TimeoutSeconds) * time.Second
}

func (s StatusReport) validate() error {
	if s.TimeoutSeconds == nil {
		return nil
	}
	if v := *s.TimeoutSeconds; v < 0 || v > MaxStatusReportTimeoutSeconds {
		return fmt.Errorf("timeout_seconds = %d is out of range: must be 0 (disabled) to %d", v, MaxStatusReportTimeoutSeconds)
	}
	return nil
}
