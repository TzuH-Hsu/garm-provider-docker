package config

// Network is the [network] table: per-job bridge network behavior (ADR-001).
type Network struct {
	// EnableJobNetwork is reserved and not honored: every allocation gets its own
	// network, because it is the per-job isolation and the claim marker for
	// duplicate-create detection (ADR-004). false only logs a warning.
	EnableJobNetwork bool `toml:"enable_job_network"`

	// Internal sets Docker's internal flag, which leaves a job with no route out
	// at all, GitHub and registry pulls included. It defaults to false because
	// job-to-job isolation comes from each allocation's own network, not from
	// this flag (ADR-001, "Job network"). Config-only, never an extra_specs key.
	Internal bool `toml:"internal"`
}
