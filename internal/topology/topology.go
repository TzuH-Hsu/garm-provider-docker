// Package topology owns this provider's per-allocation resource orchestration
// (ADR-001, ADR-004): creating the claim-marker job network and the workspace
// volume, tearing an allocation down in the ADR-004 order, the creation-guard
// rollback, and the opportunistic orphan sweep.
//
// It talks only to a docker.Client and depends on internal/spec for the label,
// name, and predicate builders, so it stays free of package config and package
// metadata — those are the provider layer's concern. The provider fetches
// credentials and builds/starts/verifies the runner container itself, calling
// into this package for the network/volume lifecycle, teardown, and sweep.
package topology

import (
	"time"

	"github.com/docker/docker/api/types/filters"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// orphanGraceWindow is the grace period (ADR-004) before an allocation with no
// live runner container is treated as abandoned and swept. It serves both roles
// ADR-004 describes: the concurrency-safe claim-marker window (never delete a
// peer's in-flight allocation before its runner container exists) and the
// exited-container window (never race GARM's own in-flight DeleteInstance for a
// just-finished job). ADR-004 leaves open whether these should be one value or
// two independently tunable ones; WP2 tunes them to the same ~2 minutes and
// flags the split as a later decision.
const orphanGraceWindow = 2 * time.Minute

// Manager orchestrates a job's networks and volumes on one Docker host. It
// holds no cross-call state (ADR-004's one-shot subprocess model): every "what
// do I own" query is a Docker label filter answered at call time.
type Manager struct {
	cli          docker.Client
	controllerID string

	// grace and now are the orphan-sweep clock, defaulted in New and settable
	// within the package so sweep-boundary tests are deterministic.
	grace time.Duration
	now   func() time.Time
}

// New constructs a Manager for one controller.
func New(cli docker.Client, controllerID string) *Manager {
	return &Manager{
		cli:          cli,
		controllerID: controllerID,
		grace:        orphanGraceWindow,
		now:          time.Now,
	}
}

// instanceScopedFilter selects this controller's managed, job-scoped resources
// for one instance name (containers, networks, or volumes — the filter is the
// same "label" query all three list endpoints take). It is the base of the
// ADR-004 predicate scoped to a single allocation; callers re-assert
// spec.MatchesPredicate on the labels in hand for the "NOT cache=true" conjunct
// Docker's filter API cannot express.
func (m *Manager) instanceScopedFilter(instanceName string) filters.Args {
	return filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+m.controllerID),
		filters.Arg("label", spec.LabelInstanceName+"="+instanceName),
	)
}

// instanceScopedNonceFilter narrows instanceScopedFilter to a single
// CreateInstance attempt via its create-nonce (ADR-004 claim-marker nonce). The
// creation-guard rollback uses it so it removes only the resources THIS attempt
// created, never a concurrent peer's.
func (m *Manager) instanceScopedNonceFilter(instanceName, nonce string) filters.Args {
	f := m.instanceScopedFilter(instanceName)
	f.Add("label", spec.LabelCreateNonce+"="+nonce)
	return f
}
