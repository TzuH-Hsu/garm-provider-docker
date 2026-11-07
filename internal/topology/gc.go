package topology

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// cacheVolumeFilter selects this controller's cache volumes. A shared cache first
// created by another controller carries that controller's id, so it is not selected
// here.
func (m *Manager) cacheVolumeFilter() filters.Args {
	return filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+m.controllerID),
		filters.Arg("label", spec.LabelCache+"=true"),
	)
}

// StaleCache is a cache volume the GC reports as stale for the provider to log.
type StaleCache struct {
	Name   string
	Reason string
}

// ListStaleCaches returns this controller's cache volumes that decide flags as stale
// and that carry their kind's full identity, so the log never names a volume not
// provably ours. It removes nothing: VolumeRemove is by name with no atomic
// label-qualified form, so an inspect-then-delete could remove a foreign volume that
// took the freed name (ADR-003, "A cache's identity is its label set, not its name").
func (m *Manager) ListStaleCaches(ctx context.Context, decide func(labels map[string]string) (bool, string)) ([]StaleCache, error) {
	list, err := m.cli.VolumeList(ctx, volume.ListOptions{Filters: m.cacheVolumeFilter()})
	if err != nil {
		return nil, fmt.Errorf("cache GC: failed to list cache volumes: %w", err)
	}

	var stale []StaleCache
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		// Re-check what the label filter returned before naming a volume as ours.
		if v.Labels[spec.LabelControllerID] != m.controllerID {
			continue
		}
		if err := spec.ValidateCacheVolumeKind(v.Labels); err != nil {
			continue
		}
		evict, reason := decide(v.Labels)
		if !evict {
			continue
		}
		stale = append(stale, StaleCache{Name: v.Name, Reason: reason})
	}
	return stale, nil
}

// DiagVolumeRef is a diagnostic-logs cache volume with the identity labels it had when
// listed. The provider checks them against the volume its prune helper pins, which
// catches a same-name replacement (spec.ValidateDiagPruneTarget).
type DiagVolumeRef struct {
	Name   string
	Labels map[string]string
}

// ListDiagVolumes returns this controller's diagnostic-logs cache volumes that carry
// their full identity, for the provider's retention prune.
func (m *Manager) ListDiagVolumes(ctx context.Context) ([]DiagVolumeRef, error) {
	f := m.cacheVolumeFilter()
	f.Add("label", spec.LabelCacheKind+"="+string(spec.CacheKindDiagLogs))

	list, err := m.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return nil, fmt.Errorf("cache GC: failed to list diag volumes: %w", err)
	}
	var refs []DiagVolumeRef
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		if v.Labels[spec.LabelCacheKind] != string(spec.CacheKindDiagLogs) || v.Labels[spec.LabelControllerID] != m.controllerID {
			continue
		}
		// The prune is destructive, so a volume missing repo or repo-url-digest is
		// never a target.
		if err := spec.ValidateCacheVolumeKind(v.Labels); err != nil {
			continue
		}
		refs = append(refs, DiagVolumeRef{Name: v.Name, Labels: cloneLabelMap(v.Labels)})
	}
	return refs, nil
}

// cloneLabelMap copies labels so a snapshot cannot change with the daemon response's
// map.
func cloneLabelMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ListUnprovableCacheCruft returns the names of volumes with a cache name prefix but no
// cache=true label, for the provider to log. A daemon auto-created unlabelled volume
// and a foreign one look the same, so the provider never deletes them (ADR-003). It
// lists every volume on the host.
func (m *Manager) ListUnprovableCacheCruft(ctx context.Context) ([]string, error) {
	list, err := m.cli.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("cache GC: failed to list volumes for cruft visibility: %w", err)
	}
	var cruft []string
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		if !spec.HasCacheVolumeNamePrefix(v.Name) {
			continue
		}
		if v.Labels[spec.LabelCache] == "true" {
			continue // labelled as a cache: not cruft
		}
		cruft = append(cruft, v.Name)
	}
	return cruft, nil
}
