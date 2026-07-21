package spec

import (
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
)

// This file holds ADR-003's externals-cache pure functions (M2-W2): the
// image-digest-keyed externals volume name/labels, the READ-ONLY runner mount,
// and the dedicated privileged seed step (ARC init-copy pattern, research.md
// §3.D) that copies the runner image's own externals payload into the volume
// exactly once, under a lock, before any runner ever mounts it. Like the rest of
// package spec these are side-effect-free builders — no Docker calls, no
// time.Now() — so they are cheap to table-test; the caller supplies the digest,
// labels timestamp, and volume name.

// RunnerExternalsDir is the runner image's own externals directory
// (research.md §3.B: the myoung34/github-runner base installs the runner into
// /actions-runner). It plays TWO roles: it is the SOURCE the seed step copies
// FROM (the image ships its Node runtimes here), and it is the path the
// externals volume is mounted READ-ONLY ONTO in the runner, so the runner reads
// the shared, seeded Node runtimes instead of its own image-baked copy.
const RunnerExternalsDir = "/actions-runner/externals"

// externalsSeedStagingDir is where the externals volume is mounted READ-WRITE
// inside the short-lived seed container. It is deliberately NOT
// RunnerExternalsDir: mounting the volume over the image's own externals would
// shadow the very SOURCE the seed copies from. The seed copies
// RunnerExternalsDir/. -> externalsSeedStagingDir/ instead.
const externalsSeedStagingDir = "/garm-externals-seed"

// externalsSeededMarker is the atomic "seeding complete" sentinel written LAST,
// only after the full copy finishes (ADR-003 F15). A later job — or a
// concurrent second seeder that lost the lock race — sees this file and skips
// re-seeding, so it never mounts a half-copied externals tree. It lives inside
// the volume (as a dotfile alongside the Node runtime dirs) so every container
// sharing the volume sees the same marker.
const externalsSeededMarker = externalsSeedStagingDir + "/.garm-seeded"

// externalsSeedLock is the flock lock file inside the volume. Two concurrent
// first-jobs for the same image digest both open and flock it; because both
// containers mount the SAME named volume, they lock the SAME underlying inode,
// so exactly one seeder acquires it and copies while the other blocks, then
// (finding the marker) no-ops. flock releases automatically when the holder's fd
// closes on process exit, so a crashed seeder cannot deadlock the next one.
const externalsSeedLock = externalsSeedStagingDir + "/.garm-seed.lock"

// externalsSeedScript is the seed container's shell program (ADR-003 F15,
// ARC init-copy pattern). Under `sh -ec` it: takes an exclusive flock on the
// in-volume lock file; if the atomic marker already exists, exits 0 (already
// seeded — the common warm path and the lock-race loser); otherwise copies the
// image's externals payload into the volume (when the image actually ships one),
// fsyncs, and writes the marker LAST. `cp -a` preserves ownership/perms so the
// runner (uid 1001) can execute the Node binaries through the read-only mount.
// Any copy failure aborts non-zero (set -e), which fails the allocation rather
// than mounting a partial tree.
//
// The copy is guarded by `[ -d <source> ]`: an image that ships no externals
// (e.g. a minimal test/sleep image, or any non-actions-runner image) seeds the
// volume EMPTY and still writes the marker, so the provider mounts a
// consistent (if empty) read-only externals rather than failing the create —
// such an image does not use externals anyway, and a real runner image always
// has the source so the guard is a true no-op there.
const externalsSeedScript = `exec 9>"` + externalsSeedLock + `"
flock 9
if [ -e "` + externalsSeededMarker + `" ]; then
  exit 0
fi
if [ -d "` + RunnerExternalsDir + `" ]; then
  cp -a "` + RunnerExternalsDir + `/." "` + externalsSeedStagingDir + `/"
fi
sync
touch "` + externalsSeededMarker + `"
`

// externalsVolumeNamePrefix is the shared name prefix for externals volumes,
// so an operator can spot them via `docker volume ls | grep`. The authoritative
// selector is still the label set (cache=true + cache-kind=externals).
const externalsVolumeNamePrefix = cacheVolumeNamePrefix + string(CacheKindExternals) + "-"

// ExternalsVolumeName builds the externals volume name (ADR-003):
// garm-cache-externals-<imageDigest>. It is keyed on the runner IMAGE digest
// ONLY — no repokey and no controller-id — so it is deliberately shared across
// every repository AND every controller on the host: its contents are Node
// runtimes tied to the runner image version, carrying no repository data, so
// cross-repo/cross-controller sharing is safe (and the read-only mount, plus
// privileged-only seeding, is what keeps it safe — see BuildExternalsSeedContainer
// and ExternalsROMount). imageDigest must be a bare, Docker-name-safe token
// (the caller passes the image ID's hex, digest colon stripped).
func ExternalsVolumeName(imageDigest string) string {
	return externalsVolumeNamePrefix + imageDigest
}

// ExternalsVolumeIdentity is the identity an externals volume's labels derive
// from. Unlike CacheVolumeIdentity (controller + repokey), an externals volume
// is scoped to a controller and an IMAGE DIGEST only — no repokey — because it
// is shared across repositories. The controller-id is still recorded (so this
// controller's opportunistic GC can find the externals volumes it created),
// with the documented consequence that a controller-scoped GC will not reap an
// externals volume first created (and thus labeled) by another controller — the
// same cross-controller consequence the shared, controller-id-omitting NAME
// already implies (ADR-003 amendment).
type ExternalsVolumeIdentity struct {
	ControllerID string
	ImageDigest  string
}

// ExternalsLabels returns the label set for an externals volume (ADR-003 W2):
// the common cache markers (managed, controller-id, cache=true, a
// CREATION-time last-used) plus cache-kind=externals and image-digest=<digest>.
// It carries NO repo label (shared across repos) and — like every cache volume
// — NO instance-name, so ADR-004's teardown/sweep predicate structurally
// excludes it. lastUsed is supplied by the caller (never time.Now() here),
// keeping this a pure builder; per local-volume label immutability it records the
// creation instant and is not re-stamped on reuse (see cache.go / ADR-003).
func (e ExternalsVolumeIdentity) ExternalsLabels(lastUsed time.Time) map[string]string {
	return map[string]string{
		LabelManaged:      "true",
		LabelControllerID: e.ControllerID,
		LabelCache:        "true",
		LabelCacheKind:    string(CacheKindExternals),
		LabelImageDigest:  e.ImageDigest,
		LabelLastUsed:     lastUsed.UTC().Format(time.RFC3339),
	}
}

// ExternalsROMount returns the externals volume mounted READ-ONLY into the
// runner at RunnerExternalsDir (ADR-003 red-line F4). Read-only is the whole
// point: because this volume is shared across every repository on the host, a
// read-write mount would let any one job's untrusted `run:` steps overwrite a
// shared Node binary (externals/<ver>/bin/node) that every OTHER repository's
// runner then executes — a cross-repo remote-code-execution path. The runner
// never writes it; only the privileged seed step (BuildExternalsSeedContainer)
// ever populates it.
func ExternalsROMount(name string) mount.Mount {
	return mount.Mount{
		Type:     mount.TypeVolume,
		Source:   name,
		Target:   RunnerExternalsDir,
		ReadOnly: true,
	}
}

// ExternalsSeedContainerSpec bundles the inputs BuildExternalsSeedContainer
// turns into the seed container's config. Image is the RUNNER image (its own
// externals payload is the copy source); VolumeName is the externals volume;
// Labels are the helper labels (role=cache-helper, no instance-name).
type ExternalsSeedContainerSpec struct {
	Image      string
	VolumeName string
	Labels     map[string]string
}

// BuildExternalsSeedContainer assembles the container.Config/HostConfig for the
// short-lived externals SEED container (ADR-003 F15). It overrides the runner
// image's entrypoint with the flock+copy+marker seed script
// (externalsSeedScript) and mounts the externals volume READ-WRITE at the
// staging dir — the ONE place this volume is ever written. The job's own
// workflow code never runs in this container and never influences it: the
// entrypoint is fully replaced and no job env is threaded in, so seeding stays
// outside the untrusted execution boundary. It joins no network (the copy is
// purely local) and drops the image CMD so only the seed script runs.
//
// Concurrency safety comes from the in-volume flock plus the atomic marker
// written last: exactly one of N racing first-jobs copies; the rest block on the
// lock and then no-op on the marker (externalsSeedScript). The provider runs
// this container to completion BEFORE creating the runner, so the read-only
// externals mount the runner gets is always a fully-seeded tree.
func BuildExternalsSeedContainer(s ExternalsSeedContainerSpec) (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:      s.Image,
		Labels:     s.Labels,
		Entrypoint: strslice.StrSlice{"/bin/sh", "-ec", externalsSeedScript},
		// Drop the image CMD so nothing is appended as an argument to the seed
		// script (the runner image's CMD is ["./run.sh"]).
		Cmd: strslice.StrSlice{},
	}
	host := &container.HostConfig{
		// No network (L9): the seed is a purely local `cp -a` under a flock and
		// needs no egress, so the helper joins the "none" network rather than the
		// default bridge — least privilege for a container that touches only the
		// one mounted externals volume.
		NetworkMode: "none",
		Mounts: []mount.Mount{{
			Type:   mount.TypeVolume,
			Source: s.VolumeName,
			Target: externalsSeedStagingDir,
		}},
	}
	return cfg, host
}
