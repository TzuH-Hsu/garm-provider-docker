package provider

import (
	"context"
	"fmt"
	"log"

	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// ListInstances returns every managed RUNNER container scoped to this
// controller, further scoped to poolID (GARM_POOL_ID) when it is non-empty
// (ADR-004). The filter is label-only — the stateless, label-driven ownership
// model means no local state store is consulted.
//
// It first runs the opportunistic orphan sweep (ADR-004): ListInstances is a
// call the provider is invoked for anyway, so it is one of the two hooks (the
// other being CreateInstance's pre-create sweep) where abandoned allocations
// are collected without a background daemon. The sweep is best-effort — a sweep
// failure must not fail the list GARM asked for.
func (p *Provider) ListInstances(ctx context.Context, poolID string) ([]params.ProviderInstance, error) {
	if err := p.topo.SweepOrphans(ctx); err != nil {
		log.Printf("garm-provider-docker: ListInstances: orphan sweep failed (continuing): %v", err)
	}

	f := filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+p.controllerID),
		// role=runner so a DinD sidecar (WP3) is never reported to GARM as an
		// instance; a runner is the only container that represents a GARM
		// instance. Its provider_id is the stable GARM instance NAME (F6,
		// ADR-004 amendment 2026-07-21), resolved by the instance-name label —
		// never the runner container's ID.
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
