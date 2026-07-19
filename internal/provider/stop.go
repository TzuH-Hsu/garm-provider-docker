package provider

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
)

// Stop stops the runner container for instanceID (resolved by ID or name).
// GARM does not currently invoke Stop (research.md §1.G); it is kept thin to
// satisfy the interface. When force is set, the container is stopped with a
// zero grace period (immediate SIGKILL).
func (p *Provider) Stop(ctx context.Context, instanceID string, force bool) error {
	c, found, err := p.resolve(ctx, instanceID)
	if err != nil {
		return err
	}
	if !found {
		return notFoundError(instanceID)
	}

	opts := container.StopOptions{}
	if force {
		zero := 0
		opts.Timeout = &zero
	}
	if err := p.cli.ContainerStop(ctx, c.ID, opts); err != nil {
		return fmt.Errorf("failed to stop instance %q: %w", instanceID, err)
	}
	return nil
}
