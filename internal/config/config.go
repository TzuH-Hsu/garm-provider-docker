// Package config loads and validates this provider's TOML configuration
// file (ADR-005).
package config

import (
	// crypto/sha256 is imported for its side effect: it registers the sha256
	// algorithm with opencontainers/go-digest, so digest-pinned image
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

// Config is this provider's TOML configuration schema (ADR-005). Unknown
// TOML keys are tolerated rather than rejected — see Load — so a config
// file may carry keys from a newer or older provider version without a
// parse error.
//
// A published, go:embed'ed JSON Schema for this config is exposed for
// self-documentation via JSONSchema() (schema.go / schema.json), returned by
// the v0.1.1 GetConfigJSONSchema command (M3-W1). The loader still tolerates
// unknown TOML keys and does not validate the file against that schema. The
// extra_specs reserved-env denylist and schema validation landed in M3-W1 too,
// but live in package internal/extraspecs (the extra_specs trust tier), not
// here — this struct is the operator-config tier.
type Config struct {
	// DockerHost is the address of the Docker daemon this provider talks
	// to. Defaults to the local Unix socket when omitted or empty.
	DockerHost string `toml:"docker_host"`

	// RunnerImage is the image reference used for CreateInstance. It is
	// required and has no default: image provenance must always be an
	// explicit operator choice (ADR-002), never an implicit one. It must be
	// pinned by a sha256 digest (name@sha256:<64-hex>) unless
	// AllowUnpinnedRunnerImage is set. A pool may override this per-flavor
	// (Flavors below) — that is the ONLY channel that varies the runner
	// image; this field is the fallback when a flavor sets none.
	RunnerImage string `toml:"runner_image"`

	// AllowUnpinnedRunnerImage is a dev-only escape hatch. When false (the
	// default) runner_image (and any flavor's own RunnerImage override)
	// MUST be digest-pinned, so image provenance is reproducible and cannot
	// drift under a mutable tag (ADR-002/ADR-005 F10). Set it true ONLY for
	// local development against a tag; never in a real deployment.
	AllowUnpinnedRunnerImage bool `toml:"allow_unpinned_runner_image"`

	// DindMode selects this operator's default DinD sidecar strategy
	// (ADR-001): "none" (default — no DinD sidecar is ever created; M0's
	// only mode), "privileged-sidecar", or "sysbox-runc". A pool's
	// extra_specs may request a narrower mode at CreateInstance time
	// (WP2/WP3, ADR-005); this is the fallback used when it does not, and
	// in every case the effective mode is bounded by AllowedDindModes.
	DindMode string `toml:"dind_mode"`

	// AllowedDindModes is the operator ceiling on DindMode (ADR-001's F7,
	// "the operator gets the final word"): the set of modes ANY pool's
	// extra_specs may ever select on this host, regardless of what a pool
	// or DindMode itself requests. Defaults to all three modes when the
	// config file omits this key, matching ADR-001's documented default
	// ("an operator who never touches the config should get the
	// previously-documented default behavior"). DindMode must always be a
	// member of this list — see Validate.
	AllowedDindModes []string `toml:"allowed_dind_modes"`

	// DindImage is the DinD sidecar's image reference (ADR-001/ADR-002),
	// e.g. "docker:dind@sha256:...". Required, and validated as
	// digest-pinned exactly like RunnerImage, only when DindMode != "none";
	// may be left empty when DindMode is "none" since no sidecar is ever
	// created in that mode.
	DindImage string `toml:"dind_image"`

	// AllowUnpinnedDindImage is DindImage's own dev-only escape hatch. It
	// is deliberately a SEPARATE flag from AllowUnpinnedRunnerImage rather
	// than the two sharing one: an operator developing against an unpinned
	// runner_image tag should not have to also relax pinning on the
	// separately security-sensitive DinD sidecar image — privileged-sidecar
	// is the default DinD MECHANISM once an operator opts into a DinD mode
	// (ADR-001), but DinD itself defaults to "none" (no sidecar at all,
	// above) — and vice versa. Two independent, narrowly-scoped escape
	// hatches were judged cleaner than one shared flag whose blast radius
	// covers both images — flagged in the WP1 report since ADR-005 left the
	// naming/sharing choice open ("honor the existing
	// allow_unpinned_runner_image flag naming").
	AllowUnpinnedDindImage bool `toml:"allow_unpinned_dind_image"`

	// StorageDriver is the explicit --storage-driver flag WP2/WP3's DinD
	// sidecar entrypoint passes to dockerd (ADR-001): NAS host filesystems
	// make storage-driver autodetection inside the sidecar unreliable, so
	// this is always explicit, never autodetected. "overlay2" (the
	// default) or "vfs" (the slower, more compatible fallback); any other
	// value is rejected.
	StorageDriver string `toml:"storage_driver"`

	// Resources is the [resources] table: runner_memory/dind_memory
	// limits. No cpu or pids limits, by design (ADR-005).
	Resources Resources `toml:"resources"`

	// Network is the [network] table: per-job network behavior (ADR-001).
	Network Network `toml:"network"`

	// Flavors is the [flavors.<name>] map: named, per-pool overrides for
	// runner_memory/dind_memory/runner_image. A named flavor is the ONLY
	// channel that varies the runner image per pool (ADR-002's owner
	// ruling, restated in ADR-005) — no other field, in config or
	// extra_specs, accepts a raw image reference.
	Flavors map[string]Flavor `toml:"flavors"`

	// Cache is the [cache] table: ADR-003's persistent, repo-scoped toolcache
	// and pnpm-store caches (M2-W1). Defaulted to enabled with the ADR-005
	// values by Load; see cache.go.
	Cache Cache `toml:"cache"`
}

// Load reads the TOML config file at path, applies defaults, and validates
// the result. Unknown keys in the file are ignored rather than rejected.
func Load(path string) (Config, error) {
	cfg := Config{
		DockerHost:       defaultDockerHost,
		DindMode:         DindModeNone,
		AllowedDindModes: append([]string(nil), allDindModes...),
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

// Validate checks every top-level invariant. Field-group-specific checks
// (dind_mode/allowed_dind_modes/dind_image, storage_driver, [resources],
// each flavor) are delegated to their own files (dind.go, resources.go,
// flavor.go) to keep this file focused on orchestration.
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
	if err := c.validateStorageDriver(); err != nil {
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

	return nil
}

// validateDigestPinnedImage requires ref (the field named fieldName, for
// error messages) to be a canonical, digest-pinned image reference
// (name@sha256:<64-hex>), so the exact image bytes are reproducible
// (ADR-002/ADR-005 F10). A malformed reference is always rejected. A valid
// tag-only reference is rejected unless allowUnpinned is set. Shared by
// runner_image, dind_image, and each flavor's own runner_image override.
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
