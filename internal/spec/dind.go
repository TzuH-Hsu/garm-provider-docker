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
)

// DindCommand is dockerd's argv inside the sidecar (ADR-001): listen only on
// the shared unix socket, with an EXPLICIT --storage-driver (never
// autodetected — NAS host filesystems make in-container autodetection
// unreliable, so config.Config.StorageDriver is always passed through). The
// docker:dind image's entrypoint runs the DinD setup then execs this argv.
func DindCommand(storageDriver string) []string {
	return []string{
		"dockerd",
		"--host=" + DindDockerHost,
		"--storage-driver=" + storageDriver,
	}
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
// job-network attachment, and Privileged/Runtime pair are assembled.
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
		Cmd:    DindCommand(s.StorageDriver),
	}

	host := &container.HostConfig{
		Privileged: s.Privileged,
		Mounts: []mount.Mount{
			SocketVolumeMount(s.SocketVolumeName),
			DindStateMount(s.DindStateVolumeName),
		},
	}
	if s.Runtime != "" {
		// Empty for privileged-sidecar (the daemon default runtime);
		// "sysbox-runc" for sysbox-runc mode (WP4).
		host.Runtime = s.Runtime
	}
	if s.NetworkName != "" {
		// Same per-job network as the runner, as its sole attachment
		// (ADR-001) — not the default bridge.
		host.NetworkMode = container.NetworkMode(s.NetworkName)
	}
	if s.MemoryBytes > 0 {
		host.Resources.Memory = s.MemoryBytes
	}

	return cfg, host
}
