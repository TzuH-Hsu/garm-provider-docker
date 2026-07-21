package topology

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// jobNetworkDriver is the driver for every per-job network (ADR-001: a labeled
// bridge network).
const jobNetworkDriver = "bridge"

// networkCleanupTimeout bounds the detached cleanup of a network an ambiguous
// NetworkCreate may have leaked (F7). The cleanup runs under a context detached
// from the caller's (context.WithoutCancel), so a caller cancellation — the
// very condition that can make NetworkCreate's own response ambiguous — cannot
// also abort the cleanup; this timeout keeps that detached work from hanging.
const networkCleanupTimeout = 30 * time.Second

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
	// created the network before failing (the "committed but response lost"
	// case). Remove any network tagged with THIS attempt's nonce so the failed
	// claim leaves nothing behind, then surface the original error (F7). The
	// cleanup runs under a context detached from the caller's with a bounded
	// timeout, so a caller cancellation cannot abort it, and any cleanup error
	// is logged AND joined onto the returned error rather than swallowed —
	// mirroring the M0 creation-guard pattern.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), networkCleanupTimeout)
	defer cancel()
	err = fmt.Errorf("failed to create job network for %q: %w", instanceName, cerr)
	if cleanupErr := m.bestEffortRemoveOwnNetwork(cleanupCtx, nonce); cleanupErr != nil {
		log.Printf("garm-provider-docker: CreateClaimNetwork: ambiguous-create cleanup for %q failed: %v", instanceName, cleanupErr)
		err = errors.Join(err, fmt.Errorf("ambiguous-create network cleanup for %q failed: %w", instanceName, cleanupErr))
	}
	return "", nil, err
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
// leaked (F7). It is nonce-scoped so it can never touch a peer's or a foreign
// network. Per-network removal errors (NotFound tolerated) are joined and
// returned so the caller can log/join them rather than silently swallow them.
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

// CreateWorkspaceVolume creates the per-job workspace volume (ADR-001), a
// named, labeled volume mounted at the runner workdir. Created in every mode.
func (m *Manager) CreateWorkspaceVolume(ctx context.Context, identity spec.AllocationIdentity, nonce string) error {
	return m.createFreshVolume(ctx, "workspace",
		spec.WorkspaceVolumeName(identity.InstanceName),
		identity.WorkspaceVolumeLabels(m.now()), nonce)
}

// CreateSocketVolume creates the per-job DinD socket volume (ADR-001): the
// shared, job-scoped volume dockerd exposes its unix socket over and the runner
// mounts to reach it. DinD modes only — never created in "none" mode.
func (m *Manager) CreateSocketVolume(ctx context.Context, identity spec.AllocationIdentity, nonce string) error {
	return m.createFreshVolume(ctx, "socket",
		spec.SocketVolumeName(identity.InstanceName),
		identity.SocketVolumeLabels(m.now()), nonce)
}

// CreateDindStateVolume creates the per-job dind-state volume (ADR-001):
// dockerd's /var/lib/docker data root, isolated per allocation so overlay/vfs
// layers never leak across jobs and are destroyed at teardown. DinD modes only.
func (m *Manager) CreateDindStateVolume(ctx context.Context, identity spec.AllocationIdentity, nonce string) error {
	return m.createFreshVolume(ctx, "dind-state",
		spec.DindStateVolumeName(identity.InstanceName),
		identity.DindStateVolumeLabels(m.now()), nonce)
}

// createFreshVolume creates a named, labeled, guaranteed-FRESH job-scoped
// volume, stamped with this attempt's create-nonce. Because the real daemon's
// VolumeCreate is idempotent on a duplicate name (WP1 finding: it silently
// returns the EXISTING volume with its ORIGINAL labels), a stale volume left by
// a crashed prior allocation of the same instance name would be silently
// reused — cross-job residue the acceptance criteria forbid. This helper
// detects that via the create-nonce and replaces the stale volume with a fresh,
// empty one. Holding the claim-marker network for this instance name guarantees
// no peer is racing this volume, so remove-and-recreate is safe.
//
// This is defense-in-depth behind the pre-create SweepStale (which removes a
// past-grace stale allocation wholesale before this runs); together they close
// the stale-content-reuse hazard the idempotent VolumeCreate opens. It backs
// all three job-scoped volumes (workspace, socket, dind-state) so the
// stale-replacement guarantee is defined in exactly one place.
func (m *Manager) createFreshVolume(ctx context.Context, kind, name string, labels map[string]string, nonce string) error {
	labels[spec.LabelCreateNonce] = nonce

	created, err := m.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels})
	if err != nil {
		return fmt.Errorf("failed to create %s volume %q: %w", kind, name, err)
	}
	if created.Labels[spec.LabelCreateNonce] == nonce {
		return nil // a genuinely fresh volume carries our nonce
	}

	// Idempotent hit: `created` is a PRE-EXISTING volume (the real daemon
	// returns the existing volume with its ORIGINAL labels on a duplicate name,
	// discarding ours). Before force-removing it, require the COMPLETE ownership
	// tuple to match this controller/allocation (F3): managed=true + this
	// controller-id + this instance-name + the expected resource label. On ANY
	// mismatch we FAIL CLOSED — a volume named `<instance>-workspace`/`-socket`/
	// `-dind-state` that we do not fully own is a foreign or other-controller
	// resource that merely collides on the deterministic name, and destroying it
	// would violate the red line "cleanup is allowlist-only, never touch what we
	// do not own." Only a volume we own (a stale leftover from a crashed prior
	// allocation of OUR instance name) is replaced.
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
	// Re-validate the recreated volume carries THIS attempt's nonce (F3, close
	// the TOCTOU): if the create idempotent-hit an existing volume again — a
	// concurrent recreation of the same name by something not holding our claim
	// marker — we did not get the fresh, empty volume we require, so fail closed
	// rather than proceed on residue we cannot vouch for.
	if recreated.Labels[spec.LabelCreateNonce] != nonce {
		return fmt.Errorf("failed to obtain a fresh %s volume %q: after replacement it still does not carry this attempt's create-nonce (a concurrent recreation of the same name?)", kind, name)
	}
	return nil
}

// ownsVolumeForReplacement reports whether an existing volume's labels satisfy
// the COMPLETE ownership tuple for this allocation (F3), so createFreshVolume
// may safely destroy and replace it: the full ADR-004 predicate (managed + this
// controller + has instance-name + NOT cache=true), plus an exact match on the
// instance-name and resource kind we intend to (re)create. A foreign volume
// carries none of these; an other-controller or other-allocation volume fails
// the controller/instance-name/resource comparison. Any mismatch means "not
// ours" and the caller fails closed rather than deleting it.
func (m *Manager) ownsVolumeForReplacement(existing, want map[string]string) bool {
	if !spec.MatchesPredicate(existing, m.controllerID) {
		return false
	}
	if existing[spec.LabelInstanceName] != want[spec.LabelInstanceName] {
		return false
	}
	return existing[spec.LabelResource] == want[spec.LabelResource]
}
