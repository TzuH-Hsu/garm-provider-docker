package topology

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// SweepOrphans opportunistically tears down abandoned allocations for this
// controller (ADR-004): an exited (or missing) runner whose allocation is past
// the grace window, or a claim-marker network / lingering volumes whose
// instance name has no live runner past the grace window. It runs during
// ListInstances — a call the provider is invoked for anyway — since there is no
// background daemon.
//
// It never touches foreign or other-controller resources (every query is
// label-scoped to this controller) and excludes any allocation younger than the
// grace window — a peer's in-flight CreateInstance whose runner container does
// not exist yet, or a just-finished job GARM may be about to delete itself.
// Best-effort: a per-instance error is logged and the sweep continues.
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

// SweepStale runs the orphan decision for a single instance name. As of F9 the
// pre-create hook runs the host-wide SweepOrphans instead, but this targeted
// variant is retained as a focused helper (and unit-tested boundary) for
// clearing exactly one instance-name's stale leftovers — a crashed prior
// allocation's stale workspace/socket/dind-state volume that the idempotent
// VolumeCreate would otherwise silently reuse. It applies the same grace
// windows as SweepOrphans, so a concurrent peer's fresh claim marker is never
// swept.
func (m *Manager) SweepStale(ctx context.Context, instanceName string) error {
	swept, err := m.sweepInstance(ctx, instanceName)
	if err != nil {
		return err
	}
	if swept {
		slog.InfoContext(ctx, "pre-create sweep removed a stale allocation", "instance", instanceName)
	}
	return nil
}

// sweepInstance applies the ADR-004 orphan decision to one instance name and,
// if the allocation is abandoned, tears it down wholesale. It distinguishes the
// two grace bases F5/F8 make explicit:
//
//   - a running runner container → active allocation, never swept (any age);
//   - an EXITED (or dead) runner → a just-finished/failed job: age it from the
//     runner's State.FinishedAt (F8) against the SHORT exitedGrace; past that,
//     tear it down;
//   - no runner container at all (or one still in the "created" state, never
//     started) → an in-flight or never-completed create: age the allocation
//     from its youngest resource's created-at label against the LONG
//     inflightGrace (F5); only past that hard deadline — which exceeds every
//     legitimate create phase, so a valid cold create is never swept — is it
//     abandoned.
//
// The decision is deliberately conservative: an unparseable clock (no FinishedAt
// on an exited runner, or no created-at when there is no runner) means "cannot
// age" and the allocation is skipped rather than risk deleting something live.
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
		hasRunningRunner bool
		hasExitedRunner  bool
		exitedFinishedAt time.Time
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
		if c.State == "running" {
			hasRunningRunner = true
			continue
		}
		// An exited/dead runner: read its FinishedAt (F8) via inspect — the list
		// summary does not carry it. Track the latest across any runners.
		if fa, ok := m.runnerFinishedAt(ctx, c.ID); ok {
			if !hasExitedRunner || fa.After(exitedFinishedAt) {
				exitedFinishedAt, hasExitedRunner = fa, true
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
	if hasRunningRunner {
		return false, nil // active allocation, never sweep
	}

	if hasExitedRunner {
		// Exited-runner grace (F8): measured from FinishedAt, SHORT window.
		if m.now().Sub(exitedFinishedAt) < m.exitedGrace {
			return false, nil // just-finished — do not race GARM's own delete
		}
		return m.TeardownAllocation(ctx, instanceName)
	}

	// No runner has run: an in-flight or never-completed create. Age from the
	// youngest resource's created-at against the LONG in-flight-create grace
	// (F5) — a valid cold create can exceed the short exited grace, so this
	// deadline must be the one that exceeds every create phase.
	if !haveYoungest {
		return false, nil // cannot age → conservatively skip
	}
	if m.now().Sub(youngest) < m.inflightGrace {
		return false, nil // too recent — a peer's still-in-progress create
	}

	return m.TeardownAllocation(ctx, instanceName)
}

// runnerFinishedAt inspects a runner container and returns its State.FinishedAt
// (F8), the moment the daemon recorded it exiting. The bool is false when the
// container is gone (raced with a delete), the inspect fails, or FinishedAt is
// absent/zero/unparseable — in which case the caller cannot age the allocation
// on the exited-runner clock and falls back to the conservative skip.
func (m *Manager) runnerFinishedAt(ctx context.Context, containerID string) (time.Time, bool) {
	inspected, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil || inspected.State == nil {
		return time.Time{}, false
	}
	raw := inspected.State.FinishedAt
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.IsZero() || t.Year() <= 1 {
		return time.Time{}, false
	}
	return t, true
}

// enumerateInstanceNames returns the distinct instance names across this
// controller's managed, job-scoped containers, networks, and volumes (the
// ADR-004 predicate), so SweepOrphans can consider every allocation whether or
// not its runner container still exists.
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

// parseCreatedAt reads the garm.docker/created-at label (RFC3339, as
// spec.AllocationIdentity's baseLabels writes it) into a time. The bool reports
// whether the label was present and well-formed.
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
