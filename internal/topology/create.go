package topology

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// jobNetworkDriver is the driver of every job network (ADR-001).
const jobNetworkDriver = "bridge"

// networkCleanupTimeout bounds the cleanup of a network an ambiguous NetworkCreate may
// have leaked. The cleanup runs detached from the caller's context, because a
// cancellation can be what made the create ambiguous.
const networkCleanupTimeout = 30 * time.Second

// CreateClaimNetwork creates the job network, the allocation's atomic claim marker:
// NetworkCreate fails with 409 on a duplicate name, unlike VolumeCreate (ADR-004). At
// most one return is set: netID on success, dupErr (exit 31) when a managed job network
// for this controller and instance name already holds the name, and err for anything
// else, including a name collision with a foreign network, which is never claimed.
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
			// Maps to exit 31 through execution.ResolveErrorToExitCode.
			return "", gErrors.NewDuplicateUserError(fmt.Sprintf("instance %q already exists", instanceName)), nil
		}
		return "", nil, fmt.Errorf("job network %q already exists but is not a managed job network for %q (foreign name collision); refusing to claim it", name, instanceName)
	}

	// Any other error is ambiguous: the daemon may have created the network before
	// failing. Remove what carries this attempt's nonce, detached so a caller
	// cancellation cannot abort it, and report a cleanup failure with the original
	// error.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), networkCleanupTimeout)
	defer cancel()
	err = fmt.Errorf("failed to create job network for %q: %w", instanceName, cerr)
	if cleanupErr := m.bestEffortRemoveOwnNetwork(cleanupCtx, nonce); cleanupErr != nil {
		slog.ErrorContext(ctx, "CreateClaimNetwork: ambiguous-create cleanup failed",
			"instance", instanceName, "nonce", nonce, "error", cleanupErr)
		err = errors.Join(err, fmt.Errorf("ambiguous-create network cleanup for %q failed: %w", instanceName, cleanupErr))
	}
	return "", nil, err
}

// existingClaimIsOurs reports whether the network holding name is a managed job network
// for this controller and instance name, rather than a foreign one colliding on the
// name.
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

// bestEffortRemoveOwnNetwork removes the networks carrying this attempt's nonce, so it
// cannot touch a peer's or a foreign network. It returns the joined removal errors.
func (m *Manager) bestEffortRemoveOwnNetwork(ctx context.Context, nonce string) error {
	list, err := m.cli.NetworkList(ctx, network.ListOptions{
		Filters: filtersForNonce(m.controllerID, nonce),
	})
	if err != nil {
		return fmt.Errorf("failed to list nonce-scoped networks for cleanup: %w", err)
	}
	var errs []error
	for _, n := range list {
		if n.Labels[spec.LabelCreateNonce] != nonce {
			continue
		}
		if rerr := m.cli.NetworkRemove(ctx, n.ID); rerr != nil && !errdefs.IsNotFound(rerr) {
			errs = append(errs, fmt.Errorf("failed to remove leaked network %s: %w", n.ID, rerr))
		}
	}
	return errors.Join(errs...)
}

// CreateWorkspaceVolume creates the per-job workspace volume, mounted at the runner
// workdir, in every mode (ADR-001).
func (m *Manager) CreateWorkspaceVolume(ctx context.Context, identity spec.AllocationIdentity, nonce string) error {
	return m.createFreshVolume(ctx, "workspace",
		spec.WorkspaceVolumeName(identity.InstanceName, nonce),
		identity.WorkspaceVolumeLabels(m.now()), nonce)
}

// CreateSocketVolume creates the per-job volume dockerd exposes its socket over and the
// runner mounts (ADR-001). DinD modes only.
func (m *Manager) CreateSocketVolume(ctx context.Context, identity spec.AllocationIdentity, nonce string) error {
	return m.createFreshVolume(ctx, "socket",
		spec.SocketVolumeName(identity.InstanceName, nonce),
		identity.SocketVolumeLabels(m.now()), nonce)
}

// CreateDindStateVolume creates the per-job volume for dockerd's /var/lib/docker, so
// image layers never leak across jobs (ADR-001). DinD modes only.
func (m *Manager) CreateDindStateVolume(ctx context.Context, identity spec.AllocationIdentity, nonce string) error {
	return m.createFreshVolume(ctx, "dind-state",
		spec.DindStateVolumeName(identity.InstanceName, nonce),
		identity.DindStateVolumeLabels(m.now()), nonce)
}

// createFreshVolume creates an empty job-scoped volume stamped with nonce. Its name
// embeds the nonce, so a prior generation's leftover has another name, and VolumeCreate
// can only hit a volume at this generation's own name (a retried step, for example).
// That volume is replaced if this allocation fully owns it and refused otherwise.
// Holding the claim marker means no peer is racing the replacement.
func (m *Manager) createFreshVolume(ctx context.Context, kind, name string, labels map[string]string, nonce string) error {
	labels[spec.LabelCreateNonce] = nonce

	created, err := m.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels})
	if err != nil {
		return fmt.Errorf("failed to create %s volume %q: %w", kind, name, err)
	}
	if created.Labels[spec.LabelCreateNonce] == nonce {
		return nil // a genuinely fresh volume carries our nonce
	}

	// An idempotent hit: the daemon returned an existing volume with its original
	// labels. A volume not fully ours is a foreign or other-controller one colliding on
	// the name, and cleanup never touches what we do not own.
	if !m.ownsVolumeForReplacement(created.Labels, labels) {
		return fmt.Errorf("refusing to replace %s volume %q: it already exists but does not carry this controller's full ownership tuple (managed + controller-id + instance-name + resource=%s) — it is a foreign or other-controller volume colliding on the name, not a stale allocation of ours", kind, name, labels[spec.LabelResource])
	}

	// It is ours: remove and recreate so no prior job's content survives.
	if err := m.cli.VolumeRemove(ctx, name, true); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stale %s volume %q could not be removed for replacement: %w", kind, name, err)
	}
	recreated, err := m.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels})
	if err != nil {
		return fmt.Errorf("failed to recreate a fresh %s volume %q: %w", kind, name, err)
	}
	// Check the nonce again: a concurrent recreation of this name by something not
	// holding the claim marker would hand back a volume whose contents cannot be
	// vouched for.
	if recreated.Labels[spec.LabelCreateNonce] != nonce {
		return fmt.Errorf("failed to obtain a fresh %s volume %q: after replacement it still does not carry this attempt's create-nonce (a concurrent recreation of the same name?)", kind, name)
	}
	return nil
}

// ownsVolumeForReplacement reports whether existing satisfies the ADR-004 predicate for
// this controller and matches want's instance name and resource kind.
func (m *Manager) ownsVolumeForReplacement(existing, want map[string]string) bool {
	if !spec.MatchesPredicate(existing, m.controllerID) {
		return false
	}
	if existing[spec.LabelInstanceName] != want[spec.LabelInstanceName] {
		return false
	}
	return existing[spec.LabelResource] == want[spec.LabelResource]
}
