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

// resolve finds the managed runner container for a GARM_INSTANCE_ID, which
// may be either the provider-assigned container ID (ProviderID) or the GARM
// instance Name — GARM falls back to Name when ProviderID is empty
// (research.md §1.E, ADR-004). It first tries an inspect-by-ID (which the
// daemon also matches against container names), then falls back to a label
// filter on garm.docker/instance-name. The bool reports whether a container
// was found; a genuine "gone" is (zero, false, nil), distinct from an error.
//
// Every successful inspect is validated for ownership (spec.IsManagedRunner):
// a container that does not carry this controller's managed/runner labels is
// treated as not-found, so a foreign or wrong-role container that happens to
// collide on a Docker ID or name is never returned to a mutating caller
// (Delete/Stop/Start), which could otherwise touch a container this provider
// does not own (ADR-004 F3).
func (p *Provider) resolve(ctx context.Context, instanceID string) (types.ContainerJSON, bool, error) {
	inspected, err := p.cli.ContainerInspect(ctx, instanceID)
	if err == nil {
		if !p.ownsRunner(inspected) {
			// A container exists under this ID/name but is not ours. Do NOT
			// return it; fall through to the owned-label lookup, which can
			// still find our container when instanceID was a GARM Name whose
			// Docker name is taken by a foreign container.
			return p.resolveByOwnedLabel(ctx, instanceID)
		}
		return inspected, true, nil
	}
	if !errdefs.IsNotFound(err) {
		return types.ContainerJSON{}, false, fmt.Errorf("failed to inspect %q: %w", instanceID, err)
	}

	// The id was likely a GARM instance Name that differs from the
	// container's (lowercased) Docker name. Look it up by the instance-name
	// label instead.
	return p.resolveByOwnedLabel(ctx, instanceID)
}

// resolveByOwnedLabel finds this controller's runner container by the
// garm.docker/instance-name label (the label filter already scopes to
// managed=true + this controller-id), re-validating ownership defense-in-
// depth before returning it.
func (p *Provider) resolveByOwnedLabel(ctx context.Context, instanceID string) (types.ContainerJSON, bool, error) {
	list, err := p.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: p.managedByInstanceNameFilter(instanceID),
	})
	if err != nil {
		return types.ContainerJSON{}, false, fmt.Errorf("failed to list containers for %q: %w", instanceID, err)
	}
	if len(list) == 0 {
		return types.ContainerJSON{}, false, nil
	}

	inspected, err := p.cli.ContainerInspect(ctx, list[0].ID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			// Raced with a concurrent delete between list and inspect;
			// treat as gone rather than an error.
			return types.ContainerJSON{}, false, nil
		}
		return types.ContainerJSON{}, false, fmt.Errorf("failed to inspect %q: %w", list[0].ID, err)
	}
	if !p.ownsRunner(inspected) {
		return types.ContainerJSON{}, false, nil
	}
	return inspected, true, nil
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
	if c.State != nil {
		status = mapContainerStatus(c.State.Status, c.State.OOMKilled, c.State.Dead)
	}

	return params.ProviderInstance{
		ProviderID: name,
		Name:       name,
		OSType:     params.OSType(labels[spec.LabelOSType]),
		OSArch:     params.OSArch(labels[spec.LabelOSArch]),
		Status:     status,
		Addresses:  addressesFromInspect(c),
	}
}

// toProviderInstanceFromSummary maps a container list summary to a
// ProviderInstance. It uses the summary's state string only; OOM detail is
// not available in a list summary, so a caller needing exact error status
// for an OOM-killed container should GetInstance it. A "dead" state still
// maps to error via the state string.
//
// ProviderID is the GARM instance NAME, matching CreateInstance and
// toProviderInstance (F6): a stable identity resolvable by label independently
// of the runner container.
func toProviderInstanceFromSummary(c types.Container) params.ProviderInstance {
	name := c.Labels[spec.LabelInstanceName]
	if name == "" && len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	return params.ProviderInstance{
		ProviderID: name,
		Name:       name,
		OSType:     params.OSType(c.Labels[spec.LabelOSType]),
		OSArch:     params.OSArch(c.Labels[spec.LabelOSArch]),
		Status:     mapContainerStatus(c.State, false, c.State == "dead"),
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
