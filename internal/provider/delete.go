package provider

import (
	"context"
	"fmt"
)

// DeleteInstance is implemented in a later work package (M0 WP8, per
// docs/plan.md). It currently returns an explicit "not implemented" error.
func (p *Provider) DeleteInstance(_ context.Context, _ string) error {
	return fmt.Errorf("DeleteInstance: not implemented")
}
