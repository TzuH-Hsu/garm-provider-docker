package provider

import (
	"context"
	"fmt"

	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// ListInstances returns this controller's runner containers, scoped to poolID when it
// is non-empty. There is no background daemon, so it also runs the throttled orphan
// sweep and cache GC (ADR-004, Orphan sweep); neither can fail the list.
func (p *Provider) ListInstances(ctx context.Context, poolID string) ([]params.ProviderInstance, error) {
	p.sweepThrottled(ctx)

	p.runCacheGCThrottled(ctx)

	f := filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+p.controllerID),
		// Only a runner represents an instance to GARM, never a DinD sidecar.
		filters.Arg("label", spec.LabelRole+"="+spec.RoleRunner),
	)
	if poolID != "" {
		f.Add("label", spec.LabelPoolID+"="+poolID)
	}

	list, err := p.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	out := make([]params.ProviderInstance, 0, len(list))
	for _, c := range list {
		out = append(out, toProviderInstanceFromSummary(c))
	}
	return out, nil
}
