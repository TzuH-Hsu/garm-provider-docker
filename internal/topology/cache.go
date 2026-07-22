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
	// Name is the cache volume's name (echoed back for convenience).
	Name string

	// Hit reports whether the volume already existed (a cache HIT — its
	// contents are reused) as opposed to being freshly created this call (a
	// MISS — an empty volume the first job will populate).
	Hit bool
}

// maxCacheReconcileAttempts bounds how many alternate slots EnsureCacheVolume
// will try when the preferred deterministic name — and successive
// deterministic alternates — are each occupied by a foreign/unlabeled/
// wrong-identity squatter. Reaching it means an implausible number of distinct
// non-ours volumes squat our deterministic-and-hashed names on one host; the
// allocation then fails (GARM retries) rather than the provider deleting anything
// it cannot prove is ours (ADR-003 never-delete-unprovable).
const maxCacheReconcileAttempts = 8

// EnsureCacheVolume create-or-reuses a persistent cache volume under ADR-003's
// structural redesign (2026-07-22): a cache's identity is its LABEL SET, not its
// name; the name is just a SLOT. This is fundamentally DIFFERENT from
// createFreshVolume (create.go): a job-scoped volume must be guaranteed fresh
// per allocation, whereas a cache is MEANT to outlive allocations — its contents
// must be preserved, never removed-and-recreated.
//
// Two-phase, and it NEVER deletes a volume it cannot POSITIVELY prove is ours:
//
//  1. DISCOVER BY LABEL. If a volume carrying our FULL identity already exists
//     (spec.CacheIdentityFilter — managed+cache + cache-kind + repo/repo-url-digest
//     or image-digest + generation/pnpm-major), reuse it under WHATEVER name it
//     lives at — including an alternate a prior reconcile chose. This is the cache
//     HIT. Discovery-by-label is what makes reconciled alternates transparently
//     reusable: the name the caller PREFERRED is irrelevant once a labeled cache
//     for this identity exists.
//
//  2. CLAIM A FREE-OR-OURS SLOT, reconciling AROUND a squatter. With no
//     label-match, VolumeCreate the preferred name (idempotent on a duplicate:
//     the real daemon returns the EXISTING volume with its ORIGINAL labels,
//     discarding this call's — verified on Engine 29.6.1). If the returned volume
//     validates as ours, adopt it (a fresh create, or a concurrent peer that
//     created the identical-identity volume first). If it does NOT — the slot is
//     held by a foreign volume, a Moby auto-created UNLABELED volume, or a
//     DIFFERENT cache that collided on the name — do NOT delete it (a Moby
//     auto-created unlabeled volume and a genuinely foreign one are
//     INDISTINGUISHABLE, so name-based deletion can never be foreign-safe — B2)
//     and do NOT wedge forever on the occupied name (B3): route AROUND it to a
//     deterministic alternate (spec.AlternateCacheVolumeName) and re-check the
//     alternate the same way. Because discovery is by label (phase 1), the
//     alternate-named volume is found and reused on the next allocation.
//
// The idempotent cross-controller/cross-repo sharing path is UNCHANGED — two
// controllers issuing VolumeCreate on the same deterministic name both get the
// same volume and validate it as ours; reconciliation triggers ONLY on a genuine
// foreign/unlabeled/wrong-identity squatter.
//
// last-used is deliberately NOT re-stamped on a hit: the daemon cannot mutate a
// local volume's labels after creation (re-VolumeCreate keeps the original
// labels; `docker volume update` is cluster-only), and removing-and-recreating to
// refresh a label would destroy the cache. The label records the volume's
// creation instant; W2's opportunistic GC ages a warm cache by AGE-since-creation
// plus SALT SUPERSESSION (spec/gc.go) — NOT by filesystem mtime, which the
// provider cannot portably stat from outside the Docker Desktop VM.
//
// The provider (create path) builds `preferredName` and `labels` from the
// spec.CacheVolumeIdentity/ToolcacheVolumeName/PnpmVolumeName builders and calls
// this once per cache kind; keeping this helper name+labels-generic keeps the
// toolcache-vs-pnpm shaping in one place there.
func (m *Manager) EnsureCacheVolume(ctx context.Context, preferredName string, labels map[string]string) (CacheVolumeResult, error) {
	// Phase 1: discover our cache by IDENTITY, under any name.
	if name, found, err := m.discoverCacheByIdentity(ctx, labels); err != nil {
		return CacheVolumeResult{}, err
	} else if found {
		return CacheVolumeResult{Name: name, Hit: true}, nil
	}

	// Phase 2: claim a free-or-ours slot, reconciling around any squatter.
	candidate := preferredName
	for attempt := 1; ; attempt++ {
		created, err := m.cli.VolumeCreate(ctx, volume.CreateOptions{Name: candidate, Labels: labels})
		if err != nil {
			return CacheVolumeResult{}, fmt.Errorf("failed to ensure cache volume %q: %w", candidate, err)
		}
		verr := spec.ValidateAdoptedCacheVolume(candidate, created.Labels, labels)
		if verr == nil {
			// Freshly created, or a concurrent peer created the identical-identity
			// volume first — either way it is ours to use. (A concurrent first-create
			// race reporting a MISS here is harmless: the bool is informational.)
			return CacheVolumeResult{Name: candidate, Hit: false}, nil
		}
		if attempt >= maxCacheReconcileAttempts {
			return CacheVolumeResult{}, fmt.Errorf("cache slot %q and %d deterministic alternates are each occupied by a volume that is not our validated cache; refusing to delete any of them (ADR-003 never-delete-unprovable) and failing this allocation so GARM retries: %w", preferredName, attempt-1, verr)
		}
		// The slot is occupied by a volume we cannot prove is ours. Leave it
		// UNTOUCHED and reconcile to a deterministic alternate (B2/B3).
		slog.WarnContext(ctx, "cache slot occupied by a volume that is not our validated cache; reconciling to an alternate name",
			"resource", "cache-volume", "volume", candidate, "error", verr)
		next := spec.AlternateCacheVolumeName(preferredName, attempt)
		if err := spec.ValidateDerivedName("reconciled cache volume", next); err != nil {
			return CacheVolumeResult{}, fmt.Errorf("cache reconcile for %q could not derive a valid alternate name: %w", preferredName, err)
		}
		candidate = next
	}
}

// discoverCacheByIdentity finds a cache volume carrying our FULL identity
// (ADR-003 label-as-identity), regardless of the NAME it lives under — so a cache
// a prior allocation reconciled onto an alternate name is still found and reused.
// It lists by the identity label filter (spec.CacheIdentityFilter) and
// re-validates each candidate with spec.ValidateAdoptedCacheVolume, returning the
// matching volume's ACTUAL name. When several match (a pathological duplicate from
// two peers reconciling around the same squatter concurrently), the
// lexicographically smallest name is chosen — a stable, deterministic pick, and
// the rare duplicate is harmless (both are valid caches for this identity; one
// wins going forward). It deliberately does NOT filter on controller-id
// (cross-controller shared caches are expected — ADR-003 W1/W2).
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
		// Defense-in-depth on top of the daemon's filter: only adopt a volume that
		// passes the exact identity check.
		if spec.ValidateAdoptedCacheVolume(v.Name, v.Labels, want) != nil {
			continue
		}
		if best == "" || v.Name < best {
			best = v.Name
		}
	}
	return best, best != "", nil
}
