package topology

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// This file holds the topology side of ADR-003's opportunistic cache GC (M2-W2):
// listing this controller's cache volumes and removing the ones a
// caller-supplied decision evicts, plus enumerating the diagnostic-logs volumes
// the provider runs its prune helper against. The DECISION (age/supersession)
// lives in spec.EvaluateCacheEviction — a pure function the provider threads in
// here — so this layer stays about the label-scoped listing and the in-use-safe
// removal.

// cacheVolumeFilter selects THIS controller's cache volumes: managed=true +
// this controller-id + cache=true. It is the only label scope the GC ever
// operates in, so a non-cache (job-scoped or foreign) or other-controller volume
// is never even listed — never mind removed. A consequence of the cache NAMES
// omitting controller-id (ADR-003) is that a volume first created (and thus
// LABELED) by another controller carries that controller's id and so is invisible
// to this controller's GC — documented in ADR-003.
func (m *Manager) cacheVolumeFilter() filters.Args {
	return filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+m.controllerID),
		filters.Arg("label", spec.LabelCache+"=true"),
	)
}

// CacheEviction records one evicted cache volume for the GC log.
type CacheEviction struct {
	Name   string
	Reason string
}

// EvictCaches lists this controller's cache volumes and removes those `decide`
// returns (true, reason) for, best-effort and bounded by maxEvictions (a stale
// backlog is simply caught over successive opportunistic passes). It is the
// removal half of ADR-003's opportunistic GC; `decide` is
// spec.EvaluateCacheEviction bound to the current config + clock by the provider.
//
// It NEVER force-removes: a cache volume mounted into a running container returns
// a Conflict ("volume is in use") which is treated as SKIP, so GC can never yank
// a warm cache out from under a live runner — an active job pins its own cache.
// NotFound is tolerated (a concurrent removal won the race). Every candidate is
// re-checked to be a cache volume for THIS controller before `decide` is even
// consulted, defense-in-depth on top of the label filter, so this can only ever
// touch this controller's own cache=true volumes and never a job-scoped or
// foreign resource.
func (m *Manager) EvictCaches(ctx context.Context, decide func(labels map[string]string) (bool, string), maxEvictions int) ([]CacheEviction, error) {
	list, err := m.cli.VolumeList(ctx, volume.ListOptions{Filters: m.cacheVolumeFilter()})
	if err != nil {
		return nil, fmt.Errorf("cache GC: failed to list cache volumes: %w", err)
	}

	var (
		evicted []CacheEviction
		errs    []error
	)
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		if maxEvictions > 0 && len(evicted) >= maxEvictions {
			log.Printf("garm-provider-docker: cache GC: hit the per-pass eviction cap (%d); remaining stale caches will be reaped on a later pass", maxEvictions)
			break
		}
		// Defense-in-depth re-assertion: only THIS controller's cache volumes.
		if v.Labels[spec.LabelCache] != "true" || v.Labels[spec.LabelControllerID] != m.controllerID {
			continue
		}
		evict, reason := decide(v.Labels)
		if !evict {
			continue
		}
		// force=false so an in-use (mounted) cache is protected — the daemon
		// returns Conflict, which we SKIP rather than escalate.
		if err := m.cli.VolumeRemove(ctx, v.Name, false); err != nil {
			switch {
			case errdefs.IsNotFound(err):
				// Raced with another removal — already gone.
			case errdefs.IsConflict(err):
				log.Printf("garm-provider-docker: cache GC: %q is in use (a live job holds it); skipping", v.Name)
			default:
				errs = append(errs, fmt.Errorf("cache GC: failed to evict %q: %w", v.Name, err))
			}
			continue
		}
		evicted = append(evicted, CacheEviction{Name: v.Name, Reason: reason})
	}
	return evicted, errors.Join(errs...)
}

// ListDiagVolumes returns the names of this controller's diagnostic-logs cache
// volumes (cache=true + cache-kind=diag-logs), so the provider can run its
// retention prune helper against each (ADR-003 F14 keeps retention provider-side,
// out of the untrusted runner). It re-asserts the kind + controller on each hit,
// defense-in-depth on top of the label filter.
func (m *Manager) ListDiagVolumes(ctx context.Context) ([]string, error) {
	f := m.cacheVolumeFilter()
	f.Add("label", spec.LabelCacheKind+"="+string(spec.CacheKindDiagLogs))

	list, err := m.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return nil, fmt.Errorf("cache GC: failed to list diag volumes: %w", err)
	}
	var names []string
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		if v.Labels[spec.LabelCacheKind] != string(spec.CacheKindDiagLogs) || v.Labels[spec.LabelControllerID] != m.controllerID {
			continue
		}
		names = append(names, v.Name)
	}
	return names, nil
}
