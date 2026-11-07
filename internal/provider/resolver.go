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

// resolve finds the owned runner container for a GARM_INSTANCE_ID, which is always the
// instance name. found=false with a nil error means it is gone. It looks only at the
// instance-name label, with no inspect-by-ID fallback (ADR-004, Identity), and a
// container that fails the ownership check counts as not found, so a mutating caller
// never gets a foreign one.
func (p *Provider) resolve(ctx context.Context, instanceID string) (types.ContainerJSON, bool, error) {
	return p.resolveByOwnedLabel(ctx, instanceID)
}

// resolveByOwnedLabel returns the first owned runner under the instance-name label,
// skipping a candidate that a concurrent delete has removed.
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

// ownsRunner reports whether c carries this controller's managed-runner labels.
func (p *Provider) ownsRunner(c types.ContainerJSON) bool {
	if c.Config == nil {
		return false
	}
	return spec.IsManagedRunner(c.Config.Labels, p.controllerID)
}

// hasManagedAllocationResources reports whether any managed job-scoped resource lingers
// under instanceName. DeleteInstance uses it when no runner resolves. The container
// check matters because resolve never returns the privileged sidecar, which could
// otherwise leak.
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

// notFoundError builds the not-found error, which maps to exit code 30.
func notFoundError(instanceID string) error {
	return gErrors.NewNotFoundError("instance %q not found", instanceID)
}

// toProviderInstance maps an inspected container to a ProviderInstance. ProviderID is
// the instance name, as CreateInstance returned it, so GARM never stores a container ID
// (ADR-004, Identity). ProviderFault comes only from c.State, never c.Config, so it
// cannot carry a credential.
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

// containerFaultMessage describes a container whose status mapped to InstanceError.
// state is non-nil.
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

// toProviderInstanceFromSummary maps a list summary to a ProviderInstance without
// another inspect. A summary has no OOM flag, so only GetInstance reports an OOM-killed
// container as an error. ProviderFault is the summary's own Status text.
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

// addressesFromInspect returns the container's per-network IPv4 addresses, reported to
// GARM as private.
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
