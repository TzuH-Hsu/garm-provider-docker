package config

import (
	"fmt"
	"slices"
)

// DinD mode values (ADR-001). internal/spec repeats these strings so that it
// does not import this package.
const (
	DindModeNone              = "none"
	DindModePrivilegedSidecar = "privileged-sidecar"
	DindModeSysboxRunc        = "sysbox-runc"
)

const defaultStorageDriver = "overlay2"

// allDindModes is every valid dind_mode and allowed_dind_modes entry. It is the
// vocabulary of the two fields, not the default of either.
var allDindModes = []string{DindModeNone, DindModePrivilegedSidecar, DindModeSysboxRunc}

// defaultAllowedDindModes is fail-closed on purpose: a config that omits
// allowed_dind_modes cannot select any DinD mode, so a pool can never obtain a
// privileged sidecar the operator did not opt into (ADR-001). It deliberately
// differs from allDindModes.
var defaultAllowedDindModes = []string{DindModeNone}

// validateDindMode checks DindMode, AllowedDindModes and DindImage together,
// because the operator ceiling is a relationship between the first two.
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
	// A dind_image set under "none" is still checked, so a mistyped digest fails
	// at load and not when the operator later enables DinD.
	if c.DindImage != "" {
		if err := validateDigestPinnedImage("dind_image", c.DindImage, c.AllowUnpinnedDindImage); err != nil {
			return err
		}
	}
	return nil
}

// EffectiveDindMode returns poolMode, or DindMode when poolMode is empty, and
// errors if the result is outside AllowedDindModes. It is the one place a pool's
// requested mode is bounded by the operator ceiling.
//
// An empty AllowedDindModes denies every mode. A loaded config always has a
// non-empty ceiling, so an empty one means a Config built without Load, and
// treating it as unrestricted would let that caller permit every mode.
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

// ValidateAllowedDindModes requires a non-empty ceiling of valid modes.
// provider.New runs it so that a Config built without Load cannot permit every
// mode. It does not check DindMode against the ceiling; Validate and
// EffectiveDindMode do.
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

func (c Config) validateStorageDriver() error {
	switch c.StorageDriver {
	case "overlay2", "vfs":
		return nil
	default:
		return fmt.Errorf("storage_driver %q is invalid: must be \"overlay2\" or \"vfs\"", c.StorageDriver)
	}
}
