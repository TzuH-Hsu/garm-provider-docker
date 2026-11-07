package spec

import (
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// Container filesystem layout, shared with the entrypoint in runner-images/common.
const (
	// CredentialDir is the memory-backed tmpfs the provider streams the credential
	// files into after the container starts (ADR-002).
	CredentialDir = "/run/garm"

	// RunnerWorkDir is the runner's working directory, backed by a per-job volume so a
	// checkout stays off the container's writable layer.
	RunnerWorkDir = "/actions-runner/_work"

	// RunnerUID and RunnerGID are the base image's runner user. They own the credential
	// tmpfs, and delivery runs `tar -x` as them, so the runner can read the files.
	RunnerUID = "1001"
	RunnerGID = "1001"

	// ReadyMarker is the last entry of the credential tar. `tar -x` creates entries in
	// order, so once it exists, which is all the entrypoint waits for, every file is complete.
	ReadyMarker = ".delivered"

	// credentialDirMode keeps the secrets from every other UID in the container.
	credentialDirMode = "0700"

	// credentialTmpfsSizeBytes bounds what a hostile writer in the container can put in
	// the tmpfs, with ample room for three files capped at 1 MiB each and the marker.
	credentialTmpfsSizeBytes = 16 << 20

	// RunnerIPv6Sysctl disables IPv6 in the runner's network namespace. Otherwise a
	// server binding `localhost` can land on ::1 while a Node client dials 127.0.0.1 and
	// gets ECONNREFUSED. It is per namespace and needs no privilege. If the daemon cannot
	// apply it, the provider recreates the runner once without it (ADR-002, "IPv6 is
	// disabled in the runner").
	RunnerIPv6Sysctl = "net.ipv6.conf.all.disable_ipv6"
)

// CredentialTmpfsMap returns the HostConfig.Tmpfs entry for the credential tmpfs. It
// is anonymous and per container, so no other container can see it, and it is
// destroyed with the container.
//
// It uses the short syntax because the daemon rejects uid and gid in the long syntax's
// TmpfsOptions.Options ("invalid option: uid"). Ownership by the runner user lets
// delivery write the files and the runner read them; mode 0700 excludes every other UID.
func CredentialTmpfsMap() map[string]string {
	return map[string]string{
		CredentialDir: fmt.Sprintf(
			"rw,noexec,nosuid,nodev,size=%d,mode=%s,uid=%s,gid=%s",
			credentialTmpfsSizeBytes, credentialDirMode, RunnerUID, RunnerGID,
		),
	}
}

// WorkspaceMount returns the workspace as an anonymous volume, removed with its
// container. BuildRunnerContainer uses it only when no named volume is supplied.
func WorkspaceMount() mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Target: RunnerWorkDir,
	}
}

// NamedWorkspaceMount returns the workspace backed by the labelled per-job volume
// name, so teardown and the sweep can remove it as a resource of its own (ADR-004).
func NamedWorkspaceMount(name string) mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Source: name,
		Target: RunnerWorkDir,
	}
}

// CacheVolumeMount returns a cache volume mounted read-write at target. Setup actions
// and pnpm populate these volumes; that is how the cache warms.
func CacheVolumeMount(name, target string) mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Source: name,
		Target: target,
	}
}

// RunnerContainerSpec holds the inputs of BuildRunnerContainer.
type RunnerContainerSpec struct {
	Image  string
	Env    []string
	Labels map[string]string

	// MemoryBytes is a hard memory limit when > 0, resolved by the caller.
	MemoryBytes int64

	// WorkspaceVolumeName names the labelled workspace volume; empty means anonymous.
	WorkspaceVolumeName string

	// NetworkName, when set, is the container's only network: a user-defined
	// NetworkMode also keeps it off the default bridge (ADR-001).
	NetworkName string

	// SocketVolumeName, in DinD modes, mounts the socket volume at DindSocketDir, the only
	// runner-to-daemon channel: never a host socket, never TCP (ADR-001).
	SocketVolumeName string

	// ToolcacheVolumeName/ToolcacheMountPath and PnpmVolumeName/PnpmMountPath are the
	// repo-keyed caches, each mounted read-write only when both its name and path are set.
	// They go into the runner only, since sharing them would widen their blast radius. A
	// container job therefore gets no toolcache hits (ADR-001, Container jobs).
	ToolcacheVolumeName string
	ToolcacheMountPath  string
	PnpmVolumeName      string
	PnpmMountPath       string

	// ExternalsVolumeName mounts the shared externals volume read-only at
	// RunnerExternalsDir. A writable mount would let one job poison runtimes other
	// repositories execute (ADR-003). The provider seeds it before setting this field.
	ExternalsVolumeName string

	// DiagVolumeName mounts the per-repo diagnostic-logs volume read-write at
	// RunnerDiagDir. Retention is pruned by the provider, never by this runner.
	DiagVolumeName string
}

// BuildRunnerContainer returns the runner's container and host config: the credential
// tmpfs, the workspace, and whichever optional mounts and limits s sets.
func BuildRunnerContainer(s RunnerContainerSpec) (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:  s.Image,
		Env:    s.Env,
		Labels: s.Labels,
	}

	workspace := WorkspaceMount()
	if s.WorkspaceVolumeName != "" {
		workspace = NamedWorkspaceMount(s.WorkspaceVolumeName)
	}

	mounts := []mount.Mount{workspace}
	host := &container.HostConfig{
		Tmpfs:  CredentialTmpfsMap(),
		Mounts: mounts,
		// Runner only: a sidecar that joins this namespace is covered too, and a sysbox
		// sidecar keeps the daemon default in its own. See RunnerIPv6Sysctl.
		Sysctls: map[string]string{RunnerIPv6Sysctl: "1"},
	}
	if s.SocketVolumeName != "" {
		host.Mounts = append(host.Mounts, SocketVolumeMount(s.SocketVolumeName))
		// dockerd makes the socket group-owned by DindSocketGID. GroupAdd covers a process
		// running as the container's user directly (RUN_AS_ROOT); the entrypoint adds the
		// runner user to the same GID so gosu's initgroups keeps it when dropping privileges.
		host.GroupAdd = append(host.GroupAdd, DindSocketGID)
	}
	if s.NetworkName != "" {
		host.NetworkMode = container.NetworkMode(s.NetworkName)
	}
	if s.ToolcacheVolumeName != "" && s.ToolcacheMountPath != "" {
		host.Mounts = append(host.Mounts, CacheVolumeMount(s.ToolcacheVolumeName, s.ToolcacheMountPath))
	}
	if s.PnpmVolumeName != "" && s.PnpmMountPath != "" {
		host.Mounts = append(host.Mounts, CacheVolumeMount(s.PnpmVolumeName, s.PnpmMountPath))
	}
	if s.ExternalsVolumeName != "" {
		host.Mounts = append(host.Mounts, ExternalsROMount(s.ExternalsVolumeName))
	}
	if s.DiagVolumeName != "" {
		host.Mounts = append(host.Mounts, DiagLogsMount(s.DiagVolumeName))
	}
	if s.MemoryBytes > 0 {
		host.Resources.Memory = s.MemoryBytes
	}

	return cfg, host
}

// DinD mode values, duplicated so package spec does not depend on package config. They
// must equal config.DindMode*; the provider tests use both, so a drift fails there.
const (
	dindModeNone              = "none"
	dindModePrivilegedSidecar = "privileged-sidecar"
	dindModeSysboxRunc        = "sysbox-runc"

	// dindRuntimeSysbox is the OCI runtime name sysbox-runc mode selects.
	dindRuntimeSysbox = "sysbox-runc"
)

// DindContainerSpec holds the inputs of BuildDindContainer.
type DindContainerSpec struct {
	Image  string
	Env    []string
	Labels map[string]string

	// MemoryBytes is the sidecar's memory limit when > 0, resolved by the caller.
	MemoryBytes int64

	// StorageDriver is dockerd's explicit --storage-driver. Autodetection inside a
	// nested container is unreliable on some host filesystems (ADR-001).
	StorageDriver string

	// RegistryMirrors and InsecureRegistries come from the operator's config only,
	// never from extra_specs, and become typed dockerd flags (DindCommand).
	RegistryMirrors    []string
	InsecureRegistries []string

	// Privileged and Runtime must come from DindRuntimeSelection, never from any other
	// source and least of all extra_specs (ADR-005).
	Privileged bool
	Runtime    string

	// NetnsContainerID makes the sidecar join that container's network namespace and
	// takes precedence over NetworkName. In privileged-sidecar mode it is the runner,
	// which must already be running when the sidecar starts (ADR-001).
	NetnsContainerID string

	// NetworkName is the sidecar's only network, used in sysbox-runc mode, where the
	// sidecar keeps its own namespace (DindSharesRunnerNetns).
	NetworkName string

	// SocketVolumeName backs the socket mount, the only runner-to-daemon channel. Required.
	SocketVolumeName string

	// DindStateVolumeName backs dockerd's per-allocation data root, so layers never leak
	// across jobs. Required.
	DindStateVolumeName string

	// WorkspaceVolumeName, when set, mounts the runner's workspace volume at
	// RunnerWorkDir. The daemon resolves a nested `docker run -v "$PWD":/work` bind source
	// in the sidecar's filesystem, so it must see the same files as the runner.
	WorkspaceVolumeName string

	// ExternalsVolumeName, when set, mounts the shared externals volume read-only at
	// RunnerExternalsDir, for the same reason: a job container's /__e is a bind of that
	// path, and without it JavaScript actions have no Node runtime. Read-only as on the
	// runner. Empty with the externals cache disabled (ADR-001, "Container jobs").
	ExternalsVolumeName string
}

// DindRuntimeSelection returns HostConfig.Privileged and HostConfig.Runtime for
// dindMode: privileged with the default runtime (empty), or unprivileged with
// sysbox-runc. It returns an error for "none" and any unknown mode, so a caller bug
// cannot pass for a deliberate unprivileged, default-runtime sidecar.
func DindRuntimeSelection(dindMode string) (privileged bool, runtime string, err error) {
	switch dindMode {
	case dindModePrivilegedSidecar:
		return true, "", nil
	case dindModeSysboxRunc:
		return false, dindRuntimeSysbox, nil
	case dindModeNone:
		return false, "", fmt.Errorf("dind runtime selection does not apply to dind_mode %q: no DinD sidecar exists in that mode", dindModeNone)
	default:
		return false, "", fmt.Errorf("dind runtime selection is undefined for dind_mode %q (want %q or %q)", dindMode, dindModePrivilegedSidecar, dindModeSysboxRunc)
	}
}
