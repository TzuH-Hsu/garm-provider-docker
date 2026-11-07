package provider

import (
	"context"
	"fmt"
)

// RemoveAllInstances is the manual rescue operation: it removes every job-scoped
// resource of this controller and never a cache volume (ADR-004). One stuck resource
// does not stop the rest, and any failure is returned once the teardown completes, so
// an operator can tell a clean run from one that left something behind.
func (p *Provider) RemoveAllInstances(ctx context.Context) error {
	if err := p.topo.TeardownAll(ctx); err != nil {
		return fmt.Errorf("garm-provider-docker: RemoveAllInstances: best-effort teardown left resources behind: %w", err)
	}
	return nil
}
