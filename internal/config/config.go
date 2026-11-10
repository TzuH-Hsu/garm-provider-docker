// Package config loads and validates the provider's TOML configuration file.
// The keys are described in docs/config-reference.md.
package config

import (
	// Registers sha256 with go-digest, so digest validation also works in a
	// binary that does not otherwise link it.
	_ "crypto/sha256"
	"fmt"

	"github.com/BurntSushi/toml"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// defaultDockerHost is used when the config file omits docker_host.
const defaultDockerHost = "unix:///var/run/docker.sock"

// Config is the operator tier of the provider's configuration (ADR-005). Load
// ignores unknown TOML keys and does not validate against the published JSON
// Schema (see JSONSchema).
type Config struct {
	DockerHost string `toml:"docker_host"`

	// RunnerImage has no default: the operator must choose the image (ADR-002).
	// A flavor's RunnerImage overrides it and is the only way a pool can vary
	// the runner image.
	RunnerImage string `toml:"runner_image"`

	// AllowUnpinnedRunnerImage is for development only. It relaxes the digest
	// rule for RunnerImage and every flavor's RunnerImage.
	AllowUnpinnedRunnerImage bool `toml:"allow_unpinned_runner_image"`

	// DindMode is the default for a pool that picks none. It must be in
	// AllowedDindModes.
	DindMode string `toml:"dind_mode"`

	// AllowedDindModes is the operator ceiling on every pool's mode. It defaults
	// to ["none"] (see defaultAllowedDindModes).
	AllowedDindModes []string `toml:"allowed_dind_modes"`

	// DindImage is required when DindMode is not "none".
	DindImage string `toml:"dind_image"`

	// AllowUnpinnedDindImage is separate from AllowUnpinnedRunnerImage so that
	// relaxing one image's pinning does not relax the other's.
	AllowUnpinnedDindImage bool `toml:"allow_unpinned_dind_image"`

	// JobHooks is the default job-hook mode: what the runner image's job-started
	// hook does when a check finds damage (fail, warn or off). A pool's
	// extra_specs.job_hooks overrides it. Empty means JobHooksFail.
	JobHooks string `toml:"job_hooks"`

	// StorageDriver is always passed to the sidecar's dockerd, never
	// autodetected: autodetection is unreliable inside the sidecar on some host
	// filesystems.
	StorageDriver string `toml:"storage_driver"`

	// RegistryMirrors and InsecureRegistries configure the sidecar's dockerd
	// (issue #26; see registry.go). They are config-only and global: a mirror
	// belongs to the Docker host's network, not to a pool, so no extra_specs key
	// or flavor field carries them.
	RegistryMirrors    []string `toml:"registry_mirrors"`
	InsecureRegistries []string `toml:"insecure_registries"`

	Resources Resources `toml:"resources"`
	Network   Network   `toml:"network"`

	// Flavors are named per-pool overrides, selected by extra_specs.flavor.
	Flavors map[string]Flavor `toml:"flavors"`

	Cache        Cache            `toml:"cache"`
	ExtraSpecs   ExtraSpecsPolicy `toml:"extra_specs"`
	StatusReport StatusReport     `toml:"status_report"`
	Logging      Logging          `toml:"logging"`
}

// Load reads the TOML config file at path, applies defaults and validates the
// result.
func Load(path string) (Config, error) {
	cfg := Config{
		DockerHost:       defaultDockerHost,
		DindMode:         DindModeNone,
		AllowedDindModes: append([]string(nil), defaultAllowedDindModes...),
		JobHooks:         JobHooksFail,
		StorageDriver:    defaultStorageDriver,
		Network: Network{
			EnableJobNetwork: true,
			Internal:         false,
		},
		Cache: defaultCache(),
	}

	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("failed to parse config file %q: %w", path, err)
	}

	// docker_host = "" means the same as an omitted key.
	if cfg.DockerHost == "" {
		cfg.DockerHost = defaultDockerHost
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Validate checks every invariant of c, including a Config built without Load.
func (c Config) Validate() error {
	if c.RunnerImage == "" {
		return fmt.Errorf("runner_image is required")
	}
	if err := validateDigestPinnedImage("runner_image", c.RunnerImage, c.AllowUnpinnedRunnerImage); err != nil {
		return err
	}

	if err := c.validateDindMode(); err != nil {
		return err
	}
	if err := c.validateJobHooks(); err != nil {
		return err
	}
	if err := c.validateStorageDriver(); err != nil {
		return err
	}
	if err := c.validateRegistryMirrors(); err != nil {
		return err
	}
	if err := c.validateInsecureRegistries(); err != nil {
		return err
	}
	if err := c.Resources.validate(); err != nil {
		return fmt.Errorf("[resources]: %w", err)
	}
	for name, fl := range c.Flavors {
		if err := fl.validate(c.AllowUnpinnedRunnerImage); err != nil {
			return fmt.Errorf("flavor %q: %w", name, err)
		}
	}
	if err := c.Cache.validate(); err != nil {
		return fmt.Errorf("[cache]: %w", err)
	}
	if err := c.StatusReport.validate(); err != nil {
		return fmt.Errorf("[status_report]: %w", err)
	}

	return nil
}

// validateDigestPinnedImage requires ref, the value of fieldName, to be a
// name@sha256:<64-hex> reference so the image bytes are reproducible. A
// tag-only reference passes only when allowUnpinned is set; a malformed one
// never does.
func validateDigestPinnedImage(fieldName, ref string, allowUnpinned bool) error {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return fmt.Errorf("%s %q is not a valid image reference: %w", fieldName, ref, err)
	}
	canonical, ok := named.(reference.Canonical)
	if !ok {
		if allowUnpinned {
			return nil
		}
		return fmt.Errorf("%s %q must be pinned by digest (name@sha256:<64-hex>); set the corresponding allow_unpinned_*_image flag to override in development", fieldName, ref)
	}
	if d := canonical.Digest(); d.Algorithm() != digest.SHA256 || len(d.Encoded()) != 64 {
		return fmt.Errorf("%s %q must be pinned by a sha256 digest", fieldName, ref)
	}
	return nil
}
