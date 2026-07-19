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

	// LabelCache is ADR-003's cache-volume marker. It is defined here
	// (rather than left undocumented) only because ADR-004's match
	// predicate is defined in terms of its absence — see
	// MatchPredicateFilters and MatchesPredicate below. Cache-volume
	// label builders themselves belong to the ADR-003 (M2) work package,
	// not this one.
	LabelCache = "garm.docker/cache"

	// LabelOSType and LabelOSArch are informational labels carrying the
	// bootstrap OS type/arch. They are NOT part of the ADR-004 teardown
	// predicate; they exist so GetInstance/ListInstances can reconstruct a
	// ProviderInstance's os fields directly from labels, mirroring the
	// reference k8s and werdnum providers (research.md §2.A, §2.B).
	LabelOSType = "garm.docker/os-type"
	LabelOSArch = "garm.docker/os-arch"

	// LabelCreateNonce is a per-CreateInstance-attempt random tag written on
	// the container that attempt creates. It exists so the ambiguous-create
	// cleanup (create.go) can tell a container THIS attempt created apart from
	// one a concurrent, same-instance-name CreateInstance won the create race
	// for: cleanup removes only a container whose nonce matches this attempt's,
	// and treats a name-conflict against a DIFFERENT nonce as a genuine
	// duplicate (exit 31) rather than deleting the concurrent winner's
	// container (NEW-2).
	//
	// It is deliberately INFORMATIONAL and outside the ADR-004 teardown
	// predicate (MatchesPredicate / MatchPredicateFilters / IsManagedRunner do
	// not reference it): ownership and teardown scoping are unchanged; the
	// nonce only disambiguates the create race.
	LabelCreateNonce = "garm.docker/create-nonce"
)

// Role values for LabelRole. Containers only.
const (
	RoleRunner = "runner"
	RoleDind   = "dind"
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
