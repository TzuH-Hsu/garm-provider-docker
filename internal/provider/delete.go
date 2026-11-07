package provider

import (
	"context"
	"fmt"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// DeleteInstance tears down the whole allocation of instanceID, the instance name, even
// when the runner is already gone (ADR-004, Teardown order). It returns the not-found
// error (exit 30, which GARM treats as success) when nothing is left.
//
// It also logs, best-effort, a DinD sidecar's block-device writes and image footprint
// (ADR-006, Rollout) and why a stopped runner ended (ADR-004, Recording why a runner
// died).
func (p *Provider) DeleteInstance(ctx context.Context, instanceID string) error {
	instanceName, found, err := p.resolveInstanceName(ctx, instanceID)
	if err != nil {
		return err
	}
	if !found {
		return notFoundError(instanceID)
	}

	torn, err := p.topo.TeardownAllocationReportingIO(ctx, instanceName)
	if err != nil {
		return fmt.Errorf("failed to delete instance %q: %w", instanceID, err)
	}
	if !torn {
		// A concurrent delete or sweep got there first.
		return notFoundError(instanceID)
	}
	return nil
}

// resolveInstanceName returns the instance-name label that scopes a teardown. With no
// owned runner, instanceID still counts if any managed resource lingers under that
// name. found=false means nothing managed exists for it.
func (p *Provider) resolveInstanceName(ctx context.Context, instanceID string) (string, bool, error) {
	c, found, err := p.resolve(ctx, instanceID)
	if err != nil {
		return "", false, err
	}
	if found {
		name := ""
		if c.Config != nil {
			name = c.Config.Labels[spec.LabelInstanceName]
		}
		if name == "" {
			// baseLabels always sets the label, but tearing down nothing would be the
			// worse outcome.
			name = instanceID
		}
		return name, true, nil
	}

	lingering, err := p.hasManagedAllocationResources(ctx, instanceID)
	if err != nil {
		return "", false, err
	}
	if lingering {
		return instanceID, true, nil
	}
	return "", false, nil
}
