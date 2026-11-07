package config

import "fmt"

// Resources is the [resources] table: memory limits only. CPU and PID limits are
// not offered (ADR-005).
type Resources struct {
	// RunnerMemory is the runner container's limit, a byte size (see
	// ParseByteSize). Empty means unlimited.
	RunnerMemory string `toml:"runner_memory"`

	// DindMemory is the sidecar's limit. It is unused when dind_mode is "none".
	DindMemory string `toml:"dind_memory"`
}

// validate checks that the sizes that are set parse. Empty is valid.
func (r Resources) validate() error {
	if r.RunnerMemory != "" {
		if _, err := ParseByteSize(r.RunnerMemory); err != nil {
			return fmt.Errorf("runner_memory %q: %w", r.RunnerMemory, err)
		}
	}
	if r.DindMemory != "" {
		if _, err := ParseByteSize(r.DindMemory); err != nil {
			return fmt.Errorf("dind_memory %q: %w", r.DindMemory, err)
		}
	}
	return nil
}

// runnerMemoryBytes returns RunnerMemory in bytes, or 0 (unlimited) when unset.
// It can fail only for a Resources that skipped validate.
func (r Resources) runnerMemoryBytes() (int64, error) {
	if r.RunnerMemory == "" {
		return 0, nil
	}
	return ParseByteSize(r.RunnerMemory)
}

func (r Resources) dindMemoryBytes() (int64, error) {
	if r.DindMemory == "" {
		return 0, nil
	}
	return ParseByteSize(r.DindMemory)
}

// EffectiveRunnerMemoryBytes returns the flavor's runner_memory in bytes, else
// [resources].runner_memory; 0 means unlimited. An empty or unknown flavorName
// uses the config-wide value: checking the name is internal/extraspecs' job.
func (c Config) EffectiveRunnerMemoryBytes(flavorName string) (int64, error) {
	if fl, ok := c.Flavors[flavorName]; ok && fl.RunnerMemory != "" {
		return ParseByteSize(fl.RunnerMemory)
	}
	return c.Resources.runnerMemoryBytes()
}

// EffectiveDindMemoryBytes is EffectiveRunnerMemoryBytes for the sidecar's limit.
func (c Config) EffectiveDindMemoryBytes(flavorName string) (int64, error) {
	if fl, ok := c.Flavors[flavorName]; ok && fl.DindMemory != "" {
		return ParseByteSize(fl.DindMemory)
	}
	return c.Resources.dindMemoryBytes()
}
