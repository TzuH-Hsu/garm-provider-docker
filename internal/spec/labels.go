// Package spec holds this provider's pure functions: label, environment,
// and name builders. Everything here is side-effect-free by design (no
// Docker calls, no I/O) specifically so it is cheap to table-test
// (docs/plan.md §2).
package spec

import (
	"time"

	"github.com/docker/docker/api/types/filters"
)

// Label keys, exactly per ADR-004's label schema.
const (
	LabelManaged      = "garm.docker/managed"
	LabelControllerID = "garm.docker/controller-id"
	LabelPoolID       = "garm.docker/pool-id"
	LabelInstanceName = "garm.docker/instance-name"
	LabelRole         = "garm.docker/role"
	LabelResource     = "garm.docker/resource"
	LabelCreatedAt    = "garm.docker/created-at"

	// LabelCache is ADR-003's cache-volume marker. ADR-004's teardown/
	// orphan-sweep match predicate is defined in terms of its absence — see
	// MatchPredicateFilters and MatchesPredicate below — so a cache volume,
	// which carries cache=true and NO instance-name, is structurally excluded
	// from every teardown path. The cache-volume label builders that stamp it
	// live in cache.go (ADR-003, M2-W1).
	LabelCache = "garm.docker/cache"

	// LabelRepo, LabelGeneration, LabelPnpmMajor, LabelLastUsed, and
	// LabelCacheKind are ADR-003's cache-volume label set (M2-W1). They are
	// carried ONLY by cache volumes, never by a job-scoped resource, and a
	// cache volume never carries LabelInstanceName — that absence is what keeps
	// it out of ADR-004's teardown/sweep predicate (MatchesPredicate). The
	// builders that stamp them are CacheVolumeIdentity's methods in cache.go.
	//
	//   - LabelRepo          = the <repokey> the cache is keyed on (RepoKey).
	//   - LabelRepoURLDigest = the FULL (64-hex) normalized-URL SHA-256, carried
	//     alongside the truncated repokey so a cache reuse can re-verify the
	//     repository identity and refuse to adopt a different repo's volume that
	//     collided on the repokey (L7; validated in topology.EnsureCacheVolume).
	//   - LabelGeneration = the toolcache generation salt (config [cache].generation).
	//   - LabelPnpmMajor  = the pnpm store's pnpm major version (config [cache].pnpm_major).
	//   - LabelLastUsed   = an RFC3339 timestamp, set at CREATION. The real
	//     daemon cannot mutate a local volume's labels after creation (verified
	//     against Docker Engine 29.6.1: re-VolumeCreate keeps the ORIGINAL
	//     labels, and `docker volume update` is cluster-volumes-only), so this
	//     records the volume's creation instant. W2's opportunistic GC IDENTIFIES
	//     (never auto-deletes — ADR-003's cache-GC-safety amendment) stale caches
	//     by AGE-since-this-creation-timestamp plus SALT SUPERSESSION (a toolcache
	//     generation / pnpm-major / externals image-digest that no longer matches
	//     the current config, past a grace) — NOT by filesystem mtime, which the
	//     provider cannot portably stat from outside the Docker Desktop VM — and
	//     LOGS them for the operator's own manual `docker volume prune`. See
	//     spec/gc.go, cache.go, and ADR-003's W2 amendment (point 3).
	//   - LabelCacheKind  = "toolcache" | "pnpm" | "externals" | "diag-logs", so a
	//     GC/purge pass (W2) can tell the cache kinds apart without parsing the name.
	LabelRepo          = "garm.docker/repo"
	LabelRepoURLDigest = "garm.docker/repo-url-digest"
	LabelGeneration    = "garm.docker/generation"
	LabelPnpmMajor     = "garm.docker/pnpm-major"
	LabelLastUsed      = "garm.docker/last-used"
	LabelCacheKind     = "garm.docker/cache-kind"

	// LabelImageDigest is the W2 externals-cache key (ADR-003 amendment
	// 2026-07-22): the runner IMAGE's content digest an externals volume was
	// seeded from. The externals volume carries it INSTEAD of LabelRepo (it is
	// shared across every repository — its contents are Node runtimes tied to
	// the runner image's version, not repository data), and the opportunistic
	// cache GC keys externals supersession on it (an externals volume whose
	// image-digest is not the currently-configured runner image's, past a grace,
	// is FLAGGED as stale and logged — never auto-deleted). Like every cache
	// label it is carried ONLY by cache volumes
	// and never alongside LabelInstanceName, so it is structurally excluded from
	// ADR-004's teardown/sweep predicate. The builder that stamps it is
	// ExternalsVolumeIdentity.ExternalsLabels in externals.go.
	LabelImageDigest = "garm.docker/image-digest"

	// LabelOSType and LabelOSArch are informational labels carrying the
	// bootstrap OS type/arch. They are NOT part of the ADR-004 teardown
	// predicate; they exist so GetInstance/ListInstances can reconstruct a
	// ProviderInstance's os fields directly from labels, mirroring the
	// reference k8s and werdnum providers (research.md §2.A, §2.B).
	LabelOSType = "garm.docker/os-type"
	LabelOSArch = "garm.docker/os-arch"

	// LabelCreateNonce is a per-CreateInstance-attempt random tag (crypto/rand
	// hex, see provider.newCreateNonce) written on every resource that attempt
	// creates. It plays THREE load-bearing roles:
	//
	//  1. Ambiguous-create disambiguation (NEW-2): the create-guard cleanup
	//     removes only a container/network whose nonce matches THIS attempt's,
	//     and treats a name-conflict against a DIFFERENT nonce as a genuine
	//     duplicate (exit 31) rather than deleting the concurrent winner's
	//     resource.
	//  2. Generation-unique VOLUME NAMES (F4, ADR-004 amendment 2026-07-21): the
	//     workspace/socket/dind-state volume names now EMBED this nonce
	//     (spec.WorkspaceVolumeName/SocketVolumeName/DindStateVolumeName), so a
	//     stale teardown that removes a volume by the name it read from its own
	//     label-scoped list can never name-collide with a different generation's
	//     freshly-created volume. This is what makes name-based teardown
	//     generation-safe by construction — the re-list alone could not.
	//  3. Generation-nonce teardown gating (defense-in-depth): the network keeps
	//     a STABLE per-instance name (it is the claim marker), so its removal is
	//     still gated by comparing this nonce against the generation the teardown
	//     captured at start (teardownScope), protecting a stable-named claim
	//     network a concurrent create may have re-claimed mid-teardown.
	//
	// It remains outside the ADR-004 ownership predicate
	// (MatchesPredicate / MatchPredicateFilters / IsManagedRunner do not
	// reference it): ownership scoping is unchanged; the nonce disambiguates
	// generations, it does not decide ownership.
	LabelCreateNonce = "garm.docker/create-nonce"
)

// Role values for LabelRole. Containers only.
const (
	RoleRunner = "runner"
	RoleDind   = "dind"

	// RoleCacheHelper marks the short-lived, provider-run helper containers of
	// M2-W2 (ADR-003 amendment 2026-07-22): the externals seeder and the
	// diagnostic-log pruner. They carry managed=true + controller-id + this role
	// (but NO instance-name, so the ADR-004 teardown/sweep predicate never
	// matches them) so a helper leaked by a hard provider crash can still be
	// found and reaped — by the opportunistic GC's own helper reap and by a
	// controller-scoped rescue. Every normal path force-removes its helper in a
	// defer, so a leak requires the process to die mid-run.
	RoleCacheHelper = "cache-helper"
)

// Resource kind values for LabelResource. Networks and volumes only.
const (
	ResourceJobNetwork = "job-network"
	ResourceWorkspace  = "workspace"
	ResourceSocket     = "socket"
	ResourceDindState  = "dind-state"
)

// AllocationIdentity is the job-scoped identity every per-allocation
// resource's labels (ADR-004) are derived from.
type AllocationIdentity struct {
	ControllerID string
	PoolID       string
	InstanceName string
}

// baseLabels returns the labels every job-scoped resource carries
// regardless of kind: managed=true, controller-id, pool-id,
// instance-name, and a created-at timestamp.
func (id AllocationIdentity) baseLabels(createdAt time.Time) map[string]string {
	return map[string]string{
		LabelManaged:      "true",
		LabelControllerID: id.ControllerID,
		LabelPoolID:       id.PoolID,
		LabelInstanceName: id.InstanceName,
		LabelCreatedAt:    createdAt.UTC().Format(time.RFC3339),
	}
}

// ContainerLabels returns the labels for a runner or DinD sidecar
// container (role = RoleRunner or RoleDind).
func (id AllocationIdentity) ContainerLabels(role string, createdAt time.Time) map[string]string {
	labels := id.baseLabels(createdAt)
	labels[LabelRole] = role
	return labels
}

// ResourceLabels returns the labels for a network or volume (resource =
// ResourceJobNetwork, ResourceWorkspace, ResourceSocket, or
// ResourceDindState).
func (id AllocationIdentity) ResourceLabels(resource string, createdAt time.Time) map[string]string {
	labels := id.baseLabels(createdAt)
	labels[LabelResource] = resource
	return labels
}

// MatchPredicateFilters returns the Docker filters.Args this provider's
// single authoritative teardown/orphan-sweep match predicate (ADR-004)
// compiles down to, scoped to one controller:
//
//	garm.docker/managed=true
//	  AND garm.docker/controller-id=<controllerID>
//	  AND has garm.docker/instance-name
//	  AND NOT garm.docker/cache=true
//
// DeleteInstance, the orphan sweep, and RemoveAllInstances (ADR-004,
// implemented in a later work package) must all call this same function
// rather than construct their own label filters, so the predicate is
// defined in exactly one place in the codebase.
//
// Docker ANDs multiple values under the same "label" filter key together
// (see filters.Args.MatchKVList — both the real daemon and this repo's
// FakeClient implement this identically, internal/docker/fake.go), so the
// first three conjuncts above combine correctly in a single Docker-side
// query. The fourth, "AND NOT garm.docker/cache=true", has no Docker-side
// equivalent at all — the Docker API has no negative/"not equal" label
// filter — and needs none: cache volumes (ADR-003) never carry
// garm.docker/instance-name in the first place, so "has instance-name"
// already structurally excludes every cache resource. ADR-004 calls this
// out explicitly as "a structural exclusion, not a label-value check that
// could be gotten wrong at one call site and right at another."
func MatchPredicateFilters(controllerID string) filters.Args {
	return filters.NewArgs(
		filters.Arg("label", LabelManaged+"=true"),
		filters.Arg("label", LabelControllerID+"="+controllerID),
		filters.Arg("label", LabelInstanceName),
	)
}

// MatchesPredicate reports whether labels (e.g. from a container, network,
// or volume already fetched via MatchPredicateFilters) satisfy the full
// ADR-004 predicate, including the "NOT cache=true" conjunct that Docker's
// own filter API cannot express. It exists so the same predicate can be
// re-asserted, defense-in-depth, against labels already in hand, without
// re-deriving the logic at each call site.
func MatchesPredicate(labels map[string]string, controllerID string) bool {
	if labels[LabelManaged] != "true" {
		return false
	}
	if labels[LabelControllerID] != controllerID {
		return false
	}
	if _, ok := labels[LabelInstanceName]; !ok {
		return false
	}
	if labels[LabelCache] == "true" {
		return false
	}
	return true
}

// IsManagedRunner reports whether labels identify a runner container this
// controller owns: the full ADR-004 ownership predicate (MatchesPredicate)
// AND role=runner. The resolver and creation guard use it to validate any
// inspected container before acting on it, so a foreign or wrong-role
// container that happens to collide on a Docker ID or name is never touched
// (ADR-004 F3).
func IsManagedRunner(labels map[string]string, controllerID string) bool {
	return MatchesPredicate(labels, controllerID) && labels[LabelRole] == RoleRunner
}

// NetworkLabels returns the labels for the per-job network. This resource
// is ADR-004's atomic claim marker — the FIRST resource created for any
// allocation, stamped with instance-name and created-at at the moment of
// its own creation — so createdAt here must be the exact instant WP2/WP3
// creates the network, not a later timestamp. Like every builder in this
// file, it is pure: createdAt is supplied by the caller, never read from
// time.Now() here, so the concurrency-safe claim-marker semantics ADR-004
// depends on stay under the caller's control and this function stays cheap
// to table-test.
func (id AllocationIdentity) NetworkLabels(createdAt time.Time) map[string]string {
	return id.ResourceLabels(ResourceJobNetwork, createdAt)
}

// WorkspaceVolumeLabels returns the labels for the per-job workspace
// volume (ADR-001).
func (id AllocationIdentity) WorkspaceVolumeLabels(createdAt time.Time) map[string]string {
	return id.ResourceLabels(ResourceWorkspace, createdAt)
}

// SocketVolumeLabels returns the labels for the per-job DinD socket volume
// (ADR-001). Unused when dind_mode is "none".
func (id AllocationIdentity) SocketVolumeLabels(createdAt time.Time) map[string]string {
	return id.ResourceLabels(ResourceSocket, createdAt)
}

// DindStateVolumeLabels returns the labels for the per-job dind-state
// volume (ADR-001), mounted at /var/lib/docker inside the DinD sidecar.
// Unused when dind_mode is "none".
func (id AllocationIdentity) DindStateVolumeLabels(createdAt time.Time) map[string]string {
	return id.ResourceLabels(ResourceDindState, createdAt)
}

// DindContainerLabels returns the labels for the DinD sidecar container
// (role=dind). A named wrapper around ContainerLabels(RoleDind, ...) so
// WP2/WP3 call sites read self-documented ("DindContainerLabels(t)")
// instead of needing to know which RoleXxx constant pairs with the sidecar.
// Unused when dind_mode is "none".
func (id AllocationIdentity) DindContainerLabels(createdAt time.Time) map[string]string {
	return id.ContainerLabels(RoleDind, createdAt)
}
