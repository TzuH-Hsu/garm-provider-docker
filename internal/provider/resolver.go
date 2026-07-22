package provider

import (
	"context"
	"fmt"
	"strings"

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// resolve finds the managed runner container for a GARM_INSTANCE_ID by the
// instance-name label, AUTHORITATIVELY — the instance-name label is the single
// resolution key (NEW-H1/N2). GARM_INSTANCE_ID is always the GARM instance name:
// provider_id is the instance name (F6), and GARM falls back to the instance
// Name when provider_id is empty (research.md §1.E, ADR-004) — both equal the
// instance name, so a single label lookup satisfies GARM's "ProviderID or Name"
// contract. The bool reports whether a container was found; a genuine "gone" is
// (zero, false, nil), distinct from an error.
//
// There is deliberately NO raw inspect-by-ID fallback (dropped 2026-07-21,
// NEW-H1). The pre-F6 identity was the runner's container ID, but provider_id
// has ALWAYS been the instance name in this pre-release provider — there are no
// deployed container-ID provider_ids to migrate — so a raw inspect-by-ID would
// serve no legitimate case while opening a residual ambiguity: an instance name
// that happens to be container-ID-shaped could inspect-resolve a FOREIGN or
// other-generation container by ID even though it is not the runner that owns
// that instance-name label. Resolving solely by the instance-name label (with a
// full ownership check) removes that ambiguity by construction — the id-shaped
// name resolves ITS OWN allocation via the label, never an id collision.
//
// Every candidate is validated for ownership (spec.IsManagedRunner): a container
// that does not carry this controller's managed/runner labels is treated as
// not-found, so a foreign or wrong-role container that happens to collide on a
// Docker ID or name is never returned to a mutating caller (Delete/Stop/Start),
// which could otherwise touch a container this provider does not own (F3).
func (p *Provider) resolve(ctx context.Context, instanceID string) (types.ContainerJSON, bool, error) {
	return p.resolveByOwnedLabel(ctx, instanceID)
}

// resolveByOwnedLabel finds this controller's RUNNER container by the
// garm.docker/instance-name label, scoped to role=runner
// (managedRunnerByInstanceNameFilter) so the DinD sidecar that shares the
// instance-name label is never selected (F6/N1). It ITERATES the matches to the
// first owned runner rather than trusting list[0], and re-validates ownership
// defense-in-depth before returning it. A NotFound on any candidate's inspect
// (raced with a concurrent delete) is skipped, not surfaced.
func (p *Provider) resolveByOwnedLabel(ctx context.Context, instanceID string) (types.ContainerJSON, bool, error) {
	list, err := p.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: p.managedRunnerByInstanceNameFilter(instanceID),
	})
	if err != nil {
		return types.ContainerJSON{}, false, fmt.Errorf("failed to list containers for %q: %w", instanceID, err)
	}
	for _, c := range list {
		inspected, err := p.cli.ContainerInspect(ctx, c.ID)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue // raced with a concurrent delete; try the next candidate
			}
			return types.ContainerJSON{}, false, fmt.Errorf("failed to inspect %q: %w", c.ID, err)
		}
		if p.ownsRunner(inspected) {
			return inspected, true, nil
		}
	}
	return types.ContainerJSON{}, false, nil
}

// ownsRunner reports whether an inspected container carries this controller's
// managed-runner ownership labels (ADR-004 F3).
func (p *Provider) ownsRunner(c types.ContainerJSON) bool {
	if c.Config == nil {
		return false
	}
	return spec.IsManagedRunner(c.Config.Labels, p.controllerID)
}

// hasManagedAllocationResources reports whether any managed job-scoped resource
// for this controller lingers under instanceName — a container (e.g. the
// PRIVILEGED DinD sidecar), network, or volume. DeleteInstance uses it when no
// owned RUNNER container resolves, so an allocation whose runner has already
// been removed out of band but whose sidecar/network/volumes still exist is
// still fully torn down (F6, ADR-004: "finds the runner container already gone
// but its network or volumes still lingering").
//
// The container check is what closes the F6 red line specifically: the runner
// is gone, but the sidecar (role=dind) is NOT a runner, so resolve() never
// returns it — without checking here it could leak. Every match re-asserts
// spec.MatchesPredicate defense-in-depth against the labels in hand.
func (p *Provider) hasManagedAllocationResources(ctx context.Context, instanceName string) (bool, error) {
	f := p.managedByInstanceNameFilter(instanceName)

	conts, err := p.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return false, fmt.Errorf("failed to list containers for %q: %w", instanceName, err)
	}
	for _, c := range conts {
		if spec.MatchesPredicate(c.Labels, p.controllerID) {
			return true, nil
		}
	}

	nets, err := p.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return false, fmt.Errorf("failed to list networks for %q: %w", instanceName, err)
	}
	for _, n := range nets {
		if spec.MatchesPredicate(n.Labels, p.controllerID) {
			return true, nil
		}
	}

	vols, err := p.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return false, fmt.Errorf("failed to list volumes for %q: %w", instanceName, err)
	}
	for _, v := range vols.Volumes {
		if v != nil && spec.MatchesPredicate(v.Labels, p.controllerID) {
			return true, nil
		}
	}
	return false, nil
}

// notFoundError builds the garm-provider-common not-found error, which
// execution.ResolveErrorToExitCode maps to exit code 30 (GARM treats a
// DeleteInstance 30 as success).
func notFoundError(instanceID string) error {
	return gErrors.NewNotFoundError("instance %q not found", instanceID)
}

// toProviderInstance maps a fully inspected container to a ProviderInstance.
//
// ProviderID is the GARM instance NAME (from the instance-name label), not the
// container ID (F6, ADR-004 amendment): CreateInstance returns the instance
// name as provider_id, so GetInstance/ListInstances MUST report the same stable
// identity, or GARM could overwrite its stored provider_id with a container ID
// and reintroduce the delete-by-stale-container-id leak this fix closes.
//
// ProviderFault (research.md §1.E's ProviderInstance.provider_fault) is
// populated whenever status maps to InstanceError: this is the one channel
// through which the container's fault detail can legitimately reach GARM
// (M3-W2 error-taxonomy audit, see taxonomy.go's package doc for why
// CreateInstance's OWN failures cannot use this same field). It never
// includes anything from the container's Config (env, labels, credentials
// tmpfs contents) — only c.State fields the daemon itself reports (exit
// code, OOM flag, dead flag, its own Error string, FinishedAt), so it can
// never carry a credential this provider handled.
func toProviderInstance(c types.ContainerJSON) params.ProviderInstance {
	labels := map[string]string{}
	if c.Config != nil {
		labels = c.Config.Labels
	}

	name := labels[spec.LabelInstanceName]
	if name == "" {
		name = strings.TrimPrefix(c.Name, "/")
	}

	status := params.InstanceStatusUnknown
	var fault []byte
	if c.State != nil {
		status = mapContainerStatus(c.State.Status, c.State.OOMKilled, c.State.Dead)
		if status == params.InstanceError {
			fault = []byte(containerFaultMessage(c.State))
		}
	}

	return params.ProviderInstance{
		ProviderID:    name,
		Name:          name,
		OSType:        params.OSType(labels[spec.LabelOSType]),
		OSArch:        params.OSArch(labels[spec.LabelOSArch]),
		Status:        status,
		Addresses:     addressesFromInspect(c),
		ProviderFault: fault,
	}
}

// containerFaultMessage builds a short, credential-free diagnostic string
// from an inspected container's daemon-reported state, for
// ProviderInstance.ProviderFault. Called only when the container's status has
// already mapped to InstanceError (OOM-killed or daemon-"dead"), state is
// guaranteed non-nil by that caller.
func containerFaultMessage(state *types.ContainerState) string {
	switch {
	case state.OOMKilled:
		return fmt.Sprintf("container was killed for out-of-memory (exit_code=%d, finished_at=%s)", state.ExitCode, state.FinishedAt)
	case state.Dead:
		msg := "container is in the daemon's dead state"
		if state.Error != "" {
			msg += ": " + state.Error
		}
		return msg
	default:
		return "container reported an error state"
	}
}

// toProviderInstanceFromSummary maps a container list summary to a
// ProviderInstance. It uses the summary's state string only; OOM detail is
// not available in a list summary, so a caller needing exact error status
// for an OOM-killed container should GetInstance it. A "dead" state still
// maps to error via the state string, and ProviderFault is populated
// best-effort from the summary's own human-readable Status field (already in
// hand — no extra inspect call, which would defeat the point of a lightweight
// list) — e.g. a daemon-supplied dead-state description.
//
// ProviderID is the GARM instance NAME, matching CreateInstance and
// toProviderInstance (F6): a stable identity resolvable by label independently
// of the runner container.
func toProviderInstanceFromSummary(c types.Container) params.ProviderInstance {
	name := c.Labels[spec.LabelInstanceName]
	if name == "" && len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	status := mapContainerStatus(c.State, false, c.State == "dead")
	var fault []byte
	if status == params.InstanceError && c.Status != "" {
		fault = []byte(c.Status)
	}
	return params.ProviderInstance{
		ProviderID:    name,
		Name:          name,
		OSType:        params.OSType(c.Labels[spec.LabelOSType]),
		OSArch:        params.OSArch(c.Labels[spec.LabelOSArch]),
		Status:        status,
		ProviderFault: fault,
	}
}

// addressesFromInspect extracts the container's per-network IPv4 addresses.
// In M0 "none" mode a runner is on the default bridge; the addresses are
// reported to GARM as private. Best-effort: an inspect without network
// settings yields no addresses.
func addressesFromInspect(c types.ContainerJSON) []params.Address {
	if c.NetworkSettings == nil {
		return nil
	}
	var addrs []params.Address
	for _, ep := range c.NetworkSettings.Networks {
		if ep == nil || ep.IPAddress == "" {
			continue
		}
		addrs = append(addrs, params.Address{
			Address: ep.IPAddress,
			Type:    params.PrivateAddress,
		})
	}
	return addrs
}
