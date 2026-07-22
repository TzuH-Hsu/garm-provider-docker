package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/filters"
)

// This file holds ADR-003's persistent-cache pure functions (M2-W1): the
// repo-URL cache key, the entity-scope classifier that decides whether a pool
// is eligible for repo-scoped caches at all, and the cache-volume name/label
// builders. Like the rest of package spec these are side-effect-free — no
// Docker calls, no time.Now(), no I/O — so they are cheap to table-test, and
// the caller (the provider create path) supplies the last-used timestamp.

// repoKeyHashLen is the number of leading hex characters of the repo-URL
// SHA-256 that form the hash suffix of a repokey. ADR-003 fixes a MINIMUM of 12
// ("the first 12 hex characters (minimum)"); L7 raises it to 32 hex = 128 bits,
// so the truncated key is collision-resistant to a cryptographic degree rather
// than merely "sufficient in practice" at 48 bits. The FULL normalized-URL digest
// still rides along as a label (RepoURLDigest / LabelRepoURLDigest), validated on
// every cache reuse (topology.EnsureCacheVolume), so even an astronomically
// unlikely 128-bit truncation collision cannot cause one repo to adopt another's
// cache — the reuse is rejected closed when the full-digest label disagrees.
const repoKeyHashLen = 32

// repoSlugMaxLen bounds the human-readable slug half of a repokey. The hash
// suffix guarantees uniqueness, so the slug is purely for operator legibility
// ("this repo's cache"); capping it keeps the final cache volume name well
// within Docker's 255-byte limit even for a deeply-nested path.
const repoSlugMaxLen = 40

// RepoKey derives ADR-003's stable cache key from a bootstrap payload's
// repo_url: <slug>-<hash>, where <hash> is the first repoKeyHashLen hex
// characters of sha256(normalizedRepoURL) and <slug> is a Docker-name-safe,
// human-readable rendering of the normalized URL's path.
//
// The hash is taken over the NORMALIZED URL, not the raw repo_url. ADR-003's
// Decision text writes "sha256(repo_url)", but its own normalization rule
// (lowercase, strip scheme, strip a trailing .git) exists precisely so that
// case/scheme/.git/trailing-slash variants of the same repository map to ONE
// cache; hashing the raw string would defeat that (github.com/Foo/Bar.git and
// github.com/foo/bar would share a slug but get different hashes, so different
// repokeys, so a cache MISS between two spellings of the same repo). Hashing
// the normalized form is the only reading consistent with the normalization's
// stated purpose. (Flagged: this resolves an ADR-003 wording ambiguity in
// favour of intent over the literal "repo_url".)
//
// The result is always non-empty and matches Docker's resource-name grammar
// ([a-z0-9] with interior dashes), so ToolcacheVolumeName/PnpmVolumeName built
// from it are valid names; ValidateDerivedName re-checks the final names
// defensively at the create site regardless.
func RepoKey(repoURL string) string {
	norm := normalizeRepoURL(repoURL)
	sum := sha256.Sum256([]byte(norm))
	hash := hex.EncodeToString(sum[:])[:repoKeyHashLen]

	slug := repoSlug(norm)
	if slug == "" {
		// No legible slug survived sanitisation (e.g. a path of only
		// punctuation): the hash alone is a valid, unique key.
		return hash
	}
	return slug + "-" + hash
}

// RepoURLDigest returns the FULL (64-hex, 256-bit) SHA-256 of the NORMALIZED
// repo URL — the same normalization RepoKey hashes, but untruncated. It is
// stamped as a label on every repo-keyed cache volume (LabelRepoURLDigest) and
// re-checked on every cache reuse, so a repokey truncation collision (two
// different repositories whose slug+128-bit-hash happen to coincide) cannot cause
// one repo to silently adopt the other's cache: the reuse fails closed when the
// full-digest labels disagree (L7, ties into topology's foreign-adoption guard).
func RepoURLDigest(repoURL string) string {
	sum := sha256.Sum256([]byte(normalizeRepoURL(repoURL)))
	return hex.EncodeToString(sum[:])
}

// normalizeRepoURL applies ADR-003's normalization: trim surrounding space,
// lowercase, strip the URL scheme, strip a trailing ".git", and strip trailing
// slashes (both before and after the ".git" trim, so "…/repo.git/" normalizes
// the same as "…/repo"). It deliberately does NOT parse the URL structurally —
// it operates on the string so a scheme-less or otherwise unusual repo_url
// still normalizes deterministically rather than erroring.
func normalizeRepoURL(repoURL string) string {
	s := strings.ToLower(strings.TrimSpace(repoURL))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimRight(s, "/")
	return s
}

// repoSlug renders the PATH portion of a normalized repo URL (everything after
// the host) as a Docker-name-safe slug. The host is dropped from the slug —
// it is constant for a given deployment, so "octo-org-octo-repo" reads better
// than "github-com-octo-org-octo-repo" — while the RepoKey hash (taken over the
// full normalized URL including host) still disambiguates two forges that
// happen to share a path.
func repoSlug(norm string) string {
	path := norm
	if i := strings.IndexByte(norm, '/'); i >= 0 {
		path = norm[i+1:]
	}
	return slugify(path)
}

// slugify lowercases (idempotent here — the input is already lowercased),
// collapses every run of non-[a-z0-9] characters to a single dash, trims
// leading/trailing dashes, and caps the length at repoSlugMaxLen (re-trimming
// dashes after the cut so the result never ends on a dash).
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > repoSlugMaxLen {
		out = strings.Trim(out[:repoSlugMaxLen], "-")
	}
	return out
}

// CacheEntityScope is the four-valued classification the cache path keys its
// eligibility decision on. It is DISTINCT from EntityScope (env.go): EntityScope
// is a three-value {repo,org,enterprise} result that ParseEntity returns
// alongside an error for anything it cannot parse, whereas CacheEntityScope
// folds "could not confidently classify" into an explicit CacheScopeUnknown, so
// the cache decision is a total function that fails SAFE (withholds persistent
// caches) rather than propagating a parse error into the create path.
type CacheEntityScope string

const (
	CacheScopeRepo       CacheEntityScope = "repo"
	CacheScopeOrg        CacheEntityScope = "org"
	CacheScopeEnterprise CacheEntityScope = "enterprise"
	CacheScopeUnknown    CacheEntityScope = "unknown"
)

// DetectCacheEntityScope classifies a repo_url's entity scope for the cache
// eligibility decision (ADR-003). It reuses ParseEntity's GitHub URL-shape
// heuristic (owner/repo = repo-scoped; a single path segment = org; an
// /enterprises/<name> shape = enterprise) and maps any parse failure — an
// empty path, an over-deep path, a malformed enterprise URL — to
// CacheScopeUnknown. A repo_url this provider cannot confidently place is
// treated as non-repo-scoped and, per CacheScopeAllowed, denied persistent
// caches: a fail-SAFE default (withhold), never fail-open (silently share).
func DetectCacheEntityScope(repoURL string) CacheEntityScope {
	ent, err := ParseEntity(repoURL)
	if err != nil {
		return CacheScopeUnknown
	}
	switch ent.Scope {
	case EntityRepo:
		return CacheScopeRepo
	case EntityOrg:
		return CacheScopeOrg
	case EntityEnterprise:
		return CacheScopeEnterprise
	default:
		return CacheScopeUnknown
	}
}

// CacheScopeAllowed reports whether an allocation with this entity scope may
// use ADR-003's persistent, repo-keyed toolcache/pnpm caches:
//
//   - repo scope            → always allowed (per-repo caches work as designed);
//   - org / enterprise scope → allowed ONLY when the operator has opted in via
//     [cache].allow_org_shared=true — this treats the whole org/enterprise as a
//     single trust domain for cache purposes, keyed on the shared org/enterprise
//     URL (ADR-003's documented, deliberate operator trade-off);
//   - unknown scope          → NEVER allowed, even with allow_org_shared: an
//     unparseable repo_url has no confident identity to key a cache on, so the
//     fail-safe is to withhold unconditionally rather than key a shared cache on
//     a URL the provider could not classify.
//
// This is the ONE place the repo-vs-org/enterprise eligibility rule lives, so
// the provider create path cannot get it subtly wrong at one call site.
func CacheScopeAllowed(scope CacheEntityScope, allowOrgShared bool) bool {
	switch scope {
	case CacheScopeRepo:
		return true
	case CacheScopeOrg, CacheScopeEnterprise:
		return allowOrgShared
	default: // CacheScopeUnknown
		return false
	}
}

// cacheIdentityLabelKeys are the label keys that DISCRIMINATE one cache volume's
// identity from another's. On a cache reuse the daemon's idempotent VolumeCreate
// returns the EXISTING volume's ORIGINAL labels; if any of these keys that the
// EXPECTED set specifies disagrees with the returned set, the existing volume is
// a DIFFERENT cache (a repokey/name collision) or a foreign volume, and must not
// be adopted. It deliberately EXCLUDES controller-id and last-used: cache volumes
// are shared across controllers (ADR-003 W1 point 4 / W2 point 1), so a peer
// controller's id and a different creation timestamp are EXPECTED on a legitimate
// shared reuse and must not trip the guard.
var cacheIdentityLabelKeys = []string{
	LabelCacheKind,
	LabelRepo,
	LabelRepoURLDigest,
	LabelImageDigest,
	LabelGeneration,
	LabelPnpmMajor,
}

// ValidateAdoptedCacheVolume checks that a cache volume returned by an idempotent
// VolumeCreate (its `got` labels) is actually OURS to reuse, given the labels we
// asked for (`want`). It requires the managed+cache markers and agreement with
// `want` on every identity key `want` sets. A foreign volume that squatted the
// deterministic name (no cache=true — M6), or a genuinely different cache that
// collided on the repokey/name (a mismatching repo-url-digest or image-digest —
// L7), is REJECTED so the provider fails CLOSED rather than mounting someone
// else's data as this repo's writable cache, or — for externals — running
// preexisting content as executable runtime. Intended cross-controller reuse of a
// legitimately shared cache still passes, because controller-id is not an identity
// key; only explicit semantic-label agreement permits adoption.
func ValidateAdoptedCacheVolume(name string, got, want map[string]string) error {
	if got[LabelManaged] != "true" || got[LabelCache] != "true" {
		return fmt.Errorf("cache volume %q already exists but is not a managed cache volume (managed=%q cache=%q); refusing to adopt a foreign volume as a cache", name, got[LabelManaged], got[LabelCache])
	}
	for _, k := range cacheIdentityLabelKeys {
		w := want[k]
		if w == "" {
			continue
		}
		if got[k] != w {
			return fmt.Errorf("cache volume %q already exists but its %s=%q does not match the expected %q; refusing to adopt a different cache under a colliding name", name, k, got[k], w)
		}
	}
	return nil
}

// requiredCacheIdentityKeys returns the identity label keys a cache volume of
// this kind MUST carry (beyond the managed+cache markers) to be provably ours —
// the STRICT schema behind ValidateCacheVolumeKind (NEW-H2). It is the full
// per-kind identity of ADR-003:
//
//   - toolcache: cache-kind + repo + repo-url-digest + generation
//   - pnpm:      cache-kind + repo + repo-url-digest + pnpm-major
//   - diag-logs: cache-kind + repo + repo-url-digest
//   - externals: cache-kind + image-digest (shared across repos, so NO repo)
//
// An unrecognised or absent cache-kind returns nil, which ValidateCacheVolumeKind
// treats as "not provably ours" and rejects.
func requiredCacheIdentityKeys(kind CacheKind) []string {
	switch kind {
	case CacheKindToolcache:
		return []string{LabelRepo, LabelRepoURLDigest, LabelGeneration}
	case CacheKindPnpm:
		return []string{LabelRepo, LabelRepoURLDigest, LabelPnpmMajor}
	case CacheKindDiagLogs:
		return []string{LabelRepo, LabelRepoURLDigest}
	case CacheKindExternals:
		return []string{LabelImageDigest}
	default:
		return nil
	}
}

// ValidateCacheVolumeKind is the STRICT, kind-aware full-identity check the cache
// subsystem applies before ANY destructive cache work (the diag file-prune) and
// when enumerating cache volumes for the log-only GC, so a foreign or
// incomplete-identity volume is never acted on or reported as ours.
//
// Unlike ValidateAdoptedCacheVolume — which SKIPS any identity key the caller's
// `want` set does not specify (the right rule for the reuse-adoption path, where a
// spec-only builder may legitimately omit repo-url-digest) — this REQUIRES every
// identity key the volume's own cache-kind demands to be PRESENT and non-empty. A
// volume that is not managed+cache, carries an unknown/absent cache-kind, or is
// missing any required key for its kind FAILS validation: it is not provably ours.
// It compares only the volume's OWN labels (no expected set), so it is the check
// used where there is no `want` to compare against — the log-only enumeration and
// the destructive diag-prune's pin re-inspect.
func ValidateCacheVolumeKind(labels map[string]string) error {
	if labels[LabelManaged] != "true" || labels[LabelCache] != "true" {
		return fmt.Errorf("volume is not a managed cache volume (managed=%q cache=%q)", labels[LabelManaged], labels[LabelCache])
	}
	kind := CacheKind(labels[LabelCacheKind])
	required := requiredCacheIdentityKeys(kind)
	if required == nil {
		return fmt.Errorf("cache volume has unknown or missing cache-kind %q", labels[LabelCacheKind])
	}
	for _, k := range required {
		if labels[k] == "" {
			return fmt.Errorf("cache volume of kind %q is missing required identity label %q", kind, k)
		}
	}
	return nil
}

// ValidateDiagPruneTarget is the STRICT identity gate the diagnostic-log file
// prune runs against the volume its helper actually PINNED, immediately before
// the destructive `find -delete` (NEW-H2 — the one remaining destructive cache
// action; it deletes FILES inside the diag volume, not the volume). The pinned
// volume must:
//
//   - pass the strict kind-aware full-identity check (ValidateCacheVolumeKind);
//   - be a diag-logs volume for THIS controller; and
//   - match the enumeration SNAPSHOT's repo + repo-url-digest identity,
//
// so a same-name replacement of a DIFFERENT repo, an unlabeled Moby auto-created
// volume, or a foreign volume is REJECTED and the prune aborts WITHOUT deleting
// anything (never delete/prune an unprovable volume — B2). snapshot is the
// DiagVolumeRef label set captured when the volume was enumerated (itself strict-
// validated at enumeration, so its repo/repo-url-digest are present).
func ValidateDiagPruneTarget(got, snapshot map[string]string, controllerID string) error {
	if err := ValidateCacheVolumeKind(got); err != nil {
		return fmt.Errorf("diag prune target failed strict identity validation: %w", err)
	}
	if got[LabelCacheKind] != string(CacheKindDiagLogs) {
		return fmt.Errorf("diag prune target is cache-kind %q, not %q", got[LabelCacheKind], CacheKindDiagLogs)
	}
	if got[LabelControllerID] != controllerID {
		return fmt.Errorf("diag prune target controller-id %q is not this controller %q", got[LabelControllerID], controllerID)
	}
	for _, k := range []string{LabelRepo, LabelRepoURLDigest} {
		if got[k] != snapshot[k] {
			return fmt.Errorf("diag prune target %s=%q does not match the enumerated snapshot %q (a same-name replacement since the snapshot)", k, got[k], snapshot[k])
		}
	}
	return nil
}

// CacheIdentityFilter builds the Docker label filter that discovers a cache
// volume by its IDENTITY, the load-bearing primitive of ADR-003's structural
// redesign (2026-07-22): a cache's identity is its LABEL SET, not its name. It
// selects managed=true + cache=true plus every identity-discriminating label
// present in `want` (cacheIdentityLabelKeys — cache-kind, repo, repo-url-digest,
// image-digest, generation, pnpm-major). Because Docker ANDs multiple same-key
// "label" filters (MatchKVList), a volume matches only if it carries our FULL
// identity — so discovery finds OUR cache under WHATEVER name it lives at
// (including an alternate a prior reconcile chose), never a foreign or
// wrong-identity volume.
//
// It deliberately OMITS controller-id and last-used: cache volumes are shared
// across controllers (ADR-003 W1/W2), so a peer controller's id and a different
// creation timestamp are EXPECTED on a legitimate shared reuse; keying discovery
// on either would miss a legitimately shared cache. The caller
// (topology.EnsureCacheVolume) still re-validates each returned volume with
// ValidateAdoptedCacheVolume, defense-in-depth against the daemon's filter.
func CacheIdentityFilter(want map[string]string) filters.Args {
	f := filters.NewArgs(
		filters.Arg("label", LabelManaged+"=true"),
		filters.Arg("label", LabelCache+"=true"),
	)
	for _, k := range cacheIdentityLabelKeys {
		if v := want[k]; v != "" {
			f.Add("label", k+"="+v)
		}
	}
	return f
}

// cacheReconcileSuffixLen is the hex length of the deterministic,
// collision-avoiding suffix AlternateCacheVolumeName appends. 8 hex = 32 bits,
// ample to route around a handful of squatters on one host while keeping the
// reconciled name well within Docker's 255-byte resource-name limit.
const cacheReconcileSuffixLen = 8

// AlternateCacheVolumeName derives a DETERMINISTIC, collision-avoiding alternate
// name for a cache volume whose preferred deterministic slot is occupied by a
// volume that is NOT our validated cache (a foreign, unlabeled, or wrong-identity
// squatter). It implements ADR-003's never-delete-unprovable rule: rather than
// delete the squatter (a Moby auto-created unlabeled volume and a genuinely
// foreign volume are INDISTINGUISHABLE, so name-based deletion can never be
// foreign-safe — B2) or wedge forever on the occupied name (B3), the provider
// routes AROUND the squatter to this alternate. Because cache discovery is by
// LABEL (CacheIdentityFilter), a volume created under the alternate is still
// found and reused on the next allocation.
//
// The suffix is sha256(preferred + ":" + attempt) truncated:
//   - DETERMINISTIC — the same preferred+attempt always yields the same
//     alternate, so two provider processes reconciling around the same squatter
//     converge on ONE shared slot rather than fragmenting the cache;
//   - COLLISION-AVOIDING — a later attempt yields a different slot when an
//     alternate is ALSO squatted (EnsureCacheVolume re-checks each the same way);
//   - Docker-name-safe — [a-z0-9-] only. attempt is 1-based.
func AlternateCacheVolumeName(preferred string, attempt int) string {
	sum := sha256.Sum256([]byte(preferred + ":" + strconv.Itoa(attempt)))
	return preferred + "-x" + hex.EncodeToString(sum[:])[:cacheReconcileSuffixLen]
}

// cacheVolumeNamePrefix is the shared prefix for every cache volume, so a
// label-blind operator can still spot cache volumes by name (`docker volume ls`
// | grep garm-cache-). ADR-003's authoritative selector is still the label set,
// not the name.
const cacheVolumeNamePrefix = "garm-cache-"

// HasCacheVolumeNamePrefix reports whether name carries this provider's cache
// volume name prefix (garm-cache-). The GC's cruft-visibility log uses it to spot
// unlabeled/foreign volumes squatting a cache-shaped name that the provider will
// NOT delete (ADR-003 never-delete-unprovable) but surfaces for operator
// awareness. It is a NAME heuristic only; ownership is always decided by labels.
func HasCacheVolumeNamePrefix(name string) bool {
	return strings.HasPrefix(name, cacheVolumeNamePrefix)
}

// CacheKind names the persistent cache kinds this provider provisions
// (ADR-003). Toolcache and pnpm are repo-scoped (M2-W1); externals is
// image-digest-scoped and diag-logs is repo-scoped (M2-W2).
type CacheKind string

const (
	CacheKindToolcache CacheKind = "toolcache"
	CacheKindPnpm      CacheKind = "pnpm"

	// CacheKindExternals is the W2 externals cache: the runner's Node runtimes
	// (research.md §3, ~380MB), keyed by the runner IMAGE digest and shared
	// across every repository (its contents carry no repo data). Mounted
	// READ-ONLY into the runner and seeded once by a privileged init step —
	// externals.go.
	CacheKindExternals CacheKind = "externals"

	// CacheKindDiagLogs is the W2 diagnostic-logs cache: a per-repo volume at the
	// runner's _diag dir so runner diagnostic logs persist across jobs, pruned
	// provider-side to a retention window (never in the untrusted runner
	// entrypoint) — diag.go.
	CacheKindDiagLogs CacheKind = "diag-logs"
)

// ToolcacheVolumeName builds the toolcache volume name (ADR-003):
// garm-cache-toolcache-<repokey>-<generation>. The <generation> salt bumps
// whenever the image generation changes, giving a clean way to invalidate a
// stale toolcache without an explicit GC pass (a new generation is simply a
// new, empty volume).
func ToolcacheVolumeName(repoKey, generation string) string {
	return cacheVolumeNamePrefix + string(CacheKindToolcache) + "-" + repoKey + "-" + generation
}

// PnpmVolumeName builds the pnpm store volume name (ADR-003):
// garm-cache-pnpm-<repokey>-<pnpmMajor>. The <pnpmMajor> salt bumps on a pnpm
// major-version change, since the store's on-disk layout is tied to the pnpm
// major (the store's own vN directory), so a major bump wants a fresh volume.
func PnpmVolumeName(repoKey, pnpmMajor string) string {
	return cacheVolumeNamePrefix + string(CacheKindPnpm) + "-" + repoKey + "-" + pnpmMajor
}

// CacheVolumeIdentity is the identity every cache-volume label set is derived
// from. Unlike AllocationIdentity (job-scoped: controller + pool + instance),
// a cache volume is scoped to a controller and a repokey ONLY — deliberately
// no pool-id and, critically, no instance-name: a cache outlives every
// individual allocation, and its lack of an instance-name label is exactly what
// excludes it from ADR-004's teardown/orphan-sweep predicate (MatchesPredicate).
type CacheVolumeIdentity struct {
	ControllerID string
	RepoKey      string

	// RepoURLDigest is the FULL normalized-URL SHA-256 (RepoURLDigest), carried
	// as LabelRepoURLDigest so a cache reuse can re-verify it is the SAME
	// repository — not a different one that collided on the truncated repokey
	// (L7). The provider sets it from the same repo_url it derives RepoKey from;
	// when empty (a spec-only unit test), the digest label is simply omitted.
	RepoURLDigest string
}

// baseCacheLabels returns the labels every cache volume carries regardless of
// kind (ADR-003): managed=true (so it is this provider's, for the eventual
// controller-scoped cache purge), controller-id (so multiple controllers on one
// host keep separate caches), cache=true (the ADR-004 exclusion marker),
// cache-kind, repo=<repokey>, and a CREATION-time last-used timestamp. It
// deliberately omits instance-name, pool-id, resource, and create-nonce — none
// of the job-scoped labels — so no teardown/sweep/rollback predicate can ever
// match a cache volume.
//
// lastUsed is supplied by the caller (never time.Now() here), keeping this a
// pure builder. Per the daemon's local-volume label immutability (see
// LabelLastUsed's doc in labels.go), this timestamp is fixed at creation; it is
// not re-stamped on a cache hit.
func (c CacheVolumeIdentity) baseCacheLabels(kind CacheKind, lastUsed time.Time) map[string]string {
	labels := map[string]string{
		LabelManaged:      "true",
		LabelControllerID: c.ControllerID,
		LabelCache:        "true",
		LabelCacheKind:    string(kind),
		LabelRepo:         c.RepoKey,
		LabelLastUsed:     lastUsed.UTC().Format(time.RFC3339),
	}
	// The full normalized-URL digest (L7) rides along so a reuse can verify the
	// repository identity beyond the truncated repokey. Omitted when unset so a
	// spec-only builder without the URL still produces a valid label set.
	if c.RepoURLDigest != "" {
		labels[LabelRepoURLDigest] = c.RepoURLDigest
	}
	return labels
}

// ToolcacheLabels returns the label set for a toolcache volume (ADR-003): the
// common cache labels plus generation=<gen>.
func (c CacheVolumeIdentity) ToolcacheLabels(generation string, lastUsed time.Time) map[string]string {
	labels := c.baseCacheLabels(CacheKindToolcache, lastUsed)
	labels[LabelGeneration] = generation
	return labels
}

// PnpmLabels returns the label set for a pnpm store volume (ADR-003): the
// common cache labels plus pnpm-major=<major>.
func (c CacheVolumeIdentity) PnpmLabels(pnpmMajor string, lastUsed time.Time) map[string]string {
	labels := c.baseCacheLabels(CacheKindPnpm, lastUsed)
	labels[LabelPnpmMajor] = pnpmMajor
	return labels
}
