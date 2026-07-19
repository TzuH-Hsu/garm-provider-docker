// Package config loads and validates this provider's TOML configuration
// file (ADR-005).
package config

import (
	// crypto/sha256 is imported for its side effect: it registers the sha256
	// algorithm with opencontainers/go-digest, so runner_image digest
	// validation works even in a build (e.g. this package's isolated unit
	// test binary) that would not otherwise link a sha256 implementation.
	_ "crypto/sha256"
	"fmt"

	"github.com/BurntSushi/toml"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
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
	// explicit operator choice (ADR-002), never an implicit one. It must be
	// pinned by a sha256 digest (name@sha256:<64-hex>) unless
	// AllowUnpinnedRunnerImage is set.
	RunnerImage string `toml:"runner_image"`

	// AllowUnpinnedRunnerImage is a dev-only escape hatch. When false (the
	// default) runner_image MUST be digest-pinned, so image provenance is
	// reproducible and cannot drift under a mutable tag (ADR-002/ADR-005
	// F10). Set it true ONLY for local development against a tag; never in a
	// real deployment.
	AllowUnpinnedRunnerImage bool `toml:"allow_unpinned_runner_image"`
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
	if err := validateRunnerImage(c.RunnerImage, c.AllowUnpinnedRunnerImage); err != nil {
		return err
	}
	return nil
}

// validateRunnerImage requires runner_image to be a canonical, digest-pinned
// image reference (name@sha256:<64-hex>), so the exact image bytes are
// reproducible (ADR-002/ADR-005 F10). A malformed reference is always
// rejected. A valid tag-only reference is rejected unless allowUnpinned is
// set (the dev-only escape hatch).
func validateRunnerImage(ref string, allowUnpinned bool) error {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return fmt.Errorf("runner_image %q is not a valid image reference: %w", ref, err)
	}
	canonical, ok := named.(reference.Canonical)
	if !ok {
		if allowUnpinned {
			return nil
		}
		return fmt.Errorf("runner_image %q must be pinned by digest (name@sha256:<64-hex>); set allow_unpinned_runner_image = true to override in development", ref)
	}
	if d := canonical.Digest(); d.Algorithm() != digest.SHA256 || len(d.Encoded()) != 64 {
		return fmt.Errorf("runner_image %q must be pinned by a sha256 digest", ref)
	}
	return nil
}
