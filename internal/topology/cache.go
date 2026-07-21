package topology

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// CacheVolumeResult reports the outcome of EnsureCacheVolume.
type CacheVolumeResult struct {
	// Name is the cache volume's name (echoed back for convenience).
	Name string

	// Hit reports whether the volume already existed (a cache HIT — its
	// contents are reused) as opposed to being freshly created this call (a
	// MISS — an empty volume the first job will populate).
	Hit bool
}

// EnsureCacheVolume idempotently creates or reuses a persistent cache volume
// (ADR-003). This is fundamentally DIFFERENT from createFreshVolume (create.go):
// a job-scoped volume must be guaranteed fresh and empty per allocation (so a
// name collision is a stale leftover to replace), whereas a cache volume is
// MEANT to outlive allocations — a name collision is the whole point, the cache
// HIT, and its contents must be preserved, never removed-and-recreated.
//
// Because the real daemon's VolumeCreate is idempotent on a duplicate name —
// it returns the EXISTING volume with its ORIGINAL labels, discarding the new
// call's labels (verified against Docker Engine 29.6.1; see docker/client.go) —
// a single VolumeCreate call both creates-on-miss and reuses-on-hit with the
// contents intact.
//
// Hit vs. miss is determined by whether the volume already EXISTED before this
// call (a label-scoped pre-check), NOT by comparing the returned last-used
// label: that label has 1-second RFC3339 resolution, so two allocations for the
// same repo landing in the same second would carry an identical timestamp and a
// genuine reuse would misreport as a miss. The pre-check is exact and
// timestamp-independent, giving operators an accurate "cache hit" signal. (A
// concurrent first-create race could have both see "absent" and both report a
// miss — harmless, since the create is idempotent and the reported bool is only
// informational.)
//
// last-used is deliberately NOT re-stamped on a hit: the daemon cannot mutate a
// local volume's labels after creation (same probe: re-VolumeCreate keeps the
// original labels; `docker volume update` is cluster-only), and removing-and-
// recreating to refresh a label would destroy the cache. The label therefore
// records the volume's creation instant; W2's opportunistic GC ages a warm
// cache by its filesystem mtime — advanced every job, since the cache is
// mounted read-write into the runner and used there — rather than by this
// immutable label. See ADR-003's amendment.
//
// The provider (create path) is what builds `name` and `labels` from the
// spec.CacheVolumeIdentity/ToolcacheVolumeName/PnpmVolumeName builders and calls
// this once per cache kind; keeping this helper name+labels-generic keeps the
// toolcache-vs-pnpm shaping in one place there.
func (m *Manager) EnsureCacheVolume(ctx context.Context, name string, labels map[string]string) (CacheVolumeResult, error) {
	existed, err := m.cacheVolumeExists(ctx, name)
	if err != nil {
		return CacheVolumeResult{}, err
	}
	if _, err := m.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels}); err != nil {
		return CacheVolumeResult{}, fmt.Errorf("failed to ensure cache volume %q: %w", name, err)
	}
	return CacheVolumeResult{Name: name, Hit: existed}, nil
}

// cacheVolumeExists reports whether a cache volume named `name` already exists
// for this controller. It filters on managed=true + this controller-id +
// cache=true, so it only ever inspects THIS provider's own cache volumes — never
// a foreign or job-scoped volume — and then matches the exact name.
func (m *Manager) cacheVolumeExists(ctx context.Context, name string) (bool, error) {
	list, err := m.cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", spec.LabelManaged+"=true"),
			filters.Arg("label", spec.LabelControllerID+"="+m.controllerID),
			filters.Arg("label", spec.LabelCache+"=true"),
		),
	})
	if err != nil {
		return false, fmt.Errorf("failed to list cache volumes to detect a hit for %q: %w", name, err)
	}
	for _, v := range list.Volumes {
		if v != nil && v.Name == name {
			return true, nil
		}
	}
	return false, nil
}
