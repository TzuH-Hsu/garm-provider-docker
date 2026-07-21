package config

import "fmt"

// Flavor is one named [flavors.<name>] entry (ADR-002/ADR-005): a pool
// selects a flavor by name via extra_specs (WP2/WP3, M3's schema
// validation), and each of its three fields, when set, overrides the
// corresponding config-wide default for that pool. All three are optional;
// an unset field falls through to the config-wide value — see
// Config.EffectiveRunnerMemoryBytes, Config.EffectiveDindMemoryBytes
// (resources.go), and Config.EffectiveRunnerImage below.
type Flavor struct {
	// RunnerMemory overrides [resources].runner_memory for pools that
	// select this flavor. Same human-readable byte-size shape.
	RunnerMemory string `toml:"runner_memory"`

	// DindMemory overrides [resources].dind_memory for pools that select
	// this flavor.
	DindMemory string `toml:"dind_memory"`

	// RunnerImage overrides the top-level runner_image for pools that
	// select this flavor. This is the ONLY way a pool can vary the runner
	// image (ADR-002's owner ruling, restated in ADR-005) — no
	// extra_specs field accepts a raw image reference.
	RunnerImage string `toml:"runner_image"`
}

// validate checks a flavor's own fields: RunnerMemory/DindMemory (if set)
// must be well-formed byte sizes, and RunnerImage (if set) must be
// digest-pinned under the SAME allowUnpinnedRunnerImage escape hatch the
// top-level runner_image uses — a flavor's image override is not a
// separate trust tier from the top-level one, so it gets no escape hatch
// of its own (unlike DindImage, which is a genuinely different image with
// its own risk profile — see Config.AllowUnpinnedDindImage's doc comment).
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

// EffectiveRunnerImage returns the runner image a caller (WP2/WP3's
// topology layer) should pull/run for flavorName: the flavor's own
// runner_image when set, else this config's top-level runner_image. An
// empty or unknown flavorName falls through to the top-level image, same
// as EffectiveRunnerMemoryBytes/EffectiveDindMemoryBytes (resources.go).
func (c Config) EffectiveRunnerImage(flavorName string) string {
	if fl, ok := c.Flavors[flavorName]; ok && fl.RunnerImage != "" {
		return fl.RunnerImage
	}
	return c.RunnerImage
}
