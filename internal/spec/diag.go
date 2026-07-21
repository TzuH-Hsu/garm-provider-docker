package spec

import (
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
)

// This file holds ADR-003's diagnostic-logs pure functions (M2-W2): the
// per-repo diag-logs volume name/labels, its runner mount, and the
// provider-run prune helper's command. Retention is enforced PROVIDER-SIDE (a
// helper container the provider runs during its GC pass), never in the runner
// entrypoint — the entrypoint runs alongside untrusted job code that could
// tamper with a retention window it could reach (ADR-003 red-team F14).

// RunnerDiagDir is the actions/runner diagnostic-log directory inside the
// runner image (research.md §3.B: the runner installs into /actions-runner, and
// writes its _diag logs under that root). The per-repo diag volume mounts here
// so a repository's runner diagnostics survive across its ephemeral jobs.
const RunnerDiagDir = "/actions-runner/_diag"

// RunnerDiagDirEnv carries RunnerDiagDir to the runner-image entrypoint so it
// can ensure the mounted diag dir exists and is runner-owned before dropping
// privileges (a fresh named volume mounts root-owned; the unprivileged runner
// could not otherwise write its logs there). This is DIRECTORY SETUP, not
// retention: pruning stays entirely provider-side (DiagPruneCommand). Emitted
// only when a persistent diag volume is actually mounted.
const RunnerDiagDirEnv = "GARM_DIAG_DIR"

// diagPruneMountDir is where the diag volume is mounted READ-WRITE inside the
// short-lived prune helper container. Deliberately not RunnerDiagDir — the
// helper runs the runner image and only needs the volume somewhere to `find`
// in; a neutral path avoids any confusion with the runner's own layout.
const diagPruneMountDir = "/garm-diag-logs"

// DiagVolumeName builds the per-repo diagnostic-logs volume name (ADR-003 W2):
// garm-cache-diag-logs-<repokey>. Like the toolcache/pnpm volumes it is
// repo-keyed (and subject to the same entity-scope eligibility — org/enterprise
// pools get none by default), but it carries no generation/pnpm-major salt: a
// repo's diagnostics are not invalidated by an image-generation bump, they are
// simply pruned by age.
func DiagVolumeName(repoKey string) string {
	return cacheVolumeNamePrefix + string(CacheKindDiagLogs) + "-" + repoKey
}

// DiagLabels returns the label set for a diagnostic-logs volume (ADR-003 W2):
// the common cache markers (managed, controller-id, cache=true, a CREATION-time
// last-used) plus cache-kind=diag-logs and repo=<repokey>. Like every cache
// volume it carries NO instance-name, so ADR-004's teardown/sweep predicate
// structurally excludes it. It reuses CacheVolumeIdentity (controller +
// repokey), the same identity the toolcache/pnpm builders use. lastUsed is
// caller-supplied (never time.Now() here) and records the creation instant.
func (c CacheVolumeIdentity) DiagLabels(lastUsed time.Time) map[string]string {
	return c.baseCacheLabels(CacheKindDiagLogs, lastUsed)
}

// DiagLogsMount returns the per-repo diag-logs volume mounted READ-WRITE into
// the runner at RunnerDiagDir. Read-write is required — the runner WRITES its
// diagnostic logs here; that is the whole point. Cross-repo poisoning is not a
// concern (it is repo-keyed, so a repo only ever sees its own diagnostics), and
// retention is enforced provider-side out of band (DiagPruneCommand), never by
// the untrusted runner.
func DiagLogsMount(name string) mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Source: name,
		Target: RunnerDiagDir,
	}
}

// DiagPruneContainerSpec bundles the inputs BuildDiagPruneContainer turns into
// the prune helper's config. Image is any image with `find` (the provider uses
// the already-present runner image); VolumeName is the diag volume; RetentionDays
// is [cache].diagnostic_log_retention_days; Labels are the helper labels
// (role=cache-helper, no instance-name).
type DiagPruneContainerSpec struct {
	Image         string
	VolumeName    string
	RetentionDays int
	Labels        map[string]string
}

// diagMinutesPerDay converts the retention window (days) to the minute-precise
// unit diagPruneScript uses.
const diagMinutesPerDay = 24 * 60

// diagPruneScript deletes every regular file older than retentionDays under the
// mounted diag volume. It uses `-mmin +<retentionDays*1440>` (minutes), NOT
// `-mtime +<N>` (L8): `find -mtime +N` counts only WHOLE 24-hour periods and
// discards the remainder, so `-mtime +7` actually requires a file to be 8+ days
// old before it is deleted — an off-by-a-day that silently widens the retention
// window. The minute form deletes anything strictly older than exactly N days. It
// runs as root in the helper (so it can delete runner-owned logs) and is scoped to
// the single mounted diag volume — it can never reach anything but the one volume
// the provider mounted, satisfying the allowlist-safe requirement (ADR-003 F14).
// It is deliberately NOT `docker system prune` or any unscoped operation.
func diagPruneScript(retentionDays int) string {
	return "find " + diagPruneMountDir + " -type f -mmin +" + strconv.Itoa(retentionDays*diagMinutesPerDay) + " -delete"
}

// BuildDiagPruneContainer assembles the container.Config/HostConfig for the
// short-lived diagnostic-log PRUNE helper (ADR-003 F14): it overrides the
// image entrypoint with the age-scoped `find -delete` (diagPruneScript) and
// mounts ONLY the one diag volume read-write at diagPruneMountDir. Retention
// logic thus lives entirely outside the untrusted runner-execution boundary —
// the runner never prunes its own logs. Joins no network; drops the image CMD.
func BuildDiagPruneContainer(s DiagPruneContainerSpec) (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:      s.Image,
		Labels:     s.Labels,
		Entrypoint: strslice.StrSlice{"/bin/sh", "-ec", diagPruneScript(s.RetentionDays)},
		Cmd:        strslice.StrSlice{},
	}
	host := &container.HostConfig{
		// No network (L9): the prune is a purely local `find -delete` and needs no
		// egress, so the helper joins the "none" network rather than the default
		// bridge — least privilege for a container that touches only one mounted
		// volume.
		NetworkMode: "none",
		Mounts: []mount.Mount{{
			Type:   mount.TypeVolume,
			Source: s.VolumeName,
			Target: diagPruneMountDir,
		}},
	}
	return cfg, host
}
