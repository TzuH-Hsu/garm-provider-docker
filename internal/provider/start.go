package provider

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
)

// Start starts instanceID's runner container. GARM v0.2.1 does not call it.
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
