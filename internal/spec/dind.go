package spec

import (
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// DinD sidecar filesystem/daemon contract (ADR-001). These are the
// provider↔sidecar wiring for the privileged-sidecar (and, WP4, sysbox-runc)
// DinD modes: dockerd exposes its socket over a shared job-scoped volume,
// never over TCP and never via a host socket, mirroring ARC's
// gha-runner-scale-set DinD values.
const (
	// DindSocketDir is where the shared socket volume is mounted in BOTH the
	// DinD sidecar and the runner (/run). dockerd creates its unix socket
	// here; the runner, mounting the same named volume at this path, reaches
	// ONLY that socket. This is the sole runner→daemon channel — there is no
	// host docker.sock mount and no TCP listener anywhere.
	//
	// It is /run, NOT /var/run, deliberately (a real-daemon correction to
	// ADR-001's literal "/var/run/docker.sock"): on Debian/Ubuntu/Alpine —
	// the myoung34 runner base and docker:dind alike — /var/run is a symlink
	// to /run, so mounting the socket volume at /var/run resolves to /run and
	// SHADOWS the credential tmpfs at /run/garm (ADR-002), making credential
	// delivery fail on the real daemon. Mounting at /run (the volume being the
	// PARENT of /run/garm) lets Docker mount /run first and the /run/garm
	// tmpfs on top, so both coexist. This is also ARC's actual
	// gha-runner-scale-set pattern — which ADR-001 says it mirrors — where the
	// shared var-run volume is mounted at /run with DOCKER_HOST=
	// unix:///run/docker.sock. See the WP3 report; ADR-001 should be amended.
	DindSocketDir = "/run"

	// DindSocketPath is the unix socket dockerd listens on inside
	// DindSocketDir, and the path the runner's DOCKER_HOST points at.
	DindSocketPath = "/run/docker.sock"

	// DindDockerHost is the DOCKER_HOST value the runner uses (and the
	// --host dockerd listens on): the unix socket on the shared volume. The
	// runner-image entrypoint's `until docker info` readiness wait blocks on
	// this until the sidecar's daemon answers (ADR-002).
	DindDockerHost = "unix://" + DindSocketPath

	// DindStateDir is dockerd's data root (/var/lib/docker), backed by the
	// dedicated per-allocation dind-state volume so overlay/vfs layers are
	// isolated per job and destroyed at teardown (ADR-001).
	DindStateDir = "/var/lib/docker"

	// DindTLSDisabledEnv turns dockerd's TLS off (DOCKER_TLS_CERTDIR=""):
	// the daemon socket is exposed only over the shared unix-socket volume,
	// never over TCP, so TLS is unnecessary — exactly ARC's pattern
	// (ADR-001). BuildDindContainer always emits this, so "TLS off" is a
	// structural property of the builder rather than something a caller must
	// remember to set.
	DindTLSDisabledEnv = "DOCKER_TLS_CERTDIR="

	// DindSocketGID is a fixed, dedicated supplementary group ID for the
	// shared DinD socket (F1). dockerd is launched with `--group <this>`
	// (DindCommand below), so it creates /run/docker.sock group-owned by this
	// GID and group-writable; the runner container is given this GID as a
	// supplementary group (HostConfig.GroupAdd, BuildRunnerContainer) AND its
	// unprivileged `runner` user is added to a group with this GID by the
	// entrypoint before it drops privileges, so a `docker` call made as that
	// non-root user can reach the daemon socket.
	//
	// It is deliberately a FIXED value the provider controls end to end, not
	// the runner image's or docker:dind's own "docker" group: those two base
	// images ship DIFFERENT docker-group GIDs (myoung34/github-runner vs.
	// docker:dind), so relying on a named `docker` group would leave the
	// socket group-owned by one GID and the runner user a member of a
	// different one — the exact permission-denied gap this constant closes.
	// It is distinct from the runner user's own primary GID (RunnerGID=1001)
	// so socket access is an explicit, auditable group membership rather than
	// conflated with the runner user's identity, and high enough to avoid the
	// base images' system groups.
	DindSocketGID = "2000"

	// DindSocketGIDEnv is the environment variable the provider sets on the
	// runner (DinD modes only, BuildRunnerEnv) carrying DindSocketGID, so the
	// runner-image entrypoint knows which GID to add the `runner` user to
	// before dropping privileges (ADR-002; runner-images/common/entrypoint.sh).
	DindSocketGIDEnv = "DOCKER_SOCK_GID"
)

// DindCommand is dockerd's argv inside the sidecar (ADR-001): listen only on
// the shared unix socket, with an EXPLICIT --storage-driver (never
// autodetected — NAS host filesystems make in-container autodetection
// unreliable, so config.Config.StorageDriver is always passed through) and an
// EXPLICIT --group so the socket is group-owned by DindSocketGID (F1) rather
// than by whatever docker:dind's own base image happens to number its `docker`
// group — the runner user is made a member of that same GID, so a non-root
// `docker` call from the runner can reach this socket. The docker:dind image's
// entrypoint runs the DinD setup then execs this argv.
//
// registryMirrors and insecureRegistries are the operator's config-only
// registry settings (config.Config.RegistryMirrors/InsecureRegistries, already
// validated at load). They are appended AFTER the four fixed flags above, which
// stay first and unchanged: one --registry-mirror=<url> per mirror, then one
// --insecure-registry=<entry> per entry, in configured order. They are typed
// flags rather than a daemon.json passthrough so the operator can never reach
// options the provider owns (--host, --storage-driver, --group). A nil or empty
// list adds nothing.
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

// DindSharesRunnerNetns reports whether the DinD sidecar for dindMode joins
// the runner container's network namespace instead of getting its own on the
// job network (ADR-001, shared network namespace amendment).
//
// It is true for privileged-sidecar, which is what makes a port published by
// the inner daemon (`docker run -p`, testcontainers, compose) reachable on the
// runner's 127.0.0.1, as on a GitHub-hosted runner and in an ARC pod. It is
// false for sysbox-runc: a sysbox container runs in its own user namespace,
// and a network namespace created by the runner (owned by the host's initial
// user namespace) grants the sidecar's root no CAP_NET_ADMIN, so its dockerd
// could not create its bridge or NAT rules there. sysbox-runc keeps its own
// namespace on the job network.
func DindSharesRunnerNetns(dindMode string) bool {
	return dindMode == dindModePrivilegedSidecar
}

// ContainerNetworkMode returns the HostConfig.NetworkMode that joins the
// network namespace of the container with the given ID ("container:<id>").
func ContainerNetworkMode(containerID string) container.NetworkMode {
	return container.NetworkMode("container:" + containerID)
}

// SocketVolumeMount returns the shared DinD socket mount (the named socket
// volume at DindSocketDir). Both the DinD sidecar and the runner mount this
// SAME named volume at this SAME path — that shared mount, not a host socket
// or a TCP endpoint, is how the runner reaches the sidecar's daemon (ADR-001).
func SocketVolumeMount(name string) mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Source: name,
		Target: DindSocketDir,
	}
}

// DindStateMount returns the dind-state mount (the named dind-state volume at
// DindStateDir), dockerd's data root isolated per allocation (ADR-001).
func DindStateMount(name string) mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Source: name,
		Target: DindStateDir,
	}
}

// BuildDindContainer assembles the container.Config/container.HostConfig for
// the DinD sidecar (ADR-001), mirroring BuildRunnerContainer's pure-builder
// shape: no Docker calls, so it is cheap to table-test, and it is the single
// place the sidecar's dockerd command, TLS-off env, socket/dind-state mounts,
// network placement (the runner's namespace or the job network), and
// Privileged/Runtime pair are assembled.
//
// The {Privileged, Runtime} pair MUST come from DindRuntimeSelection (via the
// caller populating s.Privileged/s.Runtime) — privileged-sidecar is
// Privileged=true with the default runtime, sysbox-runc is Privileged=false
// with Runtime="sysbox-runc" (ADR-001). This builder is mode-agnostic: it
// simply applies whatever pair it is given, so WP4's sysbox-runc support is a
// thin flip of those two fields with no other change here.
//
// TLS is disabled structurally (DindTLSDisabledEnv is always prepended): the
// socket is reachable only over the shared volume, never over TCP, so a TCP
// TLS cert dir would be meaningless. Any caller-supplied s.Env is appended
// after it.
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
		// F2: mount the allocation's workspace volume into the DinD daemon at
		// the SAME path the runner uses (RunnerWorkDir). A nested `docker run
		// -v "$PWD":/work …` the job issues resolves its bind SOURCE in the
		// DAEMON's (this sidecar's) filesystem, not the runner's — so without
		// this the daemon would bind an empty/new path instead of the runner's
		// checked-out workspace. Sharing the same named volume here makes the
		// runner's workspace and the daemon's view of it the same files.
		mounts = append(mounts, NamedWorkspaceMount(s.WorkspaceVolumeName))
	}

	host := &container.HostConfig{
		Privileged: s.Privileged,
		Mounts:     mounts,
	}
	if s.Runtime != "" {
		// Empty for privileged-sidecar (the daemon default runtime);
		// "sysbox-runc" for sysbox-runc mode (WP4).
		host.Runtime = s.Runtime
	}
	switch {
	case s.NetnsContainerID != "":
		// privileged-sidecar: join the runner's network namespace, so a port
		// the inner daemon publishes is bound in the runner's namespace and
		// reachable on the runner's 127.0.0.1 (ADR-001, shared network
		// namespace amendment). The sidecar then has no network attachment of
		// its own; it reaches the job network through the runner's.
		host.NetworkMode = ContainerNetworkMode(s.NetnsContainerID)
	case s.NetworkName != "":
		// sysbox-runc: the sidecar's own namespace on the same per-job network
		// as the runner, as its sole attachment (ADR-001) — not the default
		// bridge.
		host.NetworkMode = container.NetworkMode(s.NetworkName)
	}
	if s.MemoryBytes > 0 {
		host.Resources.Memory = s.MemoryBytes
	}

	return cfg, host
}
