package config

import (
	"fmt"
	"time"
)

// The maximum keeps the wait inside GARM's runner_bootstrap_timeout, whose
// reaper is the safety net when the wait expires.
const (
	DefaultStatusReportTimeoutSeconds = 90
	MaxStatusReportTimeoutSeconds     = 600
)

// StatusReport is the [status_report] table: after credentials are delivered, a
// watcher process reports the runner "installing", then "idle" once the runner logs
// that it is listening for jobs. GARM v0.2.1 scale sets need the "idle" report
// (ADR-002, "Status reporting").
type StatusReport struct {
	// TimeoutSeconds bounds the wait for the runner to come online. Omitted
	// means DefaultStatusReportTimeoutSeconds, 0 disables status reporting
	// entirely, and a negative value or one above MaxStatusReportTimeoutSeconds
	// is rejected. It is a pointer so that an omitted key differs from 0.
	TimeoutSeconds *int `toml:"timeout_seconds"`
}

// Enabled reports whether status reporting is on.
func (s StatusReport) Enabled() bool {
	return s.TimeoutSeconds == nil || *s.TimeoutSeconds > 0
}

// Timeout is the effective bound on the wait for the runner to come online. It
// is meaningful only when Enabled.
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
