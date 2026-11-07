package topology

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// CacheVolumeResult reports the outcome of EnsureCacheVolume.
type CacheVolumeResult struct {
	// Name is the volume holding the cache, which may be an alternate of the preferred
	// name.
	Name string

	// Hit reports whether a volume with this identity was found, so its contents are
	// reused.
	Hit bool
}

// maxCacheReconcileAttempts bounds how many slots EnsureCacheVolume tries, the
// preferred name included. Past it the allocation fails and GARM retries, because the
// provider never deletes a volume it cannot prove is its own (ADR-003).
const maxCacheReconcileAttempts = 8

// EnsureCacheVolume returns the cache volume for the identity in labels, creating it if
// none exists. A cache's identity is its label set and its name is only a slot, so a
// hit can live at any name, and a preferred name held by anything but this cache is
// routed around to a deterministic alternate. It never removes a volume: a daemon
// auto-created unlabelled volume cannot be told from a foreign one (ADR-003, "A cache's
// identity is its label set, not its name"). It fails once maxCacheReconcileAttempts
// slots are occupied.
func (m *Manager) EnsureCacheVolume(ctx context.Context, preferredName string, labels map[string]string) (CacheVolumeResult, error) {
	// Reuse a volume with this identity under any name, including an alternate an
	// earlier allocation chose. last-used is not refreshed: a volume's labels cannot
	// change.
	if name, found, err := m.discoverCacheByIdentity(ctx, labels); err != nil {
		return CacheVolumeResult{}, err
	} else if found {
		return CacheVolumeResult{Name: name, Hit: true}, nil
	}

	// VolumeCreate is idempotent on a duplicate name: the daemon returns the existing
	// volume with its original labels and discards these, so the returned labels say
	// whether the slot is ours.
	candidate := preferredName
	for attempt := 1; ; attempt++ {
		created, err := m.cli.VolumeCreate(ctx, volume.CreateOptions{Name: candidate, Labels: labels})
		if err != nil {
			return CacheVolumeResult{}, fmt.Errorf("failed to ensure cache volume %q: %w", candidate, err)
		}
		verr := spec.ValidateAdoptedCacheVolume(candidate, created.Labels, labels)
		if verr == nil {
			// Created here, or first by a concurrent peer or another controller with
			// this identity, which is why Hit can under-report a shared volume.
			return CacheVolumeResult{Name: candidate, Hit: false}, nil
		}
		if attempt >= maxCacheReconcileAttempts {
			return CacheVolumeResult{}, fmt.Errorf("cache slot %q and %d deterministic alternates are each occupied by a volume that is not our validated cache; refusing to delete any of them (ADR-003 never-delete-unprovable) and failing this allocation so GARM retries: %w", preferredName, attempt-1, verr)
		}
		// Leave the occupied slot untouched and route around it.
		slog.WarnContext(ctx, "cache slot occupied by a volume that is not our validated cache; reconciling to an alternate name",
			"resource", "cache-volume", "volume", candidate, "error", verr)
		next := spec.AlternateCacheVolumeName(preferredName, attempt)
		if err := spec.ValidateDerivedName("reconciled cache volume", next); err != nil {
			return CacheVolumeResult{}, fmt.Errorf("cache reconcile for %q could not derive a valid alternate name: %w", preferredName, err)
		}
		candidate = next
	}
}

// discoverCacheByIdentity returns the name of a volume carrying the identity in want,
// and whether one exists. It does not filter on controller-id, since controllers share
// caches (ADR-003). If a race left several, the lexicographically smallest wins: all
// are valid caches, so the pick only has to be stable.
func (m *Manager) discoverCacheByIdentity(ctx context.Context, want map[string]string) (string, bool, error) {
	list, err := m.cli.VolumeList(ctx, volume.ListOptions{Filters: spec.CacheIdentityFilter(want)})
	if err != nil {
		return "", false, fmt.Errorf("failed to discover cache volume by identity: %w", err)
	}
	best := ""
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		// Re-check the exact identity on top of the daemon's filter.
		if spec.ValidateAdoptedCacheVolume(v.Name, v.Labels, want) != nil {
			continue
		}
		if best == "" || v.Name < best {
			best = v.Name
		}
	}
	return best, best != "", nil
}
