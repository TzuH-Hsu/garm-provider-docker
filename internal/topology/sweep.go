package topology

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// SweepOrphans tears down this controller's abandoned allocations, as sweepInstance
// decides them, logging an exited runner's exit details first (reportExitedRunner). It
// runs at the start of CreateInstance and in ListInstances, since there is no
// background process (ADR-004, Orphan sweep). Every query is label-scoped to this
// controller. A per-instance error is logged and joined, and the sweep continues.
func (m *Manager) SweepOrphans(ctx context.Context) error {
	names, err := m.enumerateInstanceNames(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for name := range names {
		swept, err := m.sweepInstance(ctx, name)
		if err != nil {
			slog.WarnContext(ctx, "orphan sweep of instance failed", "instance", name, "error", err)
			errs = append(errs, err)
			continue
		}
		if swept {
			slog.InfoContext(ctx, "orphan sweep tore down abandoned allocation", "instance", name)
		}
	}
	return errors.Join(errs...)
}

// sweepInstance tears instanceName's allocation down if it is abandoned, and reports
// whether anything was removed:
//
//   - a running, paused or restarting runner is an active allocation, at any age;
//   - a runner that exited is aged from State.FinishedAt against exitedGrace;
//   - otherwise (no runner, one never started, or no readable FinishedAt) the allocation is
//     aged from its youngest created-at against inflightGrace, and skipped if it has none.
//
// A failed runner inspect returns the error and removes nothing, leaving the decision to
// a later sweep.
func (m *Manager) sweepInstance(ctx context.Context, instanceName string) (bool, error) {
	f := m.instanceScopedFilter(instanceName)

	containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return false, fmt.Errorf("sweep: failed to list containers for %q: %w", instanceName, err)
	}
	networks, err := m.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return false, fmt.Errorf("sweep: failed to list networks for %q: %w", instanceName, err)
	}
	vols, err := m.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return false, fmt.Errorf("sweep: failed to list volumes for %q: %w", instanceName, err)
	}

	var (
		hasResource      bool
		hasActiveRunner  bool
		hasExitedRunner  bool
		exitedFinishedAt time.Time
		latestExited     exitedRunner
		youngest         time.Time
		haveYoungest     bool
	)
	note := func(labels map[string]string) {
		if !spec.MatchesPredicate(labels, m.controllerID) {
			return
		}
		hasResource = true
		if t, ok := parseCreatedAt(labels); ok && (!haveYoungest || t.After(youngest)) {
			youngest, haveYoungest = t, true
		}
	}

	for _, c := range containers {
		note(c.Labels)
		if !spec.MatchesPredicate(c.Labels, m.controllerID) || c.Labels[spec.LabelRole] != spec.RoleRunner {
			continue
		}
		if isActiveRunnerState(c.State) {
			hasActiveRunner = true
			continue
		}
		// The list summary has no FinishedAt. Track the latest across any runners.
		info, fa, ok, err := m.runnerFinishedAt(ctx, c.ID)
		if err != nil {
			return false, fmt.Errorf("sweep: failed to inspect runner %s of %q: %w", c.ID, instanceName, err)
		}
		if ok {
			if !hasExitedRunner || fa.After(exitedFinishedAt) {
				exitedFinishedAt, hasExitedRunner = fa, true
				latestExited = exitedRunner{summary: c, info: info}
			}
		}
	}
	for _, n := range networks {
		note(n.Labels)
	}
	for _, v := range vols.Volumes {
		if v != nil {
			note(v.Labels)
		}
	}

	if !hasResource {
		return false, nil // nothing for this instance name
	}
	if hasActiveRunner {
		return false, nil // active allocation, never sweep
	}

	if hasExitedRunner {
		if m.now().Sub(exitedFinishedAt) < m.exitedGrace {
			return false, nil // just finished: do not race GARM's own delete
		}
		// It exited on its own and GARM has not deleted it: record why while the
		// container still holds its exit state and logs.
		m.reportExitedRunner(ctx, latestExited, containers)
		return m.TeardownAllocation(ctx, instanceName)
	}

	// No runner with an exit time: an in-flight or never-completed create, whose
	// deadline must exceed every create phase.
	if !haveYoungest {
		return false, nil // cannot age, so skip
	}
	if m.now().Sub(youngest) < m.inflightGrace {
		return false, nil // too recent: a peer's still-in-progress create
	}

	return m.TeardownAllocation(ctx, instanceName)
}

// isActiveRunnerState reports whether the daemon still holds a runner in state as live.
// Paused and restarting runners have no FinishedAt from this run, and can hold a job.
func isActiveRunnerState(state string) bool {
	switch state {
	case container.StateRunning, container.StatePaused, container.StateRestarting:
		return true
	}
	return false
}

// runnerFinishedAt returns a runner's inspect and State.FinishedAt. The bool is false
// when the runner is already gone or the time is absent, zero or unparseable. Any other
// inspect failure is returned as an error.
func (m *Manager) runnerFinishedAt(ctx context.Context, containerID string) (types.ContainerJSON, time.Time, bool, error) {
	inspected, err := m.cli.ContainerInspect(ctx, containerID)
	if errdefs.IsNotFound(err) {
		return types.ContainerJSON{}, time.Time{}, false, nil
	}
	if err != nil {
		return types.ContainerJSON{}, time.Time{}, false, err
	}
	if inspected.State == nil {
		return types.ContainerJSON{}, time.Time{}, false, nil
	}
	raw := inspected.State.FinishedAt
	if raw == "" {
		return types.ContainerJSON{}, time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.IsZero() || t.Year() <= 1 {
		return types.ContainerJSON{}, time.Time{}, false, nil
	}
	return inspected, t, true, nil
}

// enumerateInstanceNames returns the distinct instance names across this controller's
// job-scoped containers, networks and volumes, so an allocation is considered whether
// or not its runner still exists.
func (m *Manager) enumerateInstanceNames(ctx context.Context) (map[string]struct{}, error) {
	f := spec.MatchPredicateFilters(m.controllerID)
	names := map[string]struct{}{}

	add := func(labels map[string]string) {
		if !spec.MatchesPredicate(labels, m.controllerID) {
			return
		}
		if name := labels[spec.LabelInstanceName]; name != "" {
			names[name] = struct{}{}
		}
	}

	containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("sweep: failed to list containers: %w", err)
	}
	for _, c := range containers {
		add(c.Labels)
	}

	networks, err := m.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return nil, fmt.Errorf("sweep: failed to list networks: %w", err)
	}
	for _, n := range networks {
		add(n.Labels)
	}

	vols, err := m.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return nil, fmt.Errorf("sweep: failed to list volumes: %w", err)
	}
	for _, v := range vols.Volumes {
		if v != nil {
			add(v.Labels)
		}
	}

	return names, nil
}

// parseCreatedAt reads the created-at label. The bool reports whether it was present
// and well-formed.
func parseCreatedAt(labels map[string]string) (time.Time, bool) {
	raw := labels[spec.LabelCreatedAt]
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
