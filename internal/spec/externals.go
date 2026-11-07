package spec

import (
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
)

// RunnerExternalsDir is the runner image's externals directory. The seed step copies
// from it, and the externals volume is mounted read-only onto it in the runner.
const RunnerExternalsDir = "/actions-runner/externals"

// externalsSeededMarkerBasename is the marker's one name, so the runner waits for
// exactly the file the seeder writes.
const externalsSeededMarkerBasename = ".garm-seeded"

// RunnerExternalsSeededMarkerPath is where the seed marker appears inside the runner.
// The entrypoint blocks on it before launching the runner, which makes a half-seeded
// start impossible whatever the provider's timing (ADR-003).
const RunnerExternalsSeededMarkerPath = RunnerExternalsDir + "/" + externalsSeededMarkerBasename

// RunnerExternalsSeededMarkerEnv carries that path to the entrypoint. It is set only
// when an externals volume is mounted; when unset, the entrypoint skips the wait.
const RunnerExternalsSeededMarkerEnv = "GARM_EXTERNALS_SEEDED_MARKER"

// externalsSeedStagingDir is where the seed container mounts the volume read-write. It
// cannot be RunnerExternalsDir: that would hide the image's externals, the copy's source.
const externalsSeedStagingDir = "/garm-externals-seed"

// externalsSeededMarker is written last, after the full copy. A later job, or a
// seeder that lost the lock race, sees it and skips seeding.
const externalsSeededMarker = externalsSeedStagingDir + "/" + externalsSeededMarkerBasename

// externalsSeedLock is the flock file inside the volume. Racing seeders mount the same
// volume, so they lock the same inode. flock releases when the holder's fd closes, so
// a crashed seeder cannot deadlock the next one.
const externalsSeedLock = externalsSeedStagingDir + "/.garm-seed.lock"

// externalsSeedScript takes the lock, exits if the marker exists, and otherwise copies
// the image's externals and writes the marker last. `cp -a` preserves ownership so the
// runner can execute the binaries through the read-only mount, and under `sh -e` a
// failed copy fails the allocation rather than mounting a partial tree. An image with
// no externals seeds an empty, marked volume rather than failing the create.
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

// externalsVolumeNamePrefix is the name prefix of externals volumes.
const externalsVolumeNamePrefix = cacheVolumeNamePrefix + string(CacheKindExternals) + "-"

// ExternalsVolumeName returns garm-cache-externals-<imageDigest>, with imageDigest a
// bare Docker-name-safe token (the image ID's hex). It has no repokey or controller ID:
// the contents are Node runtimes with no repository data, and sharing them is safe
// because the mount is read-only and only the seed helper writes.
func ExternalsVolumeName(imageDigest string) string {
	return externalsVolumeNamePrefix + imageDigest
}

// ExternalsVolumeIdentity is what an externals volume's labels derive from. ControllerID
// records the creating controller, so each controller's GC reports only the externals
// volumes it created, even though the volume name is shared.
type ExternalsVolumeIdentity struct {
	ControllerID string
	ImageDigest  string
}

// ExternalsLabels returns an externals volume's labels. They have no repo label, since
// the volume is shared, and no instance-name, so ADR-004's predicate excludes it.
// lastUsed records creation and is not refreshed on reuse.
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

// ExternalsROMount returns the externals volume mounted read-only at RunnerExternalsDir.
// The volume is shared across repositories, so a writable mount would let one job's
// `run:` steps overwrite a Node binary that every other repository's runner executes:
// a cross-repository code execution path. Only the seed helper writes it.
func ExternalsROMount(name string) mount.Mount {
	return mount.Mount{
		Type:     mount.TypeVolume,
		Source:   name,
		Target:   RunnerExternalsDir,
		ReadOnly: true,
	}
}

// ExternalsSeedContainerSpec holds the inputs of BuildExternalsSeedContainer. Image is
// the runner image, whose externals are the copy source. Labels are helper labels.
type ExternalsSeedContainerSpec struct {
	Image      string
	VolumeName string
	Labels     map[string]string
}

// BuildExternalsSeedContainer returns the config of the container that seeds the
// externals volume, the one place the volume is written. It replaces the image's
// entrypoint and threads in no job environment, so workflow code cannot influence it.
// Run it to completion before creating the runner (ADR-003, "Externals").
func BuildExternalsSeedContainer(s ExternalsSeedContainerSpec) (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:      s.Image,
		Labels:     s.Labels,
		Entrypoint: strslice.StrSlice{"/bin/sh", "-ec", externalsSeedScript},
		// The image's CMD would otherwise be appended as arguments to the seed script.
		Cmd: strslice.StrSlice{},
	}
	host := &container.HostConfig{
		// The copy is local, so the helper gets no network.
		NetworkMode: "none",
		Mounts: []mount.Mount{{
			Type:   mount.TypeVolume,
			Source: s.VolumeName,
			Target: externalsSeedStagingDir,
		}},
	}
	return cfg, host
}
