package spec

import (
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

	// credentialDirMode is the tmpfs mode: owner-only. The credential files
	// are secrets; no other UID inside the container may read them.
	credentialDirMode = 0o700
)

// CredentialTmpfsMount returns the anonymous, memory-backed tmpfs mount that
// the provider delivers credential files into (ADR-002). It is per-container
// and not visible to any other container on the host; it is destroyed with
// the container at teardown, taking the credentials with it.
//
// The mount is owned by the runner uid/gid (RunnerUID/RunnerGID) via the
// tmpfs uid/gid options: the credential-delivery exec runs as that same
// unprivileged user, so it must be able to write into the tmpfs, and the
// runner process must be able to read the files back (ADR-002 F2). mode 0700
// still excludes every other in-container UID.
func CredentialTmpfsMount() mount.Mount {
	return mount.Mount{
		Type:   mount.TypeTmpfs,
		Target: CredentialDir,
		TmpfsOptions: &mount.TmpfsOptions{
			Mode: credentialDirMode,
			Options: [][]string{
				{"uid", RunnerUID},
				{"gid", RunnerGID},
			},
		},
	}
}

// WorkspaceMount returns the per-job workspace mount at the runner working
// directory. In M0 this is an anonymous volume (empty Source): it is removed
// with the container via ContainerRemove(RemoveVolumes=true), which is why
// the M0 teardown does not need a separate named-volume delete step.
//
// TODO(M1): replace with a named, job-scoped, labeled volume
// (spec.WorkspaceVolumeName + ResourceWorkspace labels) so the workspace
// participates directly in the ADR-004 teardown/orphan-sweep predicate. An
// anonymous volume carries no garm.docker/* labels of its own, so it can
// only be reclaimed via its container, not swept independently.
func WorkspaceMount() mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
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
	// leaves this 0 (unlimited): the ADR-005 [resources]/flavor config that
	// supplies it is not part of the M0-minimal config surface yet.
	// TODO(M1): thread the configured/flavor memory limit in here.
	MemoryBytes int64
}

// BuildRunnerContainer assembles the container.Config and container.HostConfig
// for a runner container in "none" mode (ADR-001). It is a pure builder — no
// Docker calls — so it is cheap to table-test, and it is the single place the
// credential tmpfs and workspace volume are attached.
func BuildRunnerContainer(s RunnerContainerSpec) (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:  s.Image,
		Env:    s.Env,
		Labels: s.Labels,
	}

	host := &container.HostConfig{
		Mounts: []mount.Mount{
			CredentialTmpfsMount(),
			WorkspaceMount(),
		},
	}
	if s.MemoryBytes > 0 {
		host.Resources.Memory = s.MemoryBytes
	}

	return cfg, host
}
