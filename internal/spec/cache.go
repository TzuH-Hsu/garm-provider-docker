package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// This file holds ADR-003's persistent-cache pure functions (M2-W1): the
// repo-URL cache key, the entity-scope classifier that decides whether a pool
// is eligible for repo-scoped caches at all, and the cache-volume name/label
// builders. Like the rest of package spec these are side-effect-free — no
// Docker calls, no time.Now(), no I/O — so they are cheap to table-test, and
// the caller (the provider create path) supplies the last-used timestamp.

// repoKeyHashLen is the number of leading hex characters of the repo-URL
// SHA-256 that form the hash suffix of a repokey. ADR-003 fixes this at 12
// ("the first 12 hex characters (minimum)"), i.e. 48 bits — collision-
// RESISTANT, not collision-PROOF, and (per ADR-003) far more than sufficient
// for the number of distinct repositories any single Docker host will realise.
const repoKeyHashLen = 12

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

// cacheVolumeNamePrefix is the shared prefix for every cache volume, so a
// label-blind operator can still spot cache volumes by name (`docker volume ls`
// | grep garm-cache-). ADR-003's authoritative selector is still the label set,
// not the name.
const cacheVolumeNamePrefix = "garm-cache-"

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
	return map[string]string{
		LabelManaged:      "true",
		LabelControllerID: c.ControllerID,
		LabelCache:        "true",
		LabelCacheKind:    string(kind),
		LabelRepo:         c.RepoKey,
		LabelLastUsed:     lastUsed.UTC().Format(time.RFC3339),
	}
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
