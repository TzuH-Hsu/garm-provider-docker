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

// repoKeyHashLen is the hex length of a repokey's hash suffix: 128 bits. The full
// digest also rides along as LabelRepoURLDigest and adoption checks it, so even a
// truncation collision cannot make one repo adopt another's cache.
const repoKeyHashLen = 32

// repoSlugMaxLen bounds a repokey's readable half; the hash provides uniqueness.
const repoSlugMaxLen = 40

// RepoKey returns the cache key for a repo_url: <slug>-<hash>, or just <hash> when no
// readable slug survives. The hash covers the normalized URL, so spellings of one
// repository that differ in case, scheme, .git or trailing slash share a cache
// (ADR-003). The result is never empty and is safe in a Docker resource name.
func RepoKey(repoURL string) string {
	norm := normalizeRepoURL(repoURL)
	sum := sha256.Sum256([]byte(norm))
	hash := hex.EncodeToString(sum[:])[:repoKeyHashLen]

	slug := repoSlug(norm)
	if slug == "" {
		return hash
	}
	return slug + "-" + hash
}

// RepoURLDigest returns the full 64-hex SHA-256 of the normalized repo URL. Cache
// volumes carry it as LabelRepoURLDigest and adoption rechecks it, so a repokey
// collision fails closed instead of sharing a cache between repositories.
func RepoURLDigest(repoURL string) string {
	sum := sha256.Sum256([]byte(normalizeRepoURL(repoURL)))
	return hex.EncodeToString(sum[:])
}

// normalizeRepoURL lowercases, trims, and strips the scheme, a trailing ".git" and
// trailing slashes. It does not parse, so an unusual repo_url normalizes without error.
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

// repoSlug renders the path of a normalized URL as a slug. The host is left out for
// readability; RepoKey's hash covers it, so two forges sharing a path stay distinct.
func repoSlug(norm string) string {
	path := norm
	if i := strings.IndexByte(norm, '/'); i >= 0 {
		path = norm[i+1:]
	}
	return slugify(path)
}

// slugify collapses runs outside [a-z0-9] into one dash, trims dashes, and caps the
// result at repoSlugMaxLen. It does not lowercase: the input is already normalized.
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

// CacheEntityScope classifies a repo_url for cache eligibility. Unlike EntityScope it
// has an unknown value, so a parse failure withholds caches instead of failing a create.
type CacheEntityScope string

const (
	CacheScopeRepo       CacheEntityScope = "repo"
	CacheScopeOrg        CacheEntityScope = "org"
	CacheScopeEnterprise CacheEntityScope = "enterprise"
	CacheScopeUnknown    CacheEntityScope = "unknown"
)

// DetectCacheEntityScope classifies repoURL with ParseEntity. Anything ParseEntity
// rejects is CacheScopeUnknown, which CacheScopeAllowed always denies.
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

// CacheScopeAllowed reports whether an allocation of this scope may use persistent
// repo-keyed caches. Repo scope always may. Org and enterprise scope may only with
// [cache].allow_org_shared, which treats the whole entity as one trust domain. Unknown
// scope never may: it has no confident identity to key a cache on (ADR-003).
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

// cacheIdentityLabelKeys are the labels that tell one cache volume from another.
// Controller-id and last-used are excluded: a peer controller's legitimately shared
// volume differs on both (ADR-003).
var cacheIdentityLabelKeys = []string{
	LabelCacheKind,
	LabelRepo,
	LabelRepoURLDigest,
	LabelImageDigest,
	LabelGeneration,
	LabelPnpmMajor,
	LabelOSRelease,
}

// ValidateAdoptedCacheVolume returns an error unless an existing volume (labels got)
// is ours to reuse for the labels want: it must be a managed cache and agree on every
// identity key want sets. Otherwise a foreign volume squatting the name, or a different
// cache colliding on it, would be mounted as this repo's writable cache or, for
// externals, run as executable runtime.
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

// requiredCacheIdentityKeys returns the labels a cache volume of this kind must carry
// to be provably ours, or nil for an unknown kind. os-release is not required: a
// toolcache or pnpm volume without it must stay provably ours so the GC reports it as
// superseded rather than foreign. Adoption still checks it.
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

// ValidateCacheVolumeKind returns an error unless labels are a managed cache volume's
// and carry every identity key its kind requires. It is for callers with no expected
// label set to compare with: the GC's listing and the diag prune. Unlike
// ValidateAdoptedCacheVolume, it treats a missing key as a failure.
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

// ValidateDiagPruneTarget returns an error unless the volume the prune helper pinned
// (labels got) passes ValidateCacheVolumeKind, is a diag-logs volume of this controller,
// and matches the repo and repo-url-digest in snapshot, the labels seen at enumeration.
// A same-name replacement or a daemon-created unlabelled volume therefore aborts the
// prune before it deletes anything (ADR-003).
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

// CacheIdentityFilter returns the label filter that finds a cache by identity, under
// whatever name it lives at: managed, cache, and each identity key want sets. Docker
// ANDs same-key label filters, so only a volume with the full identity matches. Callers
// still validate each result with ValidateAdoptedCacheVolume. See ADR-003, "A cache's
// identity is its label set, not its name".
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

// cacheReconcileSuffixLen is 32 bits of hex, ample for one host's few squatters.
const cacheReconcileSuffixLen = 8

// AlternateCacheVolumeName returns the name to use when preferred is occupied by a
// volume that is not our cache. The provider routes around such a volume, because
// deleting it could remove foreign data (ADR-003). attempt is 1-based. The suffix is
// deterministic, so processes racing around one squatter converge on one slot, and a
// later attempt gets another slot. Discovery is by label, so later allocations find it.
func AlternateCacheVolumeName(preferred string, attempt int) string {
	sum := sha256.Sum256([]byte(preferred + ":" + strconv.Itoa(attempt)))
	return preferred + "-x" + hex.EncodeToString(sum[:])[:cacheReconcileSuffixLen]
}

// cacheVolumeNamePrefix lets an operator spot cache volumes; labels decide ownership.
const cacheVolumeNamePrefix = "garm-cache-"

// HasCacheVolumeNamePrefix reports whether name has the cache volume prefix. It is a
// name heuristic for the GC's log of volumes it will not delete; labels decide ownership.
func HasCacheVolumeNamePrefix(name string) bool {
	return strings.HasPrefix(name, cacheVolumeNamePrefix)
}

// CacheKind names a persistent cache: per repository, except externals (per image).
type CacheKind string

const (
	CacheKindToolcache CacheKind = "toolcache"
	CacheKindPnpm      CacheKind = "pnpm"

	// CacheKindExternals holds the runner's Node runtimes, shared read-only (externals.go).
	CacheKindExternals CacheKind = "externals"

	// CacheKindDiagLogs persists the runner's diagnostic logs per repository (diag.go).
	CacheKindDiagLogs CacheKind = "diag-logs"
)

// ToolcacheVolumeName returns garm-cache-toolcache-<repokey>-<oskey>-<generation>.
// setup-python and setup-node download builds made for one Ubuntu release, so releases
// never share a toolcache. Changing the generation points new jobs at an empty volume.
func ToolcacheVolumeName(repoKey, osKey, generation string) string {
	return cacheVolumeNamePrefix + string(CacheKindToolcache) + "-" + repoKey + "-" + osKey + "-" + generation
}

// PnpmVolumeName returns garm-cache-pnpm-<repokey>-<oskey>-<pnpmMajor>. pnpm's
// side-effects cache keeps native addons linked against one release's glibc, and the
// store's layout is tied to the pnpm major.
func PnpmVolumeName(repoKey, osKey, pnpmMajor string) string {
	return cacheVolumeNamePrefix + string(CacheKindPnpm) + "-" + repoKey + "-" + osKey + "-" + pnpmMajor
}

// CacheVolumeIdentity is what a repo-keyed cache volume's labels derive from. It has no
// pool ID or instance name: a cache outlives every allocation, and lacking instance-name
// is what keeps it out of ADR-004's teardown predicate.
type CacheVolumeIdentity struct {
	ControllerID string
	RepoKey      string

	// RepoURLDigest is the value of LabelRepoURLDigest, omitted when empty.
	RepoURLDigest string

	// OSKey is the value of LabelOSRelease on toolcache and pnpm volumes, omitted when
	// empty. Diag logs are plain text and stay shared across OS releases, so they skip it.
	OSKey string
}

// baseCacheLabels returns the labels the repo-keyed cache kinds share; externals volumes
// build theirs in ExternalsLabels. It sets no job-scoped label
// (instance-name, pool-id, resource, create-nonce), so no teardown, sweep or rollback
// predicate can match a cache volume. controller-id records the volume's creator and is
// not part of its identity. lastUsed records creation: a local volume's labels cannot
// change, so it is never re-stamped on a hit.
func (c CacheVolumeIdentity) baseCacheLabels(kind CacheKind, lastUsed time.Time) map[string]string {
	labels := map[string]string{
		LabelManaged:      "true",
		LabelControllerID: c.ControllerID,
		LabelCache:        "true",
		LabelCacheKind:    string(kind),
		LabelRepo:         c.RepoKey,
		LabelLastUsed:     lastUsed.UTC().Format(time.RFC3339),
	}
	if c.RepoURLDigest != "" {
		labels[LabelRepoURLDigest] = c.RepoURLDigest
	}
	return labels
}

// ToolcacheLabels returns a toolcache volume's labels.
func (c CacheVolumeIdentity) ToolcacheLabels(generation string, lastUsed time.Time) map[string]string {
	labels := c.baseCacheLabels(CacheKindToolcache, lastUsed)
	labels[LabelGeneration] = generation
	c.addOSKey(labels)
	return labels
}

// PnpmLabels returns a pnpm store volume's labels.
func (c CacheVolumeIdentity) PnpmLabels(pnpmMajor string, lastUsed time.Time) map[string]string {
	labels := c.baseCacheLabels(CacheKindPnpm, lastUsed)
	labels[LabelPnpmMajor] = pnpmMajor
	c.addOSKey(labels)
	return labels
}

// addOSKey is not in baseCacheLabels because diag-logs volumes must not carry the key.
func (c CacheVolumeIdentity) addOSKey(labels map[string]string) {
	if c.OSKey != "" {
		labels[LabelOSRelease] = c.OSKey
	}
}
