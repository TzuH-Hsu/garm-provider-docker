// Package topology owns the Docker resources of an allocation: it creates the
// claim-marker job network and the job-scoped volumes, ensures the persistent cache
// volumes, tears allocations down, rolls back failed creates, and sweeps orphans
// (ADR-001, ADR-003, ADR-004). The provider fetches credentials and builds, starts and
// verifies the runner container itself.
package topology

import (
	"time"

	"github.com/docker/docker/api/types/filters"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// The orphan sweep's grace windows (ADR-004, Orphan sweep).
//
//   - inflightCreateGrace guards an allocation whose runner has not run: a peer's create
//     in progress or one that never completed. It must exceed every create phase, since a
//     cold image pull plus credential fetch can take many minutes. It matches GARM's
//     runner_bootstrap_timeout, past which GARM itself gives up on the create.
//   - exitedRunnerGrace is measured from the runner's State.FinishedAt. It only has to
//     avoid racing GARM's own DeleteInstance for a just-finished runner.
const (
	inflightCreateGrace = 20 * time.Minute
	exitedRunnerGrace   = 2 * time.Minute
)

// Manager orchestrates a job's networks and volumes on one Docker host. It holds no
// state between calls: every ownership question is a Docker label filter (ADR-004).
type Manager struct {
	cli          docker.Client
	controllerID string

	// The sweep's clock, settable within the package for boundary tests.
	inflightGrace time.Duration
	exitedGrace   time.Duration
	now           func() time.Time
}

// New returns a Manager for one controller.
func New(cli docker.Client, controllerID string) *Manager {
	return &Manager{
		cli:           cli,
		controllerID:  controllerID,
		inflightGrace: inflightCreateGrace,
		exitedGrace:   exitedRunnerGrace,
		now:           time.Now,
	}
}

// instanceScopedFilter selects this controller's managed resources for one instance
// name. Docker's filter cannot express "NOT cache=true", so callers re-assert
// spec.MatchesPredicate on the labels in hand.
func (m *Manager) instanceScopedFilter(instanceName string) filters.Args {
	return filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+m.controllerID),
		filters.Arg("label", spec.LabelInstanceName+"="+instanceName),
	)
}

// instanceScopedNonceFilter narrows instanceScopedFilter to one CreateInstance attempt,
// so a rollback removes only what that attempt created.
func (m *Manager) instanceScopedNonceFilter(instanceName, nonce string) filters.Args {
	f := m.instanceScopedFilter(instanceName)
	f.Add("label", spec.LabelCreateNonce+"="+nonce)
	return f
}
