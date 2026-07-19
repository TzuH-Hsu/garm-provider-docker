package provider

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
)

// Start starts the runner container for instanceID (resolved by ID or name).
// GARM does not currently invoke Start (research.md §1.G); it is kept thin to
// satisfy the interface.
func (p *Provider) Start(ctx context.Context, instanceID string) error {
	c, found, err := p.resolve(ctx, instanceID)
	if err != nil {
		return err
	}
	if !found {
		return notFoundError(instanceID)
	}
	if err := p.cli.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start instance %q: %w", instanceID, err)
	}
	return nil
}
