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

// The orphan sweep uses TWO distinct grace windows (F5/F8), which ADR-004
// always described as conceptually separate even while WP2 tuned them to one
// value:
//
//   - inflightCreateGrace guards an allocation whose runner container does not
//     (yet) exist — either a peer's still-in-progress CreateInstance or a
//     never-completed one. This MUST exceed every create phase, because a cold,
//     emulated image pull plus credential fetch can legitimately take many
//     minutes; sweeping such an allocation would destroy a valid in-flight
//     create. It is bound to a hard deadline aligned with GARM's own
//     runner_bootstrap_timeout (~20 min), the point past which GARM itself
//     gives up on a create, so anything older is genuinely abandoned. (F9's
//     host-wide sweep-on-create is only safe because of this: a peer create
//     inside this window is never swept.)
//
//   - exitedRunnerGrace guards an allocation whose runner has EXITED — a
//     just-finished (or failed) job. It is measured from the runner's
//     State.FinishedAt (F8), not from allocation creation, and is a SEPARATE,
//     much shorter constant: its only job is to avoid racing GARM's own
//     in-flight DeleteInstance for the same just-finished runner.
const (
	inflightCreateGrace = 20 * time.Minute
	exitedRunnerGrace   = 2 * time.Minute
)

// Manager orchestrates a job's networks and volumes on one Docker host. It
// holds no cross-call state (ADR-004's one-shot subprocess model): every "what
// do I own" query is a Docker label filter answered at call time.
type Manager struct {
	cli          docker.Client
	controllerID string

	// inflightGrace, exitedGrace, and now are the orphan-sweep clock, defaulted
	// in New and settable within the package so sweep-boundary tests are
	// deterministic.
	inflightGrace time.Duration
	exitedGrace   time.Duration
	now           func() time.Time
}

// New constructs a Manager for one controller.
func New(cli docker.Client, controllerID string) *Manager {
	return &Manager{
		cli:           cli,
		controllerID:  controllerID,
		inflightGrace: inflightCreateGrace,
		exitedGrace:   exitedRunnerGrace,
		now:           time.Now,
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
