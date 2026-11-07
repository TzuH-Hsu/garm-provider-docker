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

// filtersForNonce selects this controller's managed resources carrying one
// create-nonce.
func filtersForNonce(controllerID, nonce string) filters.Args {
	return filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+controllerID),
		filters.Arg("label", spec.LabelCreateNonce+"="+nonce),
	)
}

// TeardownAllocation removes instanceName's current generation for this controller:
// runner, DinD sidecar, job-scoped volumes, then the job network last (ADR-004,
// Teardown order). If a claim network exists, resources carrying another create-nonce
// are left alone, so a same-name create that claims a new generation mid-teardown
// survives. If none exists, everything under the name is a leftover to reap, except what
// carries the nonce of a claim made since (ADR-004, Teardown scope). NotFound is tolerated at every step, and a failed removal does not
// stop the rest: errors are joined. found reports whether anything existed to remove,
// so DeleteInstance can return exit 30 when the instance is already gone. Cache and
// diagnostic volumes are never matched.
func (m *Manager) TeardownAllocation(ctx context.Context, instanceName string) (found bool, err error) {
	return m.teardownGeneration(ctx, m.instanceScopedFilter(instanceName), false)
}

// TeardownAllocationReportingIO is TeardownAllocation that first logs, before any
// container stops, the exit details of a runner that is not running
// (reportStoppedRunners) and each DinD sidecar's block-device writes and images
// (ADR-006, phase 0). The reports are best-effort and time-bounded: they never change
// what is removed, and their failures are logged, not returned. The orphan sweep does
// not use it, since it runs inside other instances' creates and must not slow them, and
// it logs its own line about an exited runner.
func (m *Manager) TeardownAllocationReportingIO(ctx context.Context, instanceName string) (found bool, err error) {
	return m.teardownGeneration(ctx, m.instanceScopedFilter(instanceName), true)
}

// TeardownAll removes every job-scoped resource this controller owns, in the same
// order, for the RemoveAllInstances rescue. Unlike TeardownAllocation it is not scoped
// to a generation, so it removes live allocations too: a scoped pass skips an older
// generation's residue while a newer generation holds the claim network (ADR-004,
// Teardown scope). Cache and diagnostic volumes
// are never matched. Per-resource errors are joined and the teardown continues.
func (m *Manager) TeardownAll(ctx context.Context) error {
	f := spec.MatchPredicateFilters(m.controllerID)
	scope := &teardownScope{m: m, mode: teardownModeAll}
	_, err := m.runTeardown(ctx, f, scope)
	return err
}

// Rollback is CreateInstance's creation guard (ADR-004): it removes only the resources
// carrying this attempt's nonce, in the same order, so a concurrent peer's are never
// touched.
func (m *Manager) Rollback(ctx context.Context, instanceName, nonce string) error {
	scope := &teardownScope{m: m, mode: teardownModeExact, exact: nonce}
	_, err := m.runTeardown(ctx, m.instanceScopedNonceFilter(instanceName, nonce), scope)
	return err
}

// teardownGeneration tears down what f matches, scoped to the generation each
// instance's claim network holds now. That read happens once, before anything is
// removed.
func (m *Manager) teardownGeneration(ctx context.Context, f filters.Args, reportOnDelete bool) (bool, error) {
	captured, err := m.claimNoncesByInstance(ctx, f)
	if err != nil {
		return false, err
	}
	scope := &teardownScope{m: m, mode: teardownModeGeneration, captured: captured, reportOnDelete: reportOnDelete}
	return m.runTeardown(ctx, f, scope)
}

// runTeardown removes what f matches and scope allows, in the order ADR-004 requires
// (Teardown order). scope re-checks the labels in hand before each removal.
func (m *Manager) runTeardown(ctx context.Context, f filters.Args, scope *teardownScope) (bool, error) {
	var (
		found bool
		errs  []error
	)

	// Containers first: the daemon rejects removing a network with active endpoints or
	// a volume a container references.
	cFound, cErr := m.removeContainers(ctx, f, scope)
	found = found || cFound
	if cErr != nil {
		errs = append(errs, cErr)
	}

	vFound, vErr := m.removeVolumes(ctx, f, scope)
	found = found || vFound
	if vErr != nil {
		errs = append(errs, vErr)
	}

	// Network last: it is the claim marker, and while it exists a same-name create
	// cannot claim a new generation and create volumes this teardown would delete.
	nFound, nErr := m.removeNetworks(ctx, f, scope)
	found = found || nFound
	if nErr != nil {
		errs = append(errs, nErr)
	}

	return found, errors.Join(errs...)
}

// removeContainers stops then force-removes the containers f matches. Removal reaps a
// container's anonymous volumes; named ones are left for removeVolumes.
func (m *Manager) removeContainers(ctx context.Context, f filters.Args, scope *teardownScope) (bool, error) {
	list, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return false, fmt.Errorf("failed to list containers for teardown: %w", err)
	}

	// Runner before sidecar (ADR-004, Teardown order).
	sort.SliceStable(list, func(i, j int) bool {
		return roleRank(list[i].Labels) < roleRank(list[j].Labels)
	})

	var (
		found bool
		errs  []error
	)
	// These read exit state, logs and cgroup counters that stopping or removing takes
	// away, so they run before any container stops.
	m.reportStoppedRunners(ctx, list, scope)
	m.newDindIOReporter(ctx, list, scope).reportAll(ctx, list, scope)
	for _, c := range list {
		ok, err := scope.mayRemove(ctx, c.Labels)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
			continue
		}
		found = true
		// Best-effort graceful stop; the forced remove reaps a still-running container
		// regardless.
		_ = m.cli.ContainerStop(ctx, c.ID, container.StopOptions{})
		if err := m.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove container %s: %w", c.ID, err))
		}
	}
	return found, errors.Join(errs...)
}

func (m *Manager) removeNetworks(ctx context.Context, f filters.Args, scope *teardownScope) (bool, error) {
	list, err := m.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return false, fmt.Errorf("failed to list networks for teardown: %w", err)
	}
	var (
		found bool
		errs  []error
	)
	for _, n := range list {
		ok, err := scope.mayRemove(ctx, n.Labels)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
			continue
		}
		found = true
		if err := m.cli.NetworkRemove(ctx, n.ID); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove network %s: %w", n.ID, err))
		}
	}
	return found, errors.Join(errs...)
}

func (m *Manager) removeVolumes(ctx context.Context, f filters.Args, scope *teardownScope) (bool, error) {
	list, err := m.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return false, fmt.Errorf("failed to list volumes for teardown: %w", err)
	}
	var (
		found bool
		errs  []error
	)
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		ok, err := scope.mayRemove(ctx, v.Labels)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
			continue
		}
		found = true
		if err := m.cli.VolumeRemove(ctx, v.Name, true); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove volume %s: %w", v.Name, err))
		}
	}
	return found, errors.Join(errs...)
}

// teardownMode selects a teardownScope's nonce gating.
type teardownMode int

const (
	// teardownModeExact removes only resources carrying one create-nonce (Rollback).
	teardownModeExact teardownMode = iota
	// teardownModeGeneration removes each instance's generation as of teardown start or,
	// if it had no claim network then, its leftovers (TeardownAllocation).
	teardownModeGeneration
	// teardownModeAll ignores the create-nonce (TeardownAll).
	teardownModeAll
)

// teardownScope is the gate every teardown removal passes through. Checking the labels
// in hand means a resource that slipped a coarse Docker filter, or whose generation
// changed since the list, is not removed.
type teardownScope struct {
	m    *Manager
	mode teardownMode

	// exact is the required create-nonce in teardownModeExact.
	exact string

	// captured maps instance name to the create-nonce its claim network held at
	// teardown start, in teardownModeGeneration. It is fixed then, so a later claim
	// under a new nonce is never authorized for removal.
	captured map[string]string

	// reportOnDelete asks removeContainers to log stopped runners' exits and DinD
	// sidecars' IO first. Only TeardownAllocationReportingIO sets it.
	reportOnDelete bool
}

// mayRemove reports whether a teardown may remove a resource with these labels: the
// ADR-004 ownership predicate, then the mode's nonce check. Generation mode may read
// the live claim nonce, which is where an error comes from.
func (s *teardownScope) mayRemove(ctx context.Context, labels map[string]string) (bool, error) {
	if !spec.MatchesPredicate(labels, s.m.controllerID) {
		return false, nil
	}
	nonce := labels[spec.LabelCreateNonce]

	switch s.mode {
	case teardownModeExact:
		return nonce == s.exact, nil
	case teardownModeAll:
		// The ownership predicate is the only gate.
		return true, nil
	}

	// teardownModeGeneration.
	inst := labels[spec.LabelInstanceName]
	if gen, ok := s.captured[inst]; ok {
		// Remove only the captured generation, claim network included. A generation
		// that claimed the name mid-teardown has another nonce.
		return nonce == gen, nil
	}
	// No claim network at start: no generation to key to, so this is a leftover to
	// reap, unless a generation has since claimed the name. Read per decision, not per
	// phase, so a claim made after this phase's list is still protected.
	liveNonce, live, err := s.m.liveClaimNonce(ctx, inst)
	if err != nil {
		return false, err
	}
	if live && liveNonce != "" && nonce == liveNonce {
		return false, nil
	}
	return true, nil
}

// claimNoncesByInstance maps each instance name in f to the create-nonce its claim
// network holds. A network without one maps to "", which still keeps the instance out
// of mayRemove's fallback.
func (m *Manager) claimNoncesByInstance(ctx context.Context, f filters.Args) (map[string]string, error) {
	list, err := m.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return nil, fmt.Errorf("failed to list claim networks for teardown scoping: %w", err)
	}
	out := map[string]string{}
	for _, n := range list {
		if !spec.MatchesPredicate(n.Labels, m.controllerID) {
			continue
		}
		if n.Labels[spec.LabelResource] != spec.ResourceJobNetwork {
			continue
		}
		if inst := n.Labels[spec.LabelInstanceName]; inst != "" {
			out[inst] = n.Labels[spec.LabelCreateNonce]
		}
	}
	return out, nil
}

// liveClaimNonce reads the create-nonce instanceName's claim network holds now. The
// bool reports whether one exists.
func (m *Manager) liveClaimNonce(ctx context.Context, instanceName string) (string, bool, error) {
	f := m.instanceScopedFilter(instanceName)
	f.Add("label", spec.LabelResource+"="+spec.ResourceJobNetwork)
	list, err := m.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return "", false, fmt.Errorf("failed to read live claim nonce for %q: %w", instanceName, err)
	}
	for _, n := range list {
		if spec.MatchesPredicate(n.Labels, m.controllerID) && n.Labels[spec.LabelResource] == spec.ResourceJobNetwork {
			return n.Labels[spec.LabelCreateNonce], true, nil
		}
	}
	return "", false, nil
}

// roleRank orders containers for teardown: runner, DinD sidecar, anything else.
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
