package topology

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// This file holds the topology side of ADR-003's opportunistic cache GC (M2-W2),
// now NON-DESTRUCTIVE for cache volumes (NEW-H1): listing this controller's
// stale/superseded/aged cache volumes for the provider to LOG (never remove),
// plus enumerating the diagnostic-logs volumes the provider runs its file-prune
// helper against (the one remaining destructive cache action — it deletes FILES
// inside a volume, strict-identity-gated, not the volume itself). The staleness
// DECISION (age/supersession) lives in spec.EvaluateCacheEviction — a pure
// function the provider threads in here — so this layer stays about the
// label-scoped listing and strict-identity validation.
//
// Why cache-volume eviction is log-only (NEW-H1): Docker's VolumeRemove is BY
// NAME with no atomic label-qualified variant, and you cannot pin-then-delete a
// volume (a referencing container blocks removal), so a client-side
// inspect-then-delete-by-name is inherently TOCTOU — a concurrent delete plus a
// foreign volume taking the freed name means a by-name VolumeRemove could delete
// a FOREIGN volume (a red-line violation). Another inspection only narrows the
// race; it cannot close it. There is therefore no safe client-side atomic
// destructive path for a cache volume, so the provider never auto-deletes one;
// caches persist until an EXPLICIT operator purge (documented in ADR-003, and
// surfaced with the exact `docker volume prune` command in the provider's
// stale-cache log).

// cacheVolumeFilter selects THIS controller's cache volumes: managed=true +
// this controller-id + cache=true. It is the only label scope the GC ever
// operates in, so a non-cache (job-scoped or foreign) or other-controller volume
// is never even listed. A consequence of the cache NAMES omitting controller-id
// (ADR-003) is that a volume first created (and thus LABELED) by another
// controller carries that controller's id and so is invisible to this
// controller's GC — documented in ADR-003.
func (m *Manager) cacheVolumeFilter() filters.Args {
	return filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+m.controllerID),
		filters.Arg("label", spec.LabelCache+"=true"),
	)
}

// StaleCache records one stale/superseded/aged cache volume the LOG-ONLY GC
// surfaces for operator visibility (NEW-H1). Name is the volume's name; Reason is
// the short human string from the staleness decision (why it is stale). Nothing
// is removed — the provider LOGS these so an operator can reclaim disk with a
// deliberate purge.
type StaleCache struct {
	Name   string
	Reason string
}

// ListStaleCaches enumerates this controller's cache volumes that `decide` flags
// as stale/superseded/aged AND that pass STRICT kind-aware full-identity
// validation, returning them for the provider to LOG. It removes NOTHING.
//
// ADR-003's opportunistic cache-volume GC is deliberately NON-DESTRUCTIVE
// (NEW-H1) — see this file's header for why a client-side by-name VolumeRemove is
// inherently TOCTOU/foreign-unsafe for a volume. Caches persist until an explicit
// operator purge. `decide` is spec.EvaluateCacheEviction bound to the current
// config + clock by the provider; the strict validation (spec.ValidateCacheVolumeKind
// plus a controller-id re-assertion) keeps the log ACCURATE: only a volume
// carrying the FULL per-kind identity is reported as ours to prune, so an
// incomplete/foreign volume that slipped the label filter is never named.
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
		// Defense-in-depth on the label filter: only THIS controller's cache
		// volumes carrying the FULL per-kind identity are ours to report.
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

// DiagVolumeRef is one diagnostic-logs cache volume returned by ListDiagVolumes:
// its NAME plus the IDENTITY LABELS captured at snapshot time. The provider
// carries the snapshot labels into the prune helper's pin-then-validate step so it
// can re-assert the FULL identity (managed+cache+controller+cache-kind+repo+
// repo-url-digest) of the volume the helper actually pinned — catching a
// reincarnated/foreign same-name diag volume that a name-and-kind-only check would
// miss (spec.ValidateDiagPruneTarget).
type DiagVolumeRef struct {
	Name   string
	Labels map[string]string
}

// ListDiagVolumes returns this controller's diagnostic-logs cache volumes
// (cache=true + cache-kind=diag-logs) with their identity labels, so the provider
// can run its retention prune helper against each (ADR-003 F14 keeps retention
// provider-side, out of the untrusted runner) and re-validate the FULL identity of
// the volume the helper pins. It applies the STRICT kind-aware full-identity
// validation (NEW-H2) at enumeration — a diag volume missing repo/repo-url-digest
// is skipped — so the snapshot the destructive prune re-validates against is
// itself complete, and re-asserts the kind + controller on each hit,
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
		// STRICT full-identity gate (NEW-H2): only a complete diag-logs volume
		// (repo + repo-url-digest present) is a valid prune target. An
		// incomplete/unlabeled volume that squats the diag name is skipped here so
		// the destructive prune never enumerates it in the first place.
		if err := spec.ValidateCacheVolumeKind(v.Labels); err != nil {
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
