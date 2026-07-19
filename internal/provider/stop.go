package provider

import (
	"context"
	"fmt"
)

// Stop is required by the ExternalProvider interface but is not currently
// invoked by GARM's controller (research.md §1.G). It currently returns an
// explicit "not implemented" error.
func (p *Provider) Stop(_ context.Context, _ string, _ bool) error {
	return fmt.Errorf("Stop: not implemented")
}
