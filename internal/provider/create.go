package provider

import (
	"context"
	"fmt"

	"github.com/cloudbase/garm-provider-common/params"
)

// CreateInstance is implemented in a later work package (M0 WP7, per
// docs/plan.md). It currently returns an explicit "not implemented" error.
func (p *Provider) CreateInstance(_ context.Context, _ params.BootstrapInstance) (params.ProviderInstance, error) {
	return params.ProviderInstance{}, fmt.Errorf("CreateInstance: not implemented")
}
