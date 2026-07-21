package spec

import "strings"

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
// [a-z0-9.-] only — and well within Docker's 255-char name limit for any
// realistic GARM instance name.
func volumeName(instanceName, nonce, suffix string) string {
	return RunnerContainerName(instanceName) + "-" + nonce + "-" + suffix
}
