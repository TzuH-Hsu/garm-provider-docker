// Package spec holds the provider's pure builders for labels, environment, names,
// mounts and container configs. Nothing here calls Docker, does I/O or reads the
// clock, so it is cheap to table-test.
package spec

import (
	"time"

	"github.com/docker/docker/api/types/filters"
)

// Label keys (ADR-004).
const (
	LabelManaged      = "garm.docker/managed"
	LabelControllerID = "garm.docker/controller-id"
	LabelPoolID       = "garm.docker/pool-id"
	LabelInstanceName = "garm.docker/instance-name"
	LabelRole         = "garm.docker/role"
	LabelResource     = "garm.docker/resource"
	LabelCreatedAt    = "garm.docker/created-at"

	// LabelCache marks a cache volume. Cache volumes also never carry LabelInstanceName,
	// which structurally excludes them from every teardown path (MatchPredicateFilters).
	LabelCache = "garm.docker/cache"

	// Cache volume labels (ADR-003). No job-scoped resource carries them.
	//
	//   - LabelRepo is the repokey (RepoKey).
	//   - LabelRepoURLDigest is the full normalized-URL digest (RepoURLDigest).
	//   - LabelGeneration is the toolcache salt ([cache].generation).
	//   - LabelPnpmMajor is the pnpm store's major version ([cache].pnpm_major).
	//   - LabelLastUsed is the volume's creation time, RFC3339, which the GC ages it from.
	//     The daemon cannot change a local volume's labels, so reuse does not refresh it.
	//   - LabelCacheKind is the CacheKind.
	LabelRepo          = "garm.docker/repo"
	LabelRepoURLDigest = "garm.docker/repo-url-digest"
	LabelGeneration    = "garm.docker/generation"
	LabelPnpmMajor     = "garm.docker/pnpm-major"
	LabelLastUsed      = "garm.docker/last-used"
	LabelCacheKind     = "garm.docker/cache-kind"

	// LabelImageDigest keys an externals volume on the runner image it was seeded
	// from. It replaces LabelRepo there, since that volume is shared across repositories.
	LabelImageDigest = "garm.docker/image-digest"

	// LabelOSRelease names the runner image's OS release. Runner images set it at build
	// time and the provider reads it at create time (RunnerOSKey). Toolcache and pnpm
	// volumes carry the same value, so a reuse refuses a volume built for another release.
	LabelOSRelease = "garm.docker/os-release"

	// LabelOSType and LabelOSArch let GetInstance and ListInstances rebuild a
	// ProviderInstance's OS fields. They are not part of the ownership predicate.
	LabelOSType = "garm.docker/os-type"
	LabelOSArch = "garm.docker/os-arch"

	// LabelCreateNonce is a random value per CreateInstance attempt, on every job-scoped
	// resource the attempt creates (ADR-004). The creation guard removes only resources with its
	// own nonce, job volume names embed it so a stale teardown cannot name a newer
	// generation's volume, and teardown checks it before removing the stable-named
	// network. It is not part of the ownership predicate.
	LabelCreateNonce = "garm.docker/create-nonce"
)

// Role values for LabelRole. Containers only.
const (
	RoleRunner = "runner"
	RoleDind   = "dind"

	// RoleCacheHelper marks the short-lived externals seeder and diag pruner. They
	// carry managed and controller-id but no instance-name, so the ADR-004 predicate
	// never matches them; the GC reaps one leaked by a provider crash.
	RoleCacheHelper = "cache-helper"

	// RoleDiagPruneStamp marks the never-started per-controller container that records
	// the last diag prune (ADR-003, "Garbage collection"). It is labelled like a cache
	// helper, with its own role so the helper reaper leaves it alone.
	RoleDiagPruneStamp = "diag-prune-stamp"

	// RoleThrottleStamp marks the never-started per-controller containers that throttle
	// the orphan sweep and the cache GC (ThrottleStampName), labelled like RoleDiagPruneStamp.
	RoleThrottleStamp = "throttle-stamp"
)

// Resource kind values for LabelResource. Networks and volumes only.
const (
	ResourceJobNetwork = "job-network"
	ResourceWorkspace  = "workspace"
	ResourceSocket     = "socket"
	ResourceDindState  = "dind-state"
)

// AllocationIdentity is what a job-scoped resource's labels derive from (ADR-004).
type AllocationIdentity struct {
	ControllerID string
	PoolID       string
	InstanceName string
}

// baseLabels returns the labels every job-scoped resource carries.
func (id AllocationIdentity) baseLabels(createdAt time.Time) map[string]string {
	return map[string]string{
		LabelManaged:      "true",
		LabelControllerID: id.ControllerID,
		LabelPoolID:       id.PoolID,
		LabelInstanceName: id.InstanceName,
		LabelCreatedAt:    createdAt.UTC().Format(time.RFC3339),
	}
}

// ContainerLabels returns the labels of a container with the given Role* value.
func (id AllocationIdentity) ContainerLabels(role string, createdAt time.Time) map[string]string {
	labels := id.baseLabels(createdAt)
	labels[LabelRole] = role
	return labels
}

// ResourceLabels returns the labels of a network or volume with a Resource* value.
func (id AllocationIdentity) ResourceLabels(resource string, createdAt time.Time) map[string]string {
	labels := id.baseLabels(createdAt)
	labels[LabelResource] = resource
	return labels
}

// MatchPredicateFilters returns the Docker filter for the one teardown and orphan-sweep
// predicate (ADR-004), scoped to a controller:
//
//	garm.docker/managed=true
//	  AND garm.docker/controller-id=<controllerID>
//	  AND has garm.docker/instance-name
//	  AND NOT garm.docker/cache=true
//
// TeardownAll and the orphan sweep list with this filter; per-instance teardown narrows
// it by name. Every removal re-checks MatchesPredicate on the labels in hand. The list
// endpoints have no negative label filter and need none: cache volumes never carry
// instance-name, so the third condition already excludes them.
func MatchPredicateFilters(controllerID string) filters.Args {
	return filters.NewArgs(
		filters.Arg("label", LabelManaged+"=true"),
		filters.Arg("label", LabelControllerID+"="+controllerID),
		filters.Arg("label", LabelInstanceName),
	)
}

// MatchesPredicate reports whether labels in hand satisfy the full ADR-004 predicate,
// including the "NOT cache=true" condition the Docker filter cannot express.
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

// IsManagedRunner reports whether labels identify a runner container this controller
// owns: MatchesPredicate and role=runner. The resolver checks it on every inspected
// container, so a foreign or wrong-role one with a colliding name is never touched.
func IsManagedRunner(labels map[string]string, controllerID string) bool {
	return MatchesPredicate(labels, controllerID) && labels[LabelRole] == RoleRunner
}

// NetworkLabels returns the labels of the per-job network, ADR-004's claim marker.
// createdAt must be the instant the network is created: the orphan sweep ages an
// allocation from its youngest created-at, and at first the network's is the only one.
func (id AllocationIdentity) NetworkLabels(createdAt time.Time) map[string]string {
	return id.ResourceLabels(ResourceJobNetwork, createdAt)
}

// WorkspaceVolumeLabels returns the labels of the per-job workspace volume.
func (id AllocationIdentity) WorkspaceVolumeLabels(createdAt time.Time) map[string]string {
	return id.ResourceLabels(ResourceWorkspace, createdAt)
}

// SocketVolumeLabels returns the labels of the per-job DinD socket volume.
func (id AllocationIdentity) SocketVolumeLabels(createdAt time.Time) map[string]string {
	return id.ResourceLabels(ResourceSocket, createdAt)
}

// DindStateVolumeLabels returns the labels of the per-job dind-state volume.
func (id AllocationIdentity) DindStateVolumeLabels(createdAt time.Time) map[string]string {
	return id.ResourceLabels(ResourceDindState, createdAt)
}

// DindContainerLabels returns the labels of the DinD sidecar container.
func (id AllocationIdentity) DindContainerLabels(createdAt time.Time) map[string]string {
	return id.ContainerLabels(RoleDind, createdAt)
}
