package provider

import (
	"context"
	"fmt"
)

// Start is required by the ExternalProvider interface but is not currently
// invoked by GARM's controller (research.md §1.G). It currently returns an
// explicit "not implemented" error.
func (p *Provider) Start(_ context.Context, _ string) error {
	return fmt.Errorf("Start: not implemented")
}
