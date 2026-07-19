package provider

import (
	"context"
	"fmt"

	"github.com/cloudbase/garm-provider-common/params"
)

// GetInstance is implemented in a later work package (M0 WP8, per
// docs/plan.md). It currently returns an explicit "not implemented" error.
func (p *Provider) GetInstance(_ context.Context, _ string) (params.ProviderInstance, error) {
	return params.ProviderInstance{}, fmt.Errorf("GetInstance: not implemented")
}
