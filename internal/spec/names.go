package spec

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// RunnerContainerName returns the bootstrap name, lowercased. Docker names are
// case-sensitive, so this keeps every name derived here free of case-only collisions.
func RunnerContainerName(instanceName string) string {
	return strings.ToLower(instanceName)
}

// DindContainerName returns the DinD sidecar container's deterministic
// name for a given instance (ADR-001).
func DindContainerName(instanceName string) string {
	return RunnerContainerName(instanceName) + "-dind"
}

// JobNetworkName returns the per-job network's name. It must depend only on the
// instance name: NetworkCreate's 409 on a duplicate is ADR-004's atomic claim, so two
// creates of one instance must collide here.
func JobNetworkName(instanceName string) string {
	return RunnerContainerName(instanceName) + "-net"
}

// WorkspaceVolumeName returns <instance>-<nonce>-workspace. The nonce makes the name
// unique per generation, so a stale teardown that removes a volume by name cannot hit
// a newer generation's volume (ADR-004).
func WorkspaceVolumeName(instanceName, nonce string) string {
	return volumeName(instanceName, nonce, "workspace")
}

// SocketVolumeName returns <instance>-<nonce>-socket. See WorkspaceVolumeName.
func SocketVolumeName(instanceName, nonce string) string {
	return volumeName(instanceName, nonce, "socket")
}

// DindStateVolumeName returns <instance>-<nonce>-dind-state. See WorkspaceVolumeName.
func DindStateVolumeName(instanceName, nonce string) string {
	return volumeName(instanceName, nonce, "dind-state")
}

func volumeName(instanceName, nonce, suffix string) string {
	return RunnerContainerName(instanceName) + "-" + nonce + "-" + suffix
}

// dockerNameMaxLength is the longest resource name this provider hands to Docker.
const dockerNameMaxLength = 255

// dockerNamePattern is the character grammar the daemon enforces for container and
// volume names. The daemon also requires a second character, which this does not check.
var dockerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ValidateDerivedName returns an error unless name is non-empty, within
// dockerNameMaxLength and matches dockerNamePattern. kind names the resource in the
// error. Nothing upstream bounds an instance name's length, so without this a long name
// would surface as an opaque daemon rejection after the claim network already exists.
func ValidateDerivedName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("derived %s name is empty", kind)
	}
	if len(name) > dockerNameMaxLength {
		return fmt.Errorf("derived %s name %q is %d bytes, which exceeds Docker's %d-byte resource-name limit — the instance name is too long once this provider's generation-nonce/suffix are appended", kind, name, len(name), dockerNameMaxLength)
	}
	if !dockerNamePattern.MatchString(name) {
		return fmt.Errorf("derived %s name %q is not a valid Docker resource name (must match %s)", kind, name, dockerNamePattern.String())
	}
	return nil
}

// ValidateAllocationNames validates every name one CreateInstance attempt derives and
// joins the errors. Call it before the attempt's first Docker call. DinD-only names are
// checked in "none" mode too, which keeps the check free of mode conditions.
func ValidateAllocationNames(instanceName, nonce string) error {
	checks := []struct {
		kind string
		name string
	}{
		{"runner container", RunnerContainerName(instanceName)},
		{"DinD sidecar container", DindContainerName(instanceName)},
		{"job network", JobNetworkName(instanceName)},
		{"workspace volume", WorkspaceVolumeName(instanceName, nonce)},
		{"socket volume", SocketVolumeName(instanceName, nonce)},
		{"dind-state volume", DindStateVolumeName(instanceName, nonce)},
	}
	var errs []error
	for _, c := range checks {
		if err := ValidateDerivedName(c.kind, c.name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
