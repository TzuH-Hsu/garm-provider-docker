package topology

import (
	"context"
	"errors"
	"fmt"
	"log"
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
			log.Printf("garm-provider-docker: orphan sweep of %q failed: %v", name, err)
			errs = append(errs, err)
			continue
		}
		if swept {
			log.Printf("garm-provider-docker: orphan sweep tore down abandoned allocation %q", name)
		}
	}
	return errors.Join(errs...)
}

// SweepStale runs the orphan decision for a single instance name — the
// pre-create sweep CreateInstance runs before it reuses the name, so a crashed
// prior allocation's leftovers (a stale workspace/socket/dind-state volume that
// the idempotent VolumeCreate would otherwise silently reuse) are removed
// first. It applies the same grace window as SweepOrphans, so a concurrent
// peer's fresh claim marker — younger than the window — is never swept.
func (m *Manager) SweepStale(ctx context.Context, instanceName string) error {
	swept, err := m.sweepInstance(ctx, instanceName)
	if err != nil {
		return err
	}
	if swept {
		log.Printf("garm-provider-docker: pre-create sweep removed a stale allocation for %q", instanceName)
	}
	return nil
}

// sweepInstance applies the ADR-004 orphan decision to one instance name and,
// if the allocation is abandoned, tears it down wholesale. The rule (a single
// predicate, evaluated over the allocation's containers, network, and volumes):
//
//   - a running runner container → active allocation, never swept (any age);
//   - otherwise, the allocation's youngest resource (by created-at label) is
//     within the grace window → too recent to sweep (a peer's in-flight create,
//     or a just-finished job we must not race GARM's own delete for);
//   - otherwise → abandoned (an exited-runner or never-completed create past
//     grace) → TeardownAllocation.
//
// The "youngest resource within grace ⇒ skip" test is deliberately
// conservative: if anything about the allocation is recent, it is treated as
// in-flight/recent, which is what keeps the concurrency-safe claim-marker
// guarantee (ADR-004) — a peer that just created its claim network is never
// swept out from under its still-running CreateInstance.
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
		if spec.MatchesPredicate(c.Labels, m.controllerID) &&
			c.Labels[spec.LabelRole] == spec.RoleRunner && c.State == "running" {
			hasRunningRunner = true
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
	// A malformed/absent created-at means we cannot age the allocation; skip it
	// conservatively rather than risk deleting something in flight.
	if !haveYoungest {
		return false, nil
	}
	if m.now().Sub(youngest) < m.grace {
		return false, nil // too recent — an in-flight peer or a just-finished job
	}

	return m.TeardownAllocation(ctx, instanceName)
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
