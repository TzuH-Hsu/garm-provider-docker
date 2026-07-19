package provider

import (
	"context"
	"fmt"
)

// DeleteInstance stops and force-removes the runner container (and its
// anonymous volumes) for instanceID, resolved by ID or name (ADR-004). It
// is idempotent: an instance that is already gone returns the not-found
// error, which maps to exit code 30 — GARM treats that as success.
//
// M0 "none" mode has only the container and its anonymous volumes to remove;
// the job network, DinD sidecar, and named job-scoped volumes arrive in M1
// and extend this along the ADR-004 delete ordering.
func (p *Provider) DeleteInstance(ctx context.Context, instanceID string) error {
	c, found, err := p.resolve(ctx, instanceID)
	if err != nil {
		return err
	}
	if !found {
		return notFoundError(instanceID)
	}
	if err := p.removeContainer(ctx, c.ID); err != nil {
		return fmt.Errorf("failed to delete instance %q: %w", instanceID, err)
	}
	return nil
}
