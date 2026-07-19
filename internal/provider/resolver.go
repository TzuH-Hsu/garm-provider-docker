package provider

import (
	"context"
	"fmt"
	"strings"

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
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
func (p *Provider) resolve(ctx context.Context, instanceID string) (types.ContainerJSON, bool, error) {
	inspected, err := p.cli.ContainerInspect(ctx, instanceID)
	if err == nil {
		return inspected, true, nil
	}
	if !errdefs.IsNotFound(err) {
		return types.ContainerJSON{}, false, fmt.Errorf("failed to inspect %q: %w", instanceID, err)
	}

	// The id was likely a GARM instance Name that differs from the
	// container's (lowercased) Docker name. Look it up by the instance-name
	// label instead.
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

	inspected, err = p.cli.ContainerInspect(ctx, list[0].ID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			// Raced with a concurrent delete between list and inspect;
			// treat as gone rather than an error.
			return types.ContainerJSON{}, false, nil
		}
		return types.ContainerJSON{}, false, fmt.Errorf("failed to inspect %q: %w", list[0].ID, err)
	}
	return inspected, true, nil
}

// removeContainer stops (best-effort) then force-removes a container along
// with its anonymous volumes, following the ADR-004 delete ordering (stop,
// then remove). A NotFound at any step is tolerated so teardown is fully
// idempotent; any other removal error is returned.
func (p *Provider) removeContainer(ctx context.Context, id string) error {
	// Best-effort graceful stop first (ADR-004 ordering). The forced remove
	// below reaps a still-running container regardless, so a stop error —
	// including NotFound — is not fatal here.
	_ = p.cli.ContainerStop(ctx, id, container.StopOptions{})
	if err := p.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// notFoundError builds the garm-provider-common not-found error, which
// execution.ResolveErrorToExitCode maps to exit code 30 (GARM treats a
// DeleteInstance 30 as success).
func notFoundError(instanceID string) error {
	return gErrors.NewNotFoundError("instance %q not found", instanceID)
}

// toProviderInstance maps a fully inspected container to a ProviderInstance.
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
		ProviderID: c.ID,
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
func toProviderInstanceFromSummary(c types.Container) params.ProviderInstance {
	name := c.Labels[spec.LabelInstanceName]
	if name == "" && len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	return params.ProviderInstance{
		ProviderID: c.ID,
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
