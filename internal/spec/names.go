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
// (ADR-001, ADR-004's claim marker).
func JobNetworkName(instanceName string) string {
	return RunnerContainerName(instanceName) + "-net"
}

// WorkspaceVolumeName returns the per-job workspace volume's
// deterministic name (ADR-001).
func WorkspaceVolumeName(instanceName string) string {
	return RunnerContainerName(instanceName) + "-workspace"
}

// SocketVolumeName returns the per-job DinD socket volume's deterministic
// name (ADR-001).
func SocketVolumeName(instanceName string) string {
	return RunnerContainerName(instanceName) + "-socket"
}

// DindStateVolumeName returns the per-job dind-state volume's
// deterministic name (ADR-001).
func DindStateVolumeName(instanceName string) string {
	return RunnerContainerName(instanceName) + "-dind-state"
}
