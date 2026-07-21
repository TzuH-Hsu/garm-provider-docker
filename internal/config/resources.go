package config

import "fmt"

// Resources is the [resources] table (ADR-005): memory limits only. No cpu
// or pids limits, by design (see ADR-005's illustrative config and
// Rationale — this provider deliberately does not expose them).
type Resources struct {
	// RunnerMemory is the runner container's memory limit, a human-readable
	// byte size (see ParseByteSize). Empty means unlimited: this provider
	// imposes no hard default limit of its own. ADR-005's illustrative
	// config always sets a value (e.g. "8GiB"), but neither ADR-001 nor
	// ADR-005 mandates a specific default magnitude when the operator
	// omits it, so "unlimited unless the operator opts in" was chosen to
	// preserve M0's existing unlimited default rather than picking an
	// arbitrary number — flagged in the WP1 report as a place ADR-005
	// leaves open (also listed in plan.md §5's open questions, in the
	// adjacent context of extra_specs memory clamping).
	RunnerMemory string `toml:"runner_memory"`

	// DindMemory is the DinD sidecar's memory limit, same shape as
	// RunnerMemory. Unused when dind_mode is "none".
	DindMemory string `toml:"dind_memory"`
}

// validate parses RunnerMemory/DindMemory (when set) to confirm they are
// well-formed byte sizes. It does not decide whether "unset" (unlimited) is
// acceptable — an operator who wants a hard cap sets one; an operator who
// does not gets M0's existing unlimited behavior (see RunnerMemory's doc
// comment).
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

// runnerMemoryBytes returns RunnerMemory parsed to bytes, or 0 (unlimited)
// when unset. The error return only matters for a hand-built Resources
// that skipped Validate (e.g. in a test); Load's contract already
// guarantees this cannot fail for a Config that passed Validate.
func (r Resources) runnerMemoryBytes() (int64, error) {
	if r.RunnerMemory == "" {
		return 0, nil
	}
	return ParseByteSize(r.RunnerMemory)
}

// dindMemoryBytes is runnerMemoryBytes's DinD-memory counterpart.
func (r Resources) dindMemoryBytes() (int64, error) {
	if r.DindMemory == "" {
		return 0, nil
	}
	return ParseByteSize(r.DindMemory)
}

// EffectiveRunnerMemoryBytes returns the runner memory limit a caller
// (WP2/WP3's topology layer) should apply for flavorName: the named
// flavor's own runner_memory when it set one, else this config's
// [resources].runner_memory default. An empty flavorName, or one naming no
// configured flavor, falls straight through to the config-wide default —
// flavor-name validity against a pool's extra_specs is a schema-validation
// concern (ADR-005, M3), not this method's. This is how internal/spec's
// RunnerContainerSpec.MemoryBytes (mounts.go) is meant to be populated,
// without internal/spec itself depending on package config.
func (c Config) EffectiveRunnerMemoryBytes(flavorName string) (int64, error) {
	if fl, ok := c.Flavors[flavorName]; ok && fl.RunnerMemory != "" {
		return ParseByteSize(fl.RunnerMemory)
	}
	return c.Resources.runnerMemoryBytes()
}

// EffectiveDindMemoryBytes is EffectiveRunnerMemoryBytes's DinD-memory
// counterpart.
func (c Config) EffectiveDindMemoryBytes(flavorName string) (int64, error) {
	if fl, ok := c.Flavors[flavorName]; ok && fl.DindMemory != "" {
		return ParseByteSize(fl.DindMemory)
	}
	return c.Resources.dindMemoryBytes()
}
