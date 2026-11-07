package provider

import (
	"context"

	"github.com/cloudbase/garm-provider-common/params"
)

// GetInstance returns instanceID's ProviderInstance, with its status mapped from the
// container's Docker state, or the not-found error (exit 30).
func (p *Provider) GetInstance(ctx context.Context, instanceID string) (params.ProviderInstance, error) {
	c, found, err := p.resolve(ctx, instanceID)
	if err != nil {
		return params.ProviderInstance{}, err
	}
	if !found {
		return params.ProviderInstance{}, notFoundError(instanceID)
	}
	return toProviderInstance(c), nil
}
