package topology

import (
	"context"
	"fmt"

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
// contents intact. Hit vs. miss is inferred from whether the returned volume
// carries the last-used timestamp this call just supplied (a fresh volume does)
// or an earlier one (a pre-existing volume, whose original creation-time
// last-used the daemon kept).
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
	created, err := m.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels})
	if err != nil {
		return CacheVolumeResult{}, fmt.Errorf("failed to ensure cache volume %q: %w", name, err)
	}

	// A fresh create echoes back the labels we sent (including our last-used);
	// an idempotent reuse echoes the pre-existing volume's ORIGINAL labels, so a
	// different last-used means the volume was already there — a cache hit.
	hit := created.Labels[spec.LabelLastUsed] != labels[spec.LabelLastUsed]
	return CacheVolumeResult{Name: name, Hit: hit}, nil
}
