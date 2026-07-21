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
// instanceName in the ADR-004 (network-last) order — runner container, then
// DinD sidecar (WP3), then job-scoped volumes (workspace, socket, dind-state),
// then the job network (the claim marker) LAST — for THIS controller. It is
// GENERATION-scoped (F4): at teardown start it reads the create-nonce the
// instance's claim-marker network currently holds and removes ONLY resources
// carrying that nonce, so a concurrent same-name CreateInstance that claims a
// NEW generation mid-teardown (its resources carry a different nonce) is never
// destroyed. It is fully idempotent: a NotFound at any step is tolerated. The
// bool reports whether anything existed to remove, so DeleteInstance can return
// exit 30 when the whole instance was already gone. Per-resource errors are
// joined and returned, but the teardown continues past a failure so one stuck
// resource does not strand the rest.
//
// The network is removed LAST (F4, ADR-004 amendment 2026-07-21): it is the
// allocation's claim marker, so holding it until every volume is gone keeps a
// concurrent same-name CreateInstance from claiming a new generation (its
// CreateClaimNetwork 409s on the still-present network) and creating volumes
// that this still-running teardown would then delete out from under it. The
// nonce scoping is the second, independent guarantee on top of that ordering:
// even in the fallback case where the claim network is already gone at teardown
// start (nothing to key a generation to), removal is scoped so a generation
// that claims the name mid-teardown is left untouched (see teardownScope).
//
// Cache and diagnostic volumes (ADR-003) are never touched: they carry no
// instance-name label, so the instance-scoped filter structurally excludes
// them, and spec.MatchesPredicate re-asserts the "NOT cache=true" conjunct
// defense-in-depth.
func (m *Manager) TeardownAllocation(ctx context.Context, instanceName string) (found bool, err error) {
	return m.teardownGeneration(ctx, m.instanceScopedFilter(instanceName))
}

// TeardownAll removes EVERY managed, job-scoped resource for this controller —
// runner containers, DinD sidecars (WP3), job-scoped volumes, and job networks —
// in the ADR-004 (network-last) order (all containers first, then all volumes,
// then all networks last, so no network is removed while it still has active
// endpoints and each allocation's claim marker is held until its volumes are
// gone). It is the manual-rescue teardown behind RemoveAllInstances (ADR-004):
// label-scoped to this controller, never a global wipe, and it never touches
// cache or diagnostic volumes (they carry no instance-name, so the predicate
// excludes them structurally). Best-effort: per-resource errors are joined and
// the teardown continues.
//
// It is GENERATION-scoped PER INSTANCE (F4): the create-nonce each instance's
// claim network currently holds is captured up front, and every removal is
// scoped to its own instance's captured generation, so a concurrent create that
// claims a new generation of some instance mid-rescue is never destroyed.
func (m *Manager) TeardownAll(ctx context.Context) error {
	_, err := m.teardownGeneration(ctx, spec.MatchPredicateFilters(m.controllerID))
	return err
}

// Rollback is the creation-guard teardown (ADR-004): on any error during
// CreateInstance after the claim marker exists, it removes only the resources
// THIS attempt created — those carrying this attempt's create-nonce — in the
// same container → volume → network order. Keying on the nonce means a
// concurrent peer's resources (a different nonce) are never touched: the M0
// NEW-2 property, now applied at claim-marker granularity across all resource
// kinds.
func (m *Manager) Rollback(ctx context.Context, instanceName, nonce string) error {
	scope := &teardownScope{m: m, mode: teardownModeExact, exact: nonce}
	_, err := m.runTeardown(ctx, m.instanceScopedNonceFilter(instanceName, nonce), scope)
	return err
}

// teardownGeneration removes containers, then volumes, then the network(s)
// matching f, in the ADR-004 (network-last) order, scoped to each instance's
// generation nonce (F4). It reads the create-nonce each instance's claim-marker
// network currently holds ONCE at the start (the authorized generation to
// remove) and passes it to a teardownScope, so only that generation's resources
// are removed and a generation that claims the name mid-teardown is protected.
func (m *Manager) teardownGeneration(ctx context.Context, f filters.Args) (bool, error) {
	captured, err := m.claimNoncesByInstance(ctx, f)
	if err != nil {
		return false, err
	}
	scope := &teardownScope{m: m, mode: teardownModeGeneration, captured: captured}
	return m.runTeardown(ctx, f, scope)
}

// runTeardown removes the containers, then volumes, then the network(s) matching
// f, in that ADR-004 (network-last) order, gating every removal through scope.
//
// Ordering is load-bearing (F4): containers come first so the network has no
// active endpoints when it is removed (the real daemon — and this repo's fake —
// reject removing a network with attached containers) AND so a volume is never
// removed while a container still references it (the real daemon rejects that
// too); the network — the allocation's claim marker — is removed LAST so a
// concurrent same-name create cannot claim a new generation and create volumes
// mid-teardown (ADR-004 amendment 2026-07-21). The generation is re-validated
// immediately before each destructive op via scope.mayRemove on the labels in
// hand, so a resource whose ownership/generation changed since the enumerating
// call is never removed.
func (m *Manager) runTeardown(ctx context.Context, f filters.Args, scope *teardownScope) (bool, error) {
	var (
		found bool
		errs  []error
	)

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

	// Network LAST (F4): the claim marker is held until every volume is gone.
	nFound, nErr := m.removeNetworks(ctx, f, scope)
	found = found || nFound
	if nErr != nil {
		errs = append(errs, nErr)
	}

	return found, errors.Join(errs...)
}

// removeContainers stops then force-removes every managed container matching f,
// runner before DinD sidecar (ADR-004 ordering). Force-remove reaps a
// container's ANONYMOUS volumes; the named workspace/socket/dind-state volumes
// survive and are removed by removeVolumes. NotFound is tolerated.
func (m *Manager) removeContainers(ctx context.Context, f filters.Args, scope *teardownScope) (bool, error) {
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
		ok, err := scope.mayRemove(ctx, c.Labels)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
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

// removeNetworks removes every managed job network matching f. It runs LAST
// (F4, after removeContainers and removeVolumes): the network's endpoints are
// already detached (the real daemon — and this repo's fake — reject removing a
// network with active endpoints), and holding the claim-marker network until
// the volumes are gone keeps a concurrent same-name create from claiming a new
// generation mid-teardown (ADR-004 amendment). NotFound is tolerated.
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

// removeVolumes removes every managed job-scoped volume matching f (workspace,
// and WP3's socket/dind-state). force=true also removes a volume still
// referenced by a stopped container — teardown removes containers first, so
// this is defense-in-depth. NotFound is tolerated.
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

// teardownMode selects a teardownScope's nonce-gating policy.
type teardownMode int

const (
	// teardownModeExact removes only resources carrying an EXACT create-nonce —
	// the creation-guard rollback path (THIS attempt's resources only).
	teardownModeExact teardownMode = iota
	// teardownModeGeneration removes each instance's CURRENT generation only:
	// the create-nonce its claim-marker network held at teardown start (F4).
	teardownModeGeneration
)

// teardownScope is the single gate every teardown removal passes through. It
// re-asserts the full ADR-004 ownership predicate (managed + controller + has
// instance-name + NOT cache=true) and layers the F4 generation-nonce guard on
// top, so a resource that slipped through a coarse Docker-side filter is never
// removed unless it genuinely belongs to this controller AND to the generation
// this teardown is authorized to remove.
//
// The captured map is fixed ONCE at teardown start, so a generation that claims
// an instance name AFTER the teardown began (a new claim network with a
// different nonce) is never removed by this teardown of the prior generation.
type teardownScope struct {
	m    *Manager
	mode teardownMode

	// exact is the required create-nonce in teardownModeExact (Rollback).
	exact string

	// captured maps instance-name -> the create-nonce that instance's
	// claim-marker network held at teardown START (teardownModeGeneration). A
	// resource is removable only when its instance's captured nonce equals the
	// resource's create-nonce.
	captured map[string]string
}

// mayRemove reports whether a teardown may remove a resource with these labels,
// applying the ADR-004 ownership predicate plus the F4 generation-nonce guard.
// It takes ctx because the generation-mode fallback re-reads the live claim
// nonce fresh, so the read that gates a removal is as current as possible.
func (s *teardownScope) mayRemove(ctx context.Context, labels map[string]string) (bool, error) {
	if !spec.MatchesPredicate(labels, s.m.controllerID) {
		return false, nil
	}
	nonce := labels[spec.LabelCreateNonce]

	if s.mode == teardownModeExact {
		return nonce == s.exact, nil
	}

	// teardownModeGeneration.
	inst := labels[spec.LabelInstanceName]
	if gen, ok := s.captured[inst]; ok {
		// A generation was claimed at teardown start: remove only its resources.
		// This also removes the claim network itself (it carries gen), completing
		// the teardown; a mid-teardown gen-B (different nonce) is left untouched.
		return nonce == gen, nil
	}
	// Fallback: no claim network for this instance at start, so there is no live
	// generation to key to and the resource is a leftover to reap — UNLESS a
	// claim network has since appeared (a generation that claimed the name
	// mid-teardown). Re-read the live claim nonce FRESH here, immediately before
	// the removal decision (not once per phase), so a generation that claims the
	// name between the phase's list and this decision is still protected: with
	// its nonce matching the live claim marker, its resources are kept, so a late
	// teardown can never delete a newer generation's resources.
	liveNonce, live, err := s.m.liveClaimNonce(ctx, inst)
	if err != nil {
		return false, err
	}
	if live && liveNonce != "" && nonce == liveNonce {
		return false, nil
	}
	return true, nil
}

// claimNoncesByInstance maps each instance-name to the create-nonce its
// claim-marker network currently holds, across the resources matching f. It is
// the generation-nonce source captured at teardown start (F4) to fix the
// authorized generation each instance's removal is scoped to. A network without
// a create-nonce maps the instance to the empty string (still recorded, so the
// fallback is not entered for it).
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

// liveClaimNonce reads the create-nonce the claim-marker network for a SINGLE
// instance currently holds, fresh. It is the generation-mode fallback's
// protection source (mayRemove): a leftover whose instance has no claim network
// at teardown start is reaped, but if a claim network has since reappeared its
// nonce identifies a newer generation whose resources must be kept. The bool
// reports whether a live claim network exists for the instance.
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
