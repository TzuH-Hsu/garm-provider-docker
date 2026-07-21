package spec

import (
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// Container filesystem layout (ADR-002). These are the provider↔runner-image
// contract for M0 "none" mode: the entrypoint in runner-images/ must agree
// with them.
const (
	// CredentialDir is the memory-backed tmpfs mount the provider streams
	// the credential files into after the container has started, via a
	// `docker exec`-fed `tar -x` (ADR-002's fetch → create → start →
	// exec-deliver sequence). The entrypoint waits for the ReadyMarker, then
	// symlinks the files into the runner install dir. mode 0700 keeps the
	// secrets readable only by their owner (RunnerUID).
	CredentialDir = "/run/garm"

	// RunnerWorkDir is the runner's working directory, backed by a per-job
	// volume (ADR-002). The path matches the myoung34 base image's
	// /actions-runner install location (research.md §3.B) so a job's
	// checkout lands on the dedicated volume rather than the container
	// rootfs.
	RunnerWorkDir = "/actions-runner/_work"

	// RunnerUID and RunnerGID are the myoung34 base image's runner user's
	// numeric uid/gid. The credential tmpfs is owned by this uid/gid so the
	// unprivileged runner user — which run.sh drops to — can read the
	// credential files (through the entrypoint's symlinks), while the
	// credential-delivery `docker exec` runs the `tar -x` as this same
	// uid/gid so the extracted files are runner-owned (ADR-002 F1/F2).
	RunnerUID = "1001"
	RunnerGID = "1001"

	// ReadyMarker is the atomic delivery marker the provider writes as the
	// LAST entry of the credential tar (ADR-002 F1). Because `tar -x`
	// creates entries in archive order, the marker appears only after every
	// real credential file is fully written, so the entrypoint — which waits
	// for exactly this file rather than polling the individual files — never
	// observes a partially delivered credential set.
	ReadyMarker = ".delivered"

	// credentialDirMode is the tmpfs mode: owner-only, expressed as the octal
	// string the HostConfig.Tmpfs short-syntax mount option takes. The
	// credential files are secrets; no other UID inside the container may read
	// them.
	credentialDirMode = "0700"

	// credentialTmpfsSizeBytes caps the credential tmpfs (the "size cap" of
	// ADR-002 F2). The credential payload is a few KiB — three JIT files, each
	// bounded to 1 MiB by the metadata client, plus the tiny .delivered marker
	// — so 16 MiB is comfortable headroom while still bounding a misbehaving or
	// hostile writer inside the container.
	credentialTmpfsSizeBytes = 16 << 20
)

// CredentialTmpfsMap returns the HostConfig.Tmpfs short-syntax entry for the
// anonymous, memory-backed tmpfs the provider delivers credential files into
// (ADR-002). It is per-container and not visible to any other container on the
// host; it is destroyed with the container at teardown, taking the credentials
// with it.
//
// The short-syntax HostConfig.Tmpfs map is used deliberately instead of the
// Mounts long-syntax (mount.Mount{Type: tmpfs, TmpfsOptions}): the real Docker
// daemon does NOT implement uid/gid for the long-syntax TmpfsOptions.Options
// and rejects a create carrying them with `invalid mount config for type
// "tmpfs": invalid option: uid` (moby api/types/mount TmpfsOptions.Options),
// whereas the legacy short-syntax Tmpfs map string DOES honor uid/gid. Owning
// the mount by the runner uid/gid (RunnerUID/RunnerGID) is what lets the
// unprivileged credential-delivery exec write into the tmpfs and the runner
// process read the files back (ADR-002 F2); mode 0700 still excludes every
// other in-container UID, and noexec/nosuid/nodev harden it. CredentialDir is
// the single source of truth for the mount path.
func CredentialTmpfsMap() map[string]string {
	return map[string]string{
		CredentialDir: fmt.Sprintf(
			"rw,noexec,nosuid,nodev,size=%d,mode=%s,uid=%s,gid=%s",
			credentialTmpfsSizeBytes, credentialDirMode, RunnerUID, RunnerGID,
		),
	}
}

// WorkspaceMount returns the per-job workspace mount at the runner working
// directory as an ANONYMOUS volume (empty Source): it is removed with the
// container via ContainerRemove(RemoveVolumes=true). This is the M0 shape,
// kept as the fallback BuildRunnerContainer uses when no named workspace
// volume is supplied (e.g. a spec-level unit test).
//
// As of M1-WP2 the provider supplies a NAMED, job-scoped, labeled workspace
// volume (NamedWorkspaceMount below) so the workspace participates directly
// in the ADR-004 teardown/orphan-sweep predicate: an anonymous volume carries
// no garm.docker/* labels of its own, so it can only be reclaimed via its
// container, never swept independently.
func WorkspaceMount() mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Target: RunnerWorkDir,
	}
}

// NamedWorkspaceMount returns the per-job workspace mount backed by the
// named, job-scoped, labeled volume `name` (spec.WorkspaceVolumeName), mounted
// at RunnerWorkDir. This is the M1-WP2 shape: the volume is created and labeled
// separately (ADR-001 workspace volume, ADR-004 job-scoped predicate) so it can
// be torn down and swept as a first-class resource, not merely reaped as the
// container's anonymous volume.
//
// The target is RunnerWorkDir (/actions-runner/_work), which is exactly where
// the runner image writes _work (RUNNER_WORKDIR resolved by
// runner-images/noble/entrypoint.sh's resolve_workdir), so a job's checkout
// lands on this dedicated volume rather than the container rootfs — the ADR-002
// contract the WP9-flagged JIT-workdir check asserts.
func NamedWorkspaceMount(name string) mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Source: name,
		Target: RunnerWorkDir,
	}
}

// RunnerContainerSpec bundles the inputs BuildRunnerContainer turns into the
// moby SDK's container/host config structs.
type RunnerContainerSpec struct {
	Image  string
	Env    []string
	Labels map[string]string

	// MemoryBytes, when > 0, sets a hard memory limit on the container. M0
	// left this 0 (unlimited) unconditionally. As of M1, package config can
	// resolve the configured/flavor memory limit
	// (config.Config.EffectiveRunnerMemoryBytes, internal/config/
	// resources.go) — but the CALLER (WP2/WP3's topology layer) is what
	// invokes it and threads the result in here, not this package: package
	// spec stays free of a dependency on package config, the same layering
	// rule DindRuntimeSelection's doc comment documents below. Until that
	// wiring lands, a caller that leaves this unset still gets M0's
	// original unlimited behavior.
	MemoryBytes int64

	// WorkspaceVolumeName, when non-empty, backs the workspace mount with a
	// named, job-scoped, labeled volume (NamedWorkspaceMount) instead of the
	// anonymous volume M0 used (WorkspaceMount). WP2 always sets it —
	// spec.WorkspaceVolumeName(instanceName) — so the workspace is a
	// first-class ADR-004 resource. Left empty (e.g. a spec-only unit test),
	// the builder falls back to the M0 anonymous volume.
	WorkspaceVolumeName string

	// NetworkName, when non-empty, joins the runner container to that Docker
	// network as its sole network via HostConfig.NetworkMode (ADR-001's
	// per-job network, the claim marker of ADR-004). Setting NetworkMode to a
	// user-defined network makes it the container's only attachment — the
	// container does NOT also join the default bridge — which is the
	// per-job isolation WP2 provisions. Left empty, the container joins the
	// default bridge, the M0 behavior.
	NetworkName string

	// SocketVolumeName, when non-empty (DinD modes only, WP3), mounts the
	// shared DinD socket volume at DindSocketDir (/var/run) so the runner
	// sees ONLY the sidecar's daemon socket there. It is the same named
	// volume the DinD sidecar mounts at the same path
	// (DindContainerSpec.SocketVolumeName), the sole runner→daemon channel —
	// never a host docker.sock, never TCP (ADR-001). Left empty ("none"
	// mode), the runner mounts no socket volume and DOCKER_HOST is unset.
	SocketVolumeName string
}

// BuildRunnerContainer assembles the container.Config and container.HostConfig
// for a runner container in "none" mode (ADR-001). It is a pure builder — no
// Docker calls — so it is cheap to table-test, and it is the single place the
// credential tmpfs and workspace volume are attached.
//
// The credential tmpfs is attached via HostConfig.Tmpfs (short syntax) rather
// than as a Mounts entry, because only the short syntax honors the runner
// uid/gid the tmpfs must be owned by — see CredentialTmpfsMap. The workspace
// stays a Mounts long-syntax volume.
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
	if s.SocketVolumeName != "" {
		// DinD modes (WP3): the runner mounts the shared socket volume at
		// DindSocketDir so it reaches ONLY the sidecar's daemon socket. Never
		// the host socket, never TCP (ADR-001). The DinD sidecar mounts this
		// same named volume at the same path.
		mounts = append(mounts, SocketVolumeMount(s.SocketVolumeName))
	}

	host := &container.HostConfig{
		Tmpfs:  CredentialTmpfsMap(),
		Mounts: mounts,
	}
	if s.NetworkName != "" {
		// Attach the runner to the per-job network as its sole network
		// (ADR-001). A user-defined NetworkMode means the container does not
		// also join the default bridge, which is the isolation guarantee.
		host.NetworkMode = container.NetworkMode(s.NetworkName)
	}
	if s.MemoryBytes > 0 {
		host.Resources.Memory = s.MemoryBytes
	}

	return cfg, host
}

// DinD mode values (ADR-001), duplicated here rather than imported from
// package config so that package spec stays free of a dependency on
// package config — mirroring internal/docker/fake.go's existing
// duplication of spec.CredentialDir as credentialTarTargetDir for the
// identical layering reason (see that file's doc comment). These three
// literal strings MUST stay identical to config.DindModeNone/
// DindModePrivilegedSidecar/DindModeSysboxRunc; a drift here would fail
// WP2/WP3's tests wiring the two packages together, which is the intended
// tripwire for catching a value change in exactly one place.
const (
	dindModeNone              = "none"
	dindModePrivilegedSidecar = "privileged-sidecar"
	dindModeSysboxRunc        = "sysbox-runc"
)

// DindContainerSpec bundles the inputs a future WP2/WP3 orchestration layer
// will use to build the DinD sidecar's container.Config/container.HostConfig
// (ADR-001), mirroring RunnerContainerSpec's shape. This work package only
// defines the pure data shape and the dind_mode → {Privileged, Runtime}
// derivation (DindRuntimeSelection below) — assembling the actual moby SDK
// structs from it (the sidecar's own tmpfs/socket-volume/dind-state-volume
// mounts, entrypoint command, env) and wiring it into CreateInstance is
// WP2/WP3's job, not this one's; there is deliberately no BuildDindContainer
// function yet.
type DindContainerSpec struct {
	Image  string
	Env    []string
	Labels map[string]string

	// MemoryBytes is the DinD sidecar's memory limit, resolved the same way
	// as RunnerContainerSpec.MemoryBytes: by the caller, via
	// config.Config.EffectiveDindMemoryBytes, never inside this package.
	MemoryBytes int64

	// StorageDriver is dockerd's explicit --storage-driver flag inside the
	// sidecar (ADR-001; config.Config.StorageDriver), e.g. "overlay2" or
	// "vfs" — always explicit, never autodetected, because NAS host
	// filesystems make in-container autodetection unreliable.
	StorageDriver string

	// Privileged and Runtime are the two HostConfig fields ADR-001 says
	// differ between privileged-sidecar and sysbox-runc mode ("Only two
	// HostConfig fields differ on the DinD sidecar"). Both are derived from
	// dind_mode by DindRuntimeSelection — a caller should never set them
	// directly from any other source, extra_specs least of all (ADR-005:
	// "the privileged flag... derived exclusively from dind_mode, never set
	// directly").
	Privileged bool
	Runtime    string

	// NetworkName joins the DinD sidecar to the per-job network (ADR-001:
	// the sidecar is on the SAME job network as the runner) as its sole
	// attachment via HostConfig.NetworkMode, exactly like
	// RunnerContainerSpec.NetworkName. WP3 always sets it —
	// spec.JobNetworkName(instanceName) — so the runner reaches the daemon
	// over the shared socket volume on a network isolated from every other
	// allocation.
	NetworkName string

	// SocketVolumeName backs the shared DinD socket mount at DindSocketDir
	// (/var/run): dockerd creates its unix socket there and the runner,
	// mounting the SAME named volume, reaches it. It is the ONLY channel
	// between the runner and the daemon — never a host socket, never TCP
	// (ADR-001). Required for a DinD sidecar.
	SocketVolumeName string

	// DindStateVolumeName backs the dind-state mount at DindStateDir
	// (/var/lib/docker): the daemon's own storage, isolated per-allocation
	// on a dedicated volume so overlay/vfs layers never leak across jobs and
	// are destroyed at teardown (ADR-001). Required for a DinD sidecar.
	DindStateVolumeName string
}

// DindRuntimeSelection derives the HostConfig.Privileged/HostConfig.Runtime
// pair ADR-001 specifies for dindMode: privileged-sidecar mode runs
// Privileged=true with the daemon's default runtime (Runtime returned
// empty); sysbox-runc mode runs Privileged=false with
// Runtime="sysbox-runc". Everything else about the two modes' topology is
// identical (ADR-001) — this function is the entire difference between them.
//
// It is an error to call this for "none": there is no DinD sidecar in that
// mode, so no runtime selection is meaningful. WP2/WP3's topology layer
// must never call this when dind_mode is "none" — the error return makes
// that programmer mistake loud instead of silently returning an
// unprivileged, default-runtime zero value that could be mistaken for a
// deliberate sysbox-adjacent choice.
func DindRuntimeSelection(dindMode string) (privileged bool, runtime string, err error) {
	switch dindMode {
	case dindModePrivilegedSidecar:
		return true, "", nil
	case dindModeSysboxRunc:
		return false, "sysbox-runc", nil
	case dindModeNone:
		return false, "", fmt.Errorf("dind runtime selection does not apply to dind_mode %q: no DinD sidecar exists in that mode", dindModeNone)
	default:
		return false, "", fmt.Errorf("dind runtime selection is undefined for dind_mode %q (want %q or %q)", dindMode, dindModePrivilegedSidecar, dindModeSysboxRunc)
	}
}
