package spec

import (
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// DinD sidecar contract (ADR-001). dockerd exposes its socket over a shared
// job-scoped volume, never over TCP and never via a host socket.
const (
	// DindSocketDir is where the socket volume is mounted in both the sidecar and the
	// runner, the sole runner-to-daemon channel. It must be /run, not /var/run: that is
	// a symlink to /run in these images, so a volume there would hide the credential
	// tmpfs at /run/garm. Mounted at the parent, the tmpfs goes on top (ADR-001).
	DindSocketDir = "/run"

	// DindSocketPath is the unix socket dockerd listens on, on the shared volume.
	DindSocketPath = "/run/docker.sock"

	// DindDockerHost is the runner's DOCKER_HOST and dockerd's --host.
	DindDockerHost = "unix://" + DindSocketPath

	// DindStateDir is dockerd's data root, on a per-allocation volume.
	DindStateDir = "/var/lib/docker"

	// DindTLSDisabledEnv turns dockerd's TLS off. The socket exists only on the shared
	// volume, never on TCP, so TLS is unnecessary.
	DindTLSDisabledEnv = "DOCKER_TLS_CERTDIR="

	// DindSocketGID is the group that owns the daemon socket. It is fixed because the
	// runner image and docker:dind number their `docker` groups differently, and distinct
	// from RunnerGID so socket access is an explicit group membership. It sits above the
	// base images' system groups, because the entrypoint reuses any group that already
	// has this GID and adds the runner user to it. dockerd gets it through --group; the
	// runner gets it through GroupAdd and the entrypoint.
	DindSocketGID = "2000"

	// DindSocketGIDEnv tells the entrypoint which GID to add the runner user to.
	DindSocketGIDEnv = "DOCKER_SOCK_GID"
)

// DindCommand returns dockerd's argv in the sidecar: listening only on the shared
// socket, with an explicit storage driver and socket group. One --registry-mirror per
// mirror and one --insecure-registry per entry follow in order. They are typed flags
// rather than a daemon.json passthrough so an operator cannot reach options the
// provider owns (ADR-001).
func DindCommand(storageDriver string, registryMirrors, insecureRegistries []string) []string {
	cmd := []string{
		"dockerd",
		"--host=" + DindDockerHost,
		"--storage-driver=" + storageDriver,
		"--group=" + DindSocketGID,
	}
	for _, m := range registryMirrors {
		cmd = append(cmd, "--registry-mirror="+m)
	}
	for _, r := range insecureRegistries {
		cmd = append(cmd, "--insecure-registry="+r)
	}
	return cmd
}

// DindSharesRunnerNetns reports whether the sidecar for dindMode joins the runner's
// network namespace, which makes ports the inner daemon publishes reachable on the
// runner's 127.0.0.1. Only privileged-sidecar does: under sysbox-runc the sidecar's
// root has no CAP_NET_ADMIN over the runner's namespace, so dockerd could not create
// its bridge there (ADR-001).
func DindSharesRunnerNetns(dindMode string) bool {
	return dindMode == dindModePrivilegedSidecar
}

// DindModeUnknown is what DindModeOf returns for a {Privileged, Runtime} pair
// that is neither DinD mode's.
const DindModeUnknown = "unknown"

// DindModeOf recovers a sidecar's dind_mode from its inspected HostConfig, the inverse
// of DindRuntimeSelection. A privileged-sidecar's inspected runtime is the daemon's
// default, not the empty create request, so any runtime but sysbox's is accepted when
// Privileged is set. Teardown logging uses it because no label records the mode.
func DindModeOf(privileged bool, runtime string) string {
	switch {
	case runtime == dindRuntimeSysbox:
		return dindModeSysboxRunc
	case privileged:
		return dindModePrivilegedSidecar
	default:
		return DindModeUnknown
	}
}

// ContainerNetworkMode returns the HostConfig.NetworkMode that joins the
// network namespace of the container with the given ID ("container:<id>").
func ContainerNetworkMode(containerID string) container.NetworkMode {
	return container.NetworkMode("container:" + containerID)
}

// SocketVolumeMount returns the socket mount, the same in the sidecar and the runner.
func SocketVolumeMount(name string) mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Source: name,
		Target: DindSocketDir,
	}
}

// DindStateMount returns the mount of dockerd's per-allocation data root.
func DindStateMount(name string) mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Source: name,
		Target: DindStateDir,
	}
}

// BuildDindContainer returns the sidecar's container and host config. s.Privileged and
// s.Runtime must come from DindRuntimeSelection; it applies them as given. TLS is
// always off (DindTLSDisabledEnv), and s.Env follows it.
func BuildDindContainer(s DindContainerSpec) (*container.Config, *container.HostConfig) {
	env := append([]string{DindTLSDisabledEnv}, s.Env...)

	cfg := &container.Config{
		Image:  s.Image,
		Env:    env,
		Labels: s.Labels,
		Cmd:    DindCommand(s.StorageDriver, s.RegistryMirrors, s.InsecureRegistries),
	}

	mounts := []mount.Mount{
		SocketVolumeMount(s.SocketVolumeName),
		DindStateMount(s.DindStateVolumeName),
	}
	if s.WorkspaceVolumeName != "" {
		mounts = append(mounts, NamedWorkspaceMount(s.WorkspaceVolumeName))
	}
	if s.ExternalsVolumeName != "" {
		mounts = append(mounts, ExternalsROMount(s.ExternalsVolumeName))
	}

	host := &container.HostConfig{
		Privileged: s.Privileged,
		Mounts:     mounts,
	}
	if s.Runtime != "" {
		host.Runtime = s.Runtime
	}
	switch {
	case s.NetnsContainerID != "":
		// It reaches the job network through the runner's namespace.
		host.NetworkMode = ContainerNetworkMode(s.NetnsContainerID)
	case s.NetworkName != "":
		host.NetworkMode = container.NetworkMode(s.NetworkName)
	}
	if s.MemoryBytes > 0 {
		host.Resources.Memory = s.MemoryBytes
	}

	return cfg, host
}
