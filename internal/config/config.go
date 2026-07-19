// Package config loads and validates this provider's TOML configuration
// file (ADR-005).
package config

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// defaultDockerHost is used when the config file omits docker_host.
const defaultDockerHost = "unix:///var/run/docker.sock"

// Config is the M0-minimal subset of the provider config schema drafted in
// ADR-005: only docker_host and runner_image. The full schema (dind_mode,
// allowed_dind_modes, storage_driver, [resources], [cache], [network],
// [flavors.*], the extra_specs reserved-env denylist, ...) lands in later
// milestones. Unknown TOML keys are tolerated rather than rejected — see
// Load — so an operator's full ADR-005-shaped config file can already be
// pointed at this M0 build without a parse error.
//
// TODO(M3): switch to strict/schema-validated parsing against ADR-005's
// go:embed'ed JSON Schema once the full config surface lands.
type Config struct {
	// DockerHost is the address of the Docker daemon this provider talks
	// to. Defaults to the local Unix socket when omitted or empty.
	DockerHost string `toml:"docker_host"`

	// RunnerImage is the image reference used for CreateInstance. It is
	// required and has no default: image provenance must always be an
	// explicit operator choice (ADR-002), never an implicit one.
	RunnerImage string `toml:"runner_image"`
}

// Load reads the TOML config file at path, applies defaults, and validates
// the result. Unknown keys in the file are ignored rather than rejected,
// so this M0-minimal parser stays forward-compatible with the full
// ADR-005 schema.
func Load(path string) (Config, error) {
	cfg := Config{
		DockerHost: defaultDockerHost,
	}

	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("failed to parse config file %q: %w", path, err)
	}

	// An explicit empty string in the file (docker_host = "") is treated
	// the same as an omitted key, not as "no docker host".
	if cfg.DockerHost == "" {
		cfg.DockerHost = defaultDockerHost
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Validate checks the M0-minimal invariants.
func (c Config) Validate() error {
	if c.RunnerImage == "" {
		return fmt.Errorf("runner_image is required")
	}
	return nil
}
