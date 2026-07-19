package provider

import (
	"context"
	"fmt"
)

// RemoveAllInstances is implemented in a later work package (M1, per
// docs/plan.md and ADR-004). It currently returns an explicit "not
// implemented" error.
func (p *Provider) RemoveAllInstances(_ context.Context) error {
	return fmt.Errorf("RemoveAllInstances: not implemented")
}
