package topology

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// filtersForNonce selects this controller's managed resources tagged with one
// CreateInstance attempt's create-nonce (ADR-004 claim-marker nonce), used to
// clean up a resource an ambiguous create may have leaked.
func filtersForNonce(controllerID, nonce string) filters.Args {
	return filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+controllerID),
		filters.Arg("label", spec.LabelCreateNonce+"="+nonce),
	)
}

// TeardownAllocation removes every managed, job-scoped resource for
// instanceName in the ADR-004 order — runner container, then DinD sidecar
// (WP3), then the job network (after its endpoints detach with the
// containers), then job-scoped volumes (workspace, and WP3's socket/dind-state)
// — for THIS controller. It is nonce-agnostic (DeleteInstance and the sweep
// tear down the whole allocation regardless of which attempt created it) and
// fully idempotent: a NotFound at any step is tolerated. The bool reports
// whether anything existed to remove, so DeleteInstance can return exit 30 when
// the whole instance was already gone. Per-resource errors are joined and
// returned, but the teardown continues past a failure so one stuck resource
// does not strand the rest.
//
// Cache and diagnostic volumes (ADR-003) are never touched: they carry no
// instance-name label, so the instance-scoped filter structurally excludes
// them, and spec.MatchesPredicate re-asserts the "NOT cache=true" conjunct
// defense-in-depth.
func (m *Manager) TeardownAllocation(ctx context.Context, instanceName string) (found bool, err error) {
	return m.teardown(ctx, m.instanceScopedFilter(instanceName), "")
}

// Rollback is the creation-guard teardown (ADR-004): on any error during
// CreateInstance after the claim marker exists, it removes only the resources
// THIS attempt created — those carrying this attempt's create-nonce — in the
// same container → network → volume order. Keying on the nonce means a
// concurrent peer's resources (a different nonce) are never touched: the M0
// NEW-2 property, now applied at claim-marker granularity across all resource
// kinds.
func (m *Manager) Rollback(ctx context.Context, instanceName, nonce string) error {
	_, err := m.teardown(ctx, m.instanceScopedNonceFilter(instanceName, nonce), nonce)
	return err
}

// teardown removes the containers, then networks, then volumes matching f, in
// that ADR-004 order. When requireNonce is non-empty, only resources whose
// create-nonce label equals it are removed (defense-in-depth on top of the
// Docker-side nonce filter Rollback supplies). Every removal tolerates NotFound.
func (m *Manager) teardown(ctx context.Context, f filters.Args, requireNonce string) (bool, error) {
	var (
		found bool
		errs  []error
	)

	cFound, cErr := m.removeContainers(ctx, f, requireNonce)
	found = found || cFound
	if cErr != nil {
		errs = append(errs, cErr)
	}

	nFound, nErr := m.removeNetworks(ctx, f, requireNonce)
	found = found || nFound
	if nErr != nil {
		errs = append(errs, nErr)
	}

	vFound, vErr := m.removeVolumes(ctx, f, requireNonce)
	found = found || vFound
	if vErr != nil {
		errs = append(errs, vErr)
	}

	return found, errors.Join(errs...)
}

// removeContainers stops then force-removes every managed container matching f,
// runner before DinD sidecar (ADR-004 ordering). Force-remove reaps a
// container's ANONYMOUS volumes; the named workspace/socket/dind-state volumes
// survive and are removed by removeVolumes. NotFound is tolerated.
func (m *Manager) removeContainers(ctx context.Context, f filters.Args, requireNonce string) (bool, error) {
	list, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return false, fmt.Errorf("failed to list containers for teardown: %w", err)
	}

	// Runner before DinD (ADR-004): stop/remove order is runner, then sidecar.
	sort.SliceStable(list, func(i, j int) bool {
		return roleRank(list[i].Labels) < roleRank(list[j].Labels)
	})

	var (
		found bool
		errs  []error
	)
	for _, c := range list {
		if !m.ownedForTeardown(c.Labels, requireNonce) {
			continue
		}
		found = true
		// Best-effort graceful stop first (ADR-004 ordering); the forced
		// remove below reaps a still-running container regardless.
		_ = m.cli.ContainerStop(ctx, c.ID, container.StopOptions{})
		if err := m.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove container %s: %w", c.ID, err))
		}
	}
	return found, errors.Join(errs...)
}

// removeNetworks removes every managed job network matching f. It runs after
// removeContainers so the network's endpoints are already detached (the real
// daemon — and this repo's fake — reject removing a network with active
// endpoints). NotFound is tolerated.
func (m *Manager) removeNetworks(ctx context.Context, f filters.Args, requireNonce string) (bool, error) {
	list, err := m.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return false, fmt.Errorf("failed to list networks for teardown: %w", err)
	}
	var (
		found bool
		errs  []error
	)
	for _, n := range list {
		if !m.ownedForTeardown(n.Labels, requireNonce) {
			continue
		}
		found = true
		if err := m.cli.NetworkRemove(ctx, n.ID); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove network %s: %w", n.ID, err))
		}
	}
	return found, errors.Join(errs...)
}

// removeVolumes removes every managed job-scoped volume matching f (workspace,
// and WP3's socket/dind-state). force=true also removes a volume still
// referenced by a stopped container — teardown removes containers first, so
// this is defense-in-depth. NotFound is tolerated.
func (m *Manager) removeVolumes(ctx context.Context, f filters.Args, requireNonce string) (bool, error) {
	list, err := m.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return false, fmt.Errorf("failed to list volumes for teardown: %w", err)
	}
	var (
		found bool
		errs  []error
	)
	for _, v := range list.Volumes {
		if v == nil || !m.ownedForTeardown(v.Labels, requireNonce) {
			continue
		}
		found = true
		if err := m.cli.VolumeRemove(ctx, v.Name, true); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove volume %s: %w", v.Name, err))
		}
	}
	return found, errors.Join(errs...)
}

// ownedForTeardown re-asserts, defense-in-depth against the labels in hand, the
// full ADR-004 predicate (managed + controller + has instance-name + NOT
// cache=true) and, when requireNonce is set, that the resource carries exactly
// that create-nonce. It is the single gate every teardown removal passes
// through, so a resource that slipped through a coarse Docker-side filter is
// still never removed unless it genuinely belongs to this controller (and, for
// a nonce-scoped rollback, this attempt).
func (m *Manager) ownedForTeardown(labels map[string]string, requireNonce string) bool {
	if !spec.MatchesPredicate(labels, m.controllerID) {
		return false
	}
	if requireNonce != "" && labels[spec.LabelCreateNonce] != requireNonce {
		return false
	}
	return true
}

// roleRank orders containers for teardown: runner (0) before DinD sidecar (1)
// before anything else (2), per ADR-004's "(1) runner, (2) DinD sidecar".
func roleRank(labels map[string]string) int {
	switch labels[spec.LabelRole] {
	case spec.RoleRunner:
		return 0
	case spec.RoleDind:
		return 1
	default:
		return 2
	}
}
