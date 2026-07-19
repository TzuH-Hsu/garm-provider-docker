package provider

import (
	"context"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/version"
)

// GetVersion returns the provider's build-time version string.
func (p *Provider) GetVersion(_ context.Context) string {
	return version.Version
}
