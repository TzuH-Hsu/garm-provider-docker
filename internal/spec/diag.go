package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
)

// RunnerDiagDir is where the runner writes its _diag logs. The per-repo diag volume
// mounts here so they survive across ephemeral jobs.
const RunnerDiagDir = "/actions-runner/_diag"

// RunnerDiagDirEnv tells the entrypoint to make the mounted diag dir runner-owned
// before dropping privileges, since a fresh named volume mounts root-owned. It is
// directory setup only, and set only when a diag volume is mounted.
const RunnerDiagDirEnv = "GARM_DIAG_DIR"

// diagPruneMountDir is where the prune helper mounts the diag volume.
const diagPruneMountDir = "/garm-diag-logs"

// DiagVolumeName returns garm-cache-diag-logs-<repokey>. It has no generation or pnpm
// salt: an image bump does not invalidate a repository's logs, which are pruned by age.
func DiagVolumeName(repoKey string) string {
	return cacheVolumeNamePrefix + string(CacheKindDiagLogs) + "-" + repoKey
}

// DiagLabels returns a diagnostic-logs volume's labels. With no instance-name,
// ADR-004's predicate excludes it. lastUsed records creation.
func (c CacheVolumeIdentity) DiagLabels(lastUsed time.Time) map[string]string {
	return c.baseCacheLabels(CacheKindDiagLogs, lastUsed)
}

// DiagLogsMount returns the per-repo diag volume mounted read-write at RunnerDiagDir.
// It is repo-keyed, so a writable mount exposes a repository only to its own logs.
func DiagLogsMount(name string) mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Source: name,
		Target: RunnerDiagDir,
	}
}

// DiagPruneContainerSpec holds the inputs of BuildDiagPruneContainer. Image needs only
// `find`; the provider passes the runner image. Labels are helper labels.
type DiagPruneContainerSpec struct {
	Image         string
	VolumeName    string
	RetentionDays int
	Labels        map[string]string
}

// diagMinutesPerDay converts the retention window to the unit of find's -mmin.
const diagMinutesPerDay = 24 * 60

// diagPruneScript deletes regular files older than retentionDays on the one mounted
// volume. It uses -mmin because `-mtime +7` counts whole days and would keep files for
// 8 days. It runs as the image's default user, root on the runner image, so it can
// delete runner-owned logs.
func diagPruneScript(retentionDays int) string {
	return "find " + diagPruneMountDir + " -type f -mmin +" + strconv.Itoa(retentionDays*diagMinutesPerDay) + " -delete"
}

// BuildDiagPruneContainer returns the config of the helper that prunes one diag volume.
// Retention runs here and not in the entrypoint because job code could tamper with
// whatever the entrypoint does (ADR-003, "Garbage collection").
func BuildDiagPruneContainer(s DiagPruneContainerSpec) (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:      s.Image,
		Labels:     s.Labels,
		Entrypoint: strslice.StrSlice{"/bin/sh", "-ec", diagPruneScript(s.RetentionDays)},
		Cmd:        strslice.StrSlice{},
	}
	host := &container.HostConfig{
		// The prune is local, so the helper gets no network.
		NetworkMode: "none",
		Mounts: []mount.Mount{{
			Type:   mount.TypeVolume,
			Source: s.VolumeName,
			Target: diagPruneMountDir,
		}},
	}
	return cfg, host
}

// diagPruneStampPrefix and diagPruneStampHashLen shape DiagPruneStampName.
const (
	diagPruneStampPrefix  = "garm-diagprune-stamp-"
	diagPruneStampHashLen = 16
)

// DiagPruneStampName returns the fixed container name of a controller's diag-prune
// stamp. The name is the claim: Docker refuses a second container of that name, so when
// several processes find the stamp expired, exactly one prunes (ADR-003). Hashing the
// controller ID keeps the name legal and gives each controller on a daemon its own.
func DiagPruneStampName(controllerID string) string {
	sum := sha256.Sum256([]byte(controllerID))
	return diagPruneStampPrefix + hex.EncodeToString(sum[:])[:diagPruneStampHashLen]
}

// ThrottleStamp names the pass a throttle stamp rate-limits.
type ThrottleStamp string

const (
	// StampOrphanSweep throttles ListInstances' controller-wide orphan sweep.
	StampOrphanSweep ThrottleStamp = "sweep"
	// StampCacheGC throttles the cache GC pass of ListInstances and CreateInstance.
	StampCacheGC ThrottleStamp = "cachegc"
)

// ThrottleStampName returns the fixed container name of a controller's stamp for pass,
// built like DiagPruneStampName with the pass in the prefix.
func ThrottleStampName(controllerID string, pass ThrottleStamp) string {
	sum := sha256.Sum256([]byte(controllerID))
	return "garm-" + string(pass) + "-stamp-" + hex.EncodeToString(sum[:])[:diagPruneStampHashLen]
}

// BuildStampContainer returns the config of a stamp: a container created and never
// started, whose labels alone matter. It mounts nothing, so it never pins a volume
// against `docker volume prune`, and its `true` entrypoint makes a manual start harmless.
func BuildStampContainer(image string, labels map[string]string) (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:      image,
		Labels:     labels,
		Entrypoint: strslice.StrSlice{"true"},
		Cmd:        strslice.StrSlice{},
	}
	host := &container.HostConfig{NetworkMode: "none"}
	return cfg, host
}
