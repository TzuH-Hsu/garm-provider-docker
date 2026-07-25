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

// allDindModes is every VALID dind_mode/allowed_dind_modes entry. It is the
// vocabulary of the two fields, not the default of either.
var allDindModes = []string{DindModeNone, DindModePrivilegedSidecar, DindModeSysboxRunc}

// defaultAllowedDindModes is the operator ceiling applied when the config
// file omits allowed_dind_modes: ["none"], i.e. fail-closed — no DinD mode
// can be selected at all until the operator explicitly widens the ceiling.
//
// This deliberately does NOT match allDindModes. The earlier all-three
// default was inconsistent with the fail-closed posture of every other
// default in this provider (dind_mode itself defaults to "none", and
// [extra_specs].allowed_env defaults to empty): it meant a minimal config —
// one with no dind_image at all — still permitted a pool's extra_specs to
// select privileged-sidecar, standing up a Privileged=true daemon the
// operator never opted into, and failing only once Docker work had already
// begun. A ceiling whose whole purpose is "the operator gets the final word"
// (ADR-001 F7) must not grant the most dangerous mode by omission.
var defaultAllowedDindModes = []string{DindModeNone}

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
// An empty AllowedDindModes FAILS CLOSED (denies every mode) rather than being
// treated as "unrestricted" (F10). A loaded config always carries a non-empty
// ceiling (Load populates defaultAllowedDindModes when the file omits
// allowed_dind_modes, and Load's Validate rejects an explicitly empty list
// because DindMode could not be a member of it), so an empty slice here can
// only reach this method
// from a hand-built Config that bypassed Load — exactly the "bypassing caller"
// case a fail-open ceiling would silently let permit every mode. Provider
// construction independently rejects an empty/malformed ceiling
// (Config.ValidateAllowedDindModes, called from provider.New), so in practice
// this branch is a belt-and-braces last line rather than the only guard.
func (c Config) EffectiveDindMode(poolMode string) (string, error) {
	mode := c.DindMode
	if poolMode != "" {
		mode = poolMode
	}
	if len(c.AllowedDindModes) == 0 {
		return "", fmt.Errorf("dind_mode %q is denied: allowed_dind_modes is empty (an empty ceiling denies every mode; a loaded config always has at least the %v default)", mode, defaultAllowedDindModes)
	}
	if !slices.Contains(c.AllowedDindModes, mode) {
		return "", fmt.Errorf("dind_mode %q is not within allowed_dind_modes %v", mode, c.AllowedDindModes)
	}
	return mode, nil
}

// ValidateAllowedDindModes checks the operator ceiling is WELL-FORMED — a
// non-empty list containing only valid modes (F10). An empty ceiling is
// rejected here (never treated as fail-open "allow everything"), and any
// invalid entry is rejected. This is the check provider.New runs at
// construction so a hand-built or Load-bypassing caller cannot silently permit
// every mode; it deliberately does NOT assert DindMode ∈ AllowedDindModes
// (that relationship is enforced by Load's Validate and, defensively on every
// create, by EffectiveDindMode).
func (c Config) ValidateAllowedDindModes() error {
	if len(c.AllowedDindModes) == 0 {
		return fmt.Errorf("allowed_dind_modes must not be empty (an empty ceiling denies every mode; set it to a non-empty subset of %v)", allDindModes)
	}
	for _, m := range c.AllowedDindModes {
		if !slices.Contains(allDindModes, m) {
			return fmt.Errorf("allowed_dind_modes contains invalid mode %q: must be one of %v", m, allDindModes)
		}
	}
	return nil
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
