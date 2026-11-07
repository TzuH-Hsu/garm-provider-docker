package provider

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
)

// Stop stops instanceID's runner container, with no grace period when force is set.
// GARM v0.2.1 does not call it.
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
