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
// may be either the provider-assigned ProviderID (now the instance name — F6)
// or the GARM instance Name — GARM falls back to Name when ProviderID is empty
// (research.md §1.E, ADR-004). It tries the exact instance-name label lookup
// FIRST (N2), then falls back to an inspect-by-ID for the legacy container-ID
// case. The bool reports whether a container was found; a genuine "gone" is
// (zero, false, nil), distinct from an error.
//
// The label lookup comes first specifically to fix N2: if instanceID happens to
// equal ANOTHER runner's container ID (or a prefix a raw inspect-by-ID would
// match), an ID-first resolve would return that OTHER allocation's container.
// Keying on the instance-name label first resolves to the runner that actually
// OWNS the name; only when no owned runner carries that instance-name label do
// we treat instanceID as a raw container ID (the pre-F6 identity).
//
// Every successful inspect is validated for ownership (spec.IsManagedRunner):
// a container that does not carry this controller's managed/runner labels is
// treated as not-found, so a foreign or wrong-role container that happens to
// collide on a Docker ID or name is never returned to a mutating caller
// (Delete/Stop/Start), which could otherwise touch a container this provider
// does not own (ADR-004 F3).
func (p *Provider) resolve(ctx context.Context, instanceID string) (types.ContainerJSON, bool, error) {
	// N2: exact instance-name label lookup FIRST.
	inspected, found, err := p.resolveByOwnedLabel(ctx, instanceID)
	if err != nil {
		return types.ContainerJSON{}, false, err
	}
	if found {
		return inspected, true, nil
	}

	// Legacy container-ID fallback: no owned runner carries instanceID as its
	// instance-name label, so treat it as a raw container ID (or Docker name).
	// Validate ownership so a foreign/wrong-role container colliding on that ID
	// or name is never returned (ADR-004 F3).
	inspected, err = p.cli.ContainerInspect(ctx, instanceID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return types.ContainerJSON{}, false, nil
		}
		return types.ContainerJSON{}, false, fmt.Errorf("failed to inspect %q: %w", instanceID, err)
	}
	if !p.ownsRunner(inspected) {
		return types.ContainerJSON{}, false, nil
	}
	return inspected, true, nil
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
