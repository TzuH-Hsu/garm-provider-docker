package provider

import (
	"context"
	"fmt"

	"github.com/cloudbase/garm-provider-common/params"
)

// ListInstances is implemented in a later work package (M0 WP8, per
// docs/plan.md). It currently returns an explicit "not implemented" error.
func (p *Provider) ListInstances(_ context.Context, _ string) ([]params.ProviderInstance, error) {
	return nil, fmt.Errorf("ListInstances: not implemented")
}
