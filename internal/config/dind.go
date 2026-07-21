package config

import (
	"fmt"
	"slices"
)

// DinD mode values (ADR-001). "none" additionally means "no DinD sidecar
// resources at all" everywhere else in this provider. internal/spec
// duplicates these three literal strings (see mounts.go's
// DindRuntimeSelection) rather than importing this package, to keep
// package spec free of a dependency on package config — mirroring
// internal/docker/fake.go's existing duplication of spec.CredentialDir as
// credentialTarTargetDir for the same layering reason.
const (
	DindModeNone              = "none"
	DindModePrivilegedSidecar = "privileged-sidecar"
	DindModeSysboxRunc        = "sysbox-runc"
)

// defaultStorageDriver is dockerd's explicit --storage-driver flag inside
// the DinD sidecar when the operator does not override it (ADR-001).
const defaultStorageDriver = "overlay2"

// allDindModes is every valid dind_mode/allowed_dind_modes entry, and the
// default value of AllowedDindModes when the config file omits that key
// (ADR-001: "an operator who never touches the config should get the
// previously-documented default behavior" — i.e. every mode allowed).
var allDindModes = []string{DindModeNone, DindModePrivilegedSidecar, DindModeSysboxRunc}

// validateDindMode checks DindMode, AllowedDindModes, and (when DindMode
// requires a sidecar) DindImage together, since ADR-001's operator ceiling
// is a relationship between DindMode and AllowedDindModes, not a property
// of either field alone.
func (c Config) validateDindMode() error {
	if !slices.Contains(allDindModes, c.DindMode) {
		return fmt.Errorf("dind_mode %q is invalid: must be one of %v", c.DindMode, allDindModes)
	}
	for _, m := range c.AllowedDindModes {
		if !slices.Contains(allDindModes, m) {
			return fmt.Errorf("allowed_dind_modes contains invalid mode %q: must be one of %v", m, allDindModes)
		}
	}
	if !slices.Contains(c.AllowedDindModes, c.DindMode) {
		return fmt.Errorf("dind_mode %q is not in allowed_dind_modes %v", c.DindMode, c.AllowedDindModes)
	}

	if c.DindMode != DindModeNone && c.DindImage == "" {
		return fmt.Errorf("dind_image is required when dind_mode is %q", c.DindMode)
	}
	// Even in "none" mode, a dind_image the operator DID set is still
	// validated, so a typo'd digest is caught at load time regardless of
	// mode rather than surfacing only if/when the operator later switches
	// dind_mode away from "none".
	if c.DindImage != "" {
		if err := validateDigestPinnedImage("dind_image", c.DindImage, c.AllowUnpinnedDindImage); err != nil {
			return err
		}
	}
	return nil
}

// EffectiveDindMode returns the dind_mode a caller (WP4's provider create
// path) should use for this allocation, and ERRORS if the result would fall
// outside AllowedDindModes — ADR-001's F7 operator ceiling made a
// first-class, explicitly enforced invariant here, not merely a byproduct of
// Validate. Validate already enforces this for the config file's own
// dind_mode default at Load time (validateDindMode above); this method is
// the SAME check re-run defensively on the create path, so a hand-built
// Config that skipped Validate (or, later, a per-pool override) still fails
// closed rather than silently escalating past the ceiling.
//
// poolMode is reserved for M3's per-pool extra_specs dind_mode selection
// (ADR-005), deferred exactly like flavor selection (EffectiveRunnerImage,
// EffectiveRunnerMemoryBytes/EffectiveDindMemoryBytes above and in
// resources.go take a flavorName WP4 does not yet thread a dind_mode
// analogue of). Once M3 wires a pool's requested mode through here, this
// same call is what bounds it by AllowedDindModes — the ceiling's ONE
// enforcement point, so M3 needs no additional check of its own (see
// ADR-005). Empty poolMode (all of M1) falls through to DindMode, the
// config-wide default/fallback.
//
// An empty AllowedDindModes is treated as "no ceiling configured" rather
// than "nothing allowed": Load always populates it (defaulting to every
// mode when the config file omits allowed_dind_modes, ADR-001), so an empty
// slice here only ever comes from a hand-built Config that bypassed Load —
// exactly the shape most of this package's own tests (and WP1-WP3's
// provider tests) construct. Treating that as unrestricted, rather than as
// an implicit "allow nothing", keeps this method's introduction from
// silently breaking every test that predates it.
func (c Config) EffectiveDindMode(poolMode string) (string, error) {
	mode := c.DindMode
	if poolMode != "" {
		mode = poolMode
	}
	if len(c.AllowedDindModes) == 0 {
		return mode, nil
	}
	if !slices.Contains(c.AllowedDindModes, mode) {
		return "", fmt.Errorf("dind_mode %q is not within allowed_dind_modes %v", mode, c.AllowedDindModes)
	}
	return mode, nil
}

// validateStorageDriver checks StorageDriver against ADR-001's two
// supported values.
func (c Config) validateStorageDriver() error {
	switch c.StorageDriver {
	case "overlay2", "vfs":
		return nil
	default:
		return fmt.Errorf("storage_driver %q is invalid: must be \"overlay2\" or \"vfs\"", c.StorageDriver)
	}
}
