package config

import (
	"fmt"
	"slices"
)

// Job-hook modes: what the runner image's job-started hook does when one of its
// checks finds damage (ADR-002). The provider passes the resolved value to the
// runner as GARM_JOB_HOOKS.
const (
	// JobHooksFail stops the job before its first step. It is the default.
	JobHooksFail = "fail"
	// JobHooksWarn prints the finding and lets the job run.
	JobHooksWarn = "warn"
	// JobHooksOff skips the checks.
	JobHooksOff = "off"
)

// allJobHooksModes is every valid job_hooks value, in the order the docs list
// them. extra_specs repeats it as an enum in its JSON schema.
var allJobHooksModes = []string{JobHooksFail, JobHooksWarn, JobHooksOff}

// validateJobHooks accepts the three modes and the empty string, which means
// the default, like an omitted key. There is no ceiling in allowed_*: a pool
// that turns a check off gains no privilege, it only loses a safety net.
func (c Config) validateJobHooks() error {
	if c.JobHooks != "" && !slices.Contains(allJobHooksModes, c.JobHooks) {
		return fmt.Errorf("job_hooks %q is invalid: must be one of %v", c.JobHooks, allJobHooksModes)
	}
	return nil
}

// EffectiveJobHooks returns poolMode, or JobHooks when poolMode is empty, or
// JobHooksFail when both are. The values are validated before this is called
// (Config.Validate, extraspecs.Parse).
func (c Config) EffectiveJobHooks(poolMode string) string {
	switch {
	case poolMode != "":
		return poolMode
	case c.JobHooks != "":
		return c.JobHooks
	default:
		return JobHooksFail
	}
}
