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
//
// H3 (stale-name TOCTOU): the list above is a snapshot. Between it and the
// physical removal, a remove/recreate-under-the-same-name could replace a stale
// cache with a FRESH foreign, or a brand-new same-repo, volume that a by-name
// VolumeRemove would then wrongly delete. So immediately before removing, the
// volume is RE-INSPECTED by name and its FULL ownership+cache labels re-validated
// and the eviction decision re-run against those live labels — a candidate that
// is no longer ours, or no longer evictable, is skipped.
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
		// Defense-in-depth re-assertion on the SNAPSHOT labels: only THIS
		// controller's cache volumes, and only ones the snapshot flags as evictable
		// (a cheap pre-filter so we re-inspect only genuine candidates).
		if v.Labels[spec.LabelCache] != "true" || v.Labels[spec.LabelControllerID] != m.controllerID {
			continue
		}
		if evict, _ := decide(v.Labels); !evict {
			continue
		}

		// RE-VALIDATE against LIVE labels immediately before removing by name.
		fresh, err := m.cli.VolumeInspect(ctx, v.Name)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue // raced with a removal — already gone
			}
			errs = append(errs, fmt.Errorf("cache GC: failed to re-inspect %q before eviction: %w", v.Name, err))
			continue
		}
		if fresh.Labels[spec.LabelManaged] != "true" || fresh.Labels[spec.LabelCache] != "true" || fresh.Labels[spec.LabelControllerID] != m.controllerID {
			// A different/foreign volume now holds this name (remove+recreate since
			// the snapshot); do NOT delete it. The full ownership tuple is asserted
			// at this destructive boundary — managed=true AND cache=true AND this
			// controller-id (H3c): an UNLABELED auto-created replacement (a
			// concurrent evict+ContainerCreate race that stripped the labels) is
			// missing managed=true, so re-checking only cache/controller-id here
			// would let an auto-created volume that somehow carried a stray
			// cache=true slip through — the managed conjunct closes that.
			continue
		}
		evict, reason := decide(fresh.Labels)
		if !evict {
			// A freshly re-created same-name cache that is no longer evictable.
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

// DiagVolumeRef is one diagnostic-logs cache volume returned by ListDiagVolumes:
// its NAME plus the IDENTITY LABELS captured at snapshot time. The provider
// carries the snapshot labels into the prune helper's pin-then-validate step so it
// can re-assert the FULL identity (managed+cache+controller+cache-kind+repo+
// repo-url-digest) of the volume the helper actually pinned — catching a
// reincarnated/foreign same-name diag volume that a name-and-kind-only check would
// miss (2026-07-22 structural amendment; the earlier check validated
// controller/kind but not repo/repo-url-digest).
type DiagVolumeRef struct {
	Name   string
	Labels map[string]string
}

// ListDiagVolumes returns this controller's diagnostic-logs cache volumes
// (cache=true + cache-kind=diag-logs) with their identity labels, so the provider
// can run its retention prune helper against each (ADR-003 F14 keeps retention
// provider-side, out of the untrusted runner) and re-validate the FULL identity of
// the volume the helper pins. It re-asserts the kind + controller on each hit,
// defense-in-depth on top of the label filter.
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
		refs = append(refs, DiagVolumeRef{Name: v.Name, Labels: cloneLabelMap(v.Labels)})
	}
	return refs, nil
}

// cloneLabelMap returns a shallow copy of a label map so a snapshot's identity
// cannot be mutated by a later daemon call reusing the same backing map.
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

// ListUnprovableCacheCruft returns the names of volumes carrying this provider's
// cache NAME prefix (garm-cache-) that do NOT carry our cache=true label — i.e.
// unlabeled Moby auto-created replacements or foreign volumes squatting a
// cache-shaped name. Under ADR-003's never-delete-unprovable rule the provider
// will NOT delete these (a Moby auto-created unlabeled volume and a genuinely
// foreign one are indistinguishable), but it surfaces them here so the
// opportunistic GC can LOG them for operator visibility. It lists ALL volumes
// (best-effort) and matches by name prefix; it never removes anything.
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
			continue // a provably-ours (or shared) cache — not cruft
		}
		cruft = append(cruft, v.Name)
	}
	return cruft, nil
}
