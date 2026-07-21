package topology

import (
	"context"
	"fmt"

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// jobNetworkDriver is the driver for every per-job network (ADR-001: a labeled
// bridge network).
const jobNetworkDriver = "bridge"

// CreateClaimNetwork creates the per-job network — ADR-004's atomic claim
// marker, the FIRST resource created for any allocation, stamped with
// instance-name, created-at, and this attempt's create-nonce at the instant of
// its creation. NetworkCreate's unconditional 409-on-duplicate (WP1 finding,
// unlike VolumeCreate) is the duplicate-detection primitive.
//
// The three returns are mutually exclusive:
//   - success: netID is the created network's ID; dupErr and err are nil.
//   - genuine duplicate: dupErr is a duplicate error (exit 31) — an existing
//     managed job network for THIS controller and instance-name already holds
//     the name, so another CreateInstance owns this allocation. netID is empty.
//   - hard error: err is non-nil. This covers a name collision against a
//     FOREIGN (unmanaged, or other-controller) network — which must never be
//     claimed as our own duplicate — and any non-conflict create failure, after
//     which a network this attempt may have ambiguously leaked (matched by name
//     AND this attempt's nonce) is best-effort removed so a failed claim never
//     strands a half-created marker.
func (m *Manager) CreateClaimNetwork(ctx context.Context, identity spec.AllocationIdentity, nonce string, internalNet bool) (netID string, dupErr error, err error) {
	instanceName := identity.InstanceName
	name := spec.JobNetworkName(instanceName)

	labels := identity.NetworkLabels(m.now())
	labels[spec.LabelCreateNonce] = nonce

	resp, cerr := m.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver:   jobNetworkDriver,
		Internal: internalNet,
		Labels:   labels,
	})
	if cerr == nil {
		return resp.ID, nil, nil
	}

	if errdefs.IsConflict(cerr) {
		ours, cerr2 := m.existingClaimIsOurs(ctx, name, instanceName)
		if cerr2 != nil {
			return "", nil, cerr2
		}
		if ours {
			// gErrors.NewDuplicateUserError maps to exit 31 via
			// execution.ResolveErrorToExitCode.
			return "", gErrors.NewDuplicateUserError(fmt.Sprintf("instance %q already exists", instanceName)), nil
		}
		return "", nil, fmt.Errorf("job network %q already exists but is not a managed job network for %q (foreign name collision); refusing to claim it", name, instanceName)
	}

	// A non-conflict NetworkCreate error is ambiguous — the daemon may have
	// created the network before failing. Remove any network tagged with THIS
	// attempt's nonce so the failed claim leaves nothing behind, then surface
	// the original error.
	m.bestEffortRemoveOwnNetwork(ctx, nonce)
	return "", nil, fmt.Errorf("failed to create job network for %q: %w", instanceName, cerr)
}

// existingClaimIsOurs reports whether the network already holding `name` is a
// managed job network for this controller and instance-name — i.e. a genuine
// duplicate — as opposed to a foreign network that merely collides on the name.
// It uses a label-scoped NetworkList (managed + controller + instance-name +
// resource=job-network); a foreign network carries none of those labels, so it
// never matches.
func (m *Manager) existingClaimIsOurs(ctx context.Context, name, instanceName string) (bool, error) {
	f := m.instanceScopedFilter(instanceName)
	f.Add("label", spec.LabelResource+"="+spec.ResourceJobNetwork)
	list, err := m.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return false, fmt.Errorf("failed to classify existing job network %q for %q: %w", name, instanceName, err)
	}
	for _, n := range list {
		if n.Name == name && spec.MatchesPredicate(n.Labels, m.controllerID) {
			return true, nil
		}
	}
	return false, nil
}

// bestEffortRemoveOwnNetwork removes any network carrying this attempt's
// create-nonce, used to clean up a network an ambiguous NetworkCreate may have
// leaked. It is nonce-scoped so it can never touch a peer's or a foreign
// network; failures are ignored (best-effort).
func (m *Manager) bestEffortRemoveOwnNetwork(ctx context.Context, nonce string) {
	list, err := m.cli.NetworkList(ctx, network.ListOptions{
		Filters: filtersForNonce(m.controllerID, nonce),
	})
	if err != nil {
		return
	}
	for _, n := range list {
		if n.Labels[spec.LabelCreateNonce] != nonce {
			continue
		}
		_ = m.cli.NetworkRemove(ctx, n.ID)
	}
}

// CreateWorkspaceVolume creates the per-job workspace volume (ADR-001), a
// named, labeled volume mounted at the runner workdir. Because the real
// daemon's VolumeCreate is idempotent on a duplicate name (WP1 finding: it
// silently returns the EXISTING volume with its ORIGINAL labels), a stale
// volume left by a crashed prior allocation of the same instance name would be
// silently reused — cross-job residue the acceptance criteria forbid. This
// method detects that via the create-nonce and replaces the stale volume with a
// fresh, empty one. Holding the claim-marker network for this instance name
// guarantees no peer is racing this volume, so remove-and-recreate is safe.
//
// This is defense-in-depth behind the pre-create SweepStale (which removes a
// past-grace stale allocation wholesale before this runs); together they close
// the stale-content-reuse hazard the idempotent VolumeCreate opens.
func (m *Manager) CreateWorkspaceVolume(ctx context.Context, identity spec.AllocationIdentity, nonce string) error {
	name := spec.WorkspaceVolumeName(identity.InstanceName)
	labels := identity.WorkspaceVolumeLabels(m.now())
	labels[spec.LabelCreateNonce] = nonce

	created, err := m.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels})
	if err != nil {
		return fmt.Errorf("failed to create workspace volume for %q: %w", identity.InstanceName, err)
	}
	if created.Labels[spec.LabelCreateNonce] == nonce {
		return nil // a genuinely fresh volume carries our nonce
	}

	// Idempotent hit: `created` is a stale volume with a different (or absent)
	// nonce. Remove and recreate so no prior job's content survives.
	if err := m.cli.VolumeRemove(ctx, name, true); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stale workspace volume %q could not be removed for replacement: %w", name, err)
	}
	if _, err := m.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels}); err != nil {
		return fmt.Errorf("failed to recreate a fresh workspace volume for %q: %w", identity.InstanceName, err)
	}
	return nil
}
