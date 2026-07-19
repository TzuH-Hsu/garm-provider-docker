package provider

import (
	"context"
	"log"

	"github.com/docker/docker/api/types/container"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// RemoveAllInstances is the manual rescue operation (ADR-004): it removes
// every job-scoped resource for this controller. It is label-scoped, never a
// global wipe of the Docker host, and best-effort — a failure on one
// container is logged and the sweep continues rather than failing fast.
//
// It uses the single authoritative ADR-004 predicate (managed=true AND
// controller-id AND has instance-name AND NOT cache=true), so cache and
// diagnostic volumes (ADR-003) are never touched. In M0 "none" mode the only
// job-scoped resources are runner containers and their anonymous volumes.
func (p *Provider) RemoveAllInstances(ctx context.Context) error {
	list, err := p.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: spec.MatchPredicateFilters(p.controllerID),
	})
	if err != nil {
		return err
	}

	for _, c := range list {
		// Defense-in-depth: re-assert the full predicate (including the
		// "NOT cache=true" conjunct Docker's filter API cannot express)
		// against the labels in hand before deleting anything.
		if !spec.MatchesPredicate(c.Labels, p.controllerID) {
			continue
		}
		if err := p.removeContainer(ctx, c.ID); err != nil {
			log.Printf("garm-provider-docker: RemoveAllInstances: failed to remove container %s: %v", c.ID, err)
		}
	}
	return nil
}
