package provider

import (
	"context"
	"fmt"

	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// ListInstances returns every managed runner container scoped to this
// controller, further scoped to poolID (GARM_POOL_ID) when it is non-empty
// (ADR-004). The filter is label-only — the stateless, label-driven
// ownership model means no local state store is consulted.
func (p *Provider) ListInstances(ctx context.Context, poolID string) ([]params.ProviderInstance, error) {
	f := filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+p.controllerID),
	)
	if poolID != "" {
		f.Add("label", spec.LabelPoolID+"="+poolID)
	}
	// TODO(M1): also filter garm.docker/role=runner once DinD sidecars
	// exist, so a sidecar container is never reported to GARM as an
	// instance. In M0 "none" mode there are no sidecars, so this is a no-op.

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
