package config

import "fmt"

// Flavor is a [flavors.<name>] entry. A pool selects it with extra_specs.flavor,
// and each field that is set overrides the config-wide value for that pool.
type Flavor struct {
	// RunnerMemory overrides [resources].runner_memory.
	RunnerMemory string `toml:"runner_memory"`

	// DindMemory overrides [resources].dind_memory.
	DindMemory string `toml:"dind_memory"`

	// RunnerImage overrides the top-level runner_image. It is the only way a
	// pool can vary the runner image: no extra_specs field takes an image
	// reference (ADR-005).
	RunnerImage string `toml:"runner_image"`
}

// validate checks the fields that are set. RunnerImage uses the top-level
// allowUnpinnedRunnerImage flag, because it is the same trust tier as
// runner_image; only dind_image has a flag of its own.
func (f Flavor) validate(allowUnpinnedRunnerImage bool) error {
	if f.RunnerMemory != "" {
		if _, err := ParseByteSize(f.RunnerMemory); err != nil {
			return fmt.Errorf("runner_memory %q: %w", f.RunnerMemory, err)
		}
	}
	if f.DindMemory != "" {
		if _, err := ParseByteSize(f.DindMemory); err != nil {
			return fmt.Errorf("dind_memory %q: %w", f.DindMemory, err)
		}
	}
	if f.RunnerImage != "" {
		if err := validateDigestPinnedImage("runner_image", f.RunnerImage, allowUnpinnedRunnerImage); err != nil {
			return err
		}
	}
	return nil
}

// EffectiveRunnerImage returns the flavor's runner_image, or the top-level
// runner_image when flavorName is empty, unknown or sets none.
func (c Config) EffectiveRunnerImage(flavorName string) string {
	if fl, ok := c.Flavors[flavorName]; ok && fl.RunnerImage != "" {
		return fl.RunnerImage
	}
	return c.RunnerImage
}
