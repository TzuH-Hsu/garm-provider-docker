package spec

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// RunnerContainerName returns the runner container's name: the bootstrap
// Name, lowercased.
//
// GARM's own instance names are already Docker-name-safe in practice, but
// they are not guaranteed to be lowercase, and Docker container/volume/
// network names are case-sensitive — two allocations whose names differ
// only by case would otherwise be distinct resources on the same host.
// Lowercasing here, once, keeps every derived name in this file
// (DindContainerName, JobNetworkName, ...) deterministic and collision-
// free with respect to case alone.
func RunnerContainerName(instanceName string) string {
	return strings.ToLower(instanceName)
}

// DindContainerName returns the DinD sidecar container's deterministic
// name for a given instance (ADR-001).
func DindContainerName(instanceName string) string {
	return RunnerContainerName(instanceName) + "-dind"
}

// JobNetworkName returns the per-job network's deterministic name
// (ADR-001, ADR-004's claim marker). It deliberately depends ONLY on the
// instance name — never on the create-nonce — because the network IS the
// claim marker: its STABILITY per instance name is what makes NetworkCreate's
// unconditional 409-on-duplicate the atomic dedup primitive (ADR-004). Two
// concurrent same-instance creates must collide on this one name; a
// generation-unique network name would defeat that. The per-generation
// VOLUMES below are the ones that carry the nonce (F4).
func JobNetworkName(instanceName string) string {
	return RunnerContainerName(instanceName) + "-net"
}

// WorkspaceVolumeName returns the per-job workspace volume's
// GENERATION-UNIQUE name (ADR-001, F4): <instance>-<nonce>-workspace, where
// nonce is this CreateInstance attempt's create-nonce. Embedding the nonce is
// the STRUCTURAL fix for the name-based-teardown TOCTOU (ADR-004 amendment
// 2026-07-21): because every generation's volume name is distinct, a stale
// teardown that removes a volume by the name it read from its own label-scoped
// list can never name-collide with a DIFFERENT generation's freshly-created
// volume — the two simply do not share a name. Unlike the network (the stable
// claim marker), volumes are never a dedup primitive, so making their names
// generation-unique costs nothing and closes the hazard by construction.
func WorkspaceVolumeName(instanceName, nonce string) string {
	return volumeName(instanceName, nonce, "workspace")
}

// SocketVolumeName returns the per-job DinD socket volume's GENERATION-UNIQUE
// name (ADR-001, F4): <instance>-<nonce>-socket. See WorkspaceVolumeName for
// why the nonce is embedded.
func SocketVolumeName(instanceName, nonce string) string {
	return volumeName(instanceName, nonce, "socket")
}

// DindStateVolumeName returns the per-job dind-state volume's
// GENERATION-UNIQUE name (ADR-001, F4): <instance>-<nonce>-dind-state. See
// WorkspaceVolumeName for why the nonce is embedded.
func DindStateVolumeName(instanceName, nonce string) string {
	return volumeName(instanceName, nonce, "dind-state")
}

// volumeName assembles a generation-unique job-scoped volume name from the
// lowercased instance name, the create-nonce, and the resource suffix (F4).
// The create-nonce is a fixed-length lowercase-hex string (crypto/rand, see
// provider.newCreateNonce), so the result stays DNS/Docker-name-safe —
// [a-z0-9.-] only — for any instance name that is itself charset-safe and
// short enough to stay within Docker's 255-char name limit once the nonce and
// suffix are appended; ValidateAllocationNames below is what actually
// verifies that, rather than this function merely assuming it.
func volumeName(instanceName, nonce, suffix string) string {
	return RunnerContainerName(instanceName) + "-" + nonce + "-" + suffix
}

// dockerNameMaxLength is the Docker daemon's own maximum length for a
// container, network, or volume name.
const dockerNameMaxLength = 255

// dockerNamePattern is Docker's resource-name grammar: a leading
// alphanumeric, followed by any number of alphanumerics, underscores, dots,
// or dashes ([a-zA-Z0-9][a-zA-Z0-9_.-]*). It is the same grammar the daemon
// itself enforces for container, network, and volume names.
var dockerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ValidateDerivedName reports whether name — a container/network/volume name
// this package derived from a GARM-supplied instance name — is safe to hand
// to the Docker daemon: non-empty, within dockerNameMaxLength, and matching
// dockerNamePattern. kind is a human-readable label (e.g. "workspace volume")
// used only to make a returned error's message specific about which derived
// name failed.
//
// This exists because every builder in this file assembles a name from a
// caller-supplied instance name plus fixed ASCII literals and (for the three
// volume names) a fixed-length hex nonce — none of which this package
// controls the LENGTH of on the instance-name side. GARM's own instance names
// are Docker-name-safe "in practice" (RunnerContainerName's doc), but nothing
// upstream actually bounds their length, and the nonce-qualified volume names
// (F4) add a fixed ~44 bytes on top (1 dash + a 32-hex-char nonce + 1 dash +
// the longest suffix, "dind-state", 10 chars). A pathologically long instance
// name could therefore push a derived volume name past Docker's 255-byte
// ceiling — without this check, that surfaces as an opaque daemon rejection
// deep inside VolumeCreate/NetworkCreate/ContainerCreate, well after the
// claim network (and possibly other resources) already exist. Called via
// ValidateAllocationNames, early in provider.CreateInstance and before this
// attempt's first Docker call, so an over-long or charset-unsafe instance
// name instead fails immediately with one clear, provider-authored error.
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

// ValidateAllocationNames validates every name this package would derive for
// one CreateInstance attempt — the runner container, the DinD sidecar
// container, the job network, and the three generation-unique volumes
// (workspace, socket, dind-state) — against ValidateDerivedName. Call it once,
// early, right after nonce generation and before this attempt's first Docker
// call (CreateClaimNetwork), so a pathologically long (or otherwise
// Docker-name-unsafe) instanceName fails with one clear, aggregated provider
// error instead of an opaque daemon rejection surfacing mid-allocation. The
// DinD-only names (sidecar container, socket/dind-state volumes) are
// validated unconditionally, even in "none" mode, since the check is
// pure/side-effect-free and a single early failure is simpler to reason
// about than mode-conditional validation.
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
