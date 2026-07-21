package provider

import (
	"context"
	"fmt"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// DeleteInstance tears down the WHOLE per-allocation topology for instanceID —
// the runner container, the job network, and the job-scoped volumes — in the
// ADR-004 delete order (container(s) → network after endpoints detach →
// volumes). It is idempotent at every step (NotFound tolerated); an instance
// whose entire allocation is already gone returns the not-found error, which
// maps to exit code 30 (GARM treats that as success).
//
// instanceID may be a runner container ID (the ProviderID) or the GARM instance
// Name (research.md §1.E). resolveInstanceName maps either to the instance-name
// label that scopes the teardown; if the runner container is already gone but a
// network or volume lingers (a crashed/partial allocation), the label-filter
// fallback still finds and sweeps the leftovers (ADR-004).
func (p *Provider) DeleteInstance(ctx context.Context, instanceID string) error {
	instanceName, found, err := p.resolveInstanceName(ctx, instanceID)
	if err != nil {
		return err
	}
	if !found {
		// Nothing anywhere for this ID/name: the whole allocation is gone.
		return notFoundError(instanceID)
	}

	torn, err := p.topo.TeardownAllocation(ctx, instanceName)
	if err != nil {
		return fmt.Errorf("failed to delete instance %q: %w", instanceID, err)
	}
	if !torn {
		// Raced with a concurrent delete/sweep between resolution and teardown:
		// treat as already gone (exit 30) rather than an error.
		return notFoundError(instanceID)
	}
	return nil
}

// resolveInstanceName maps a GARM_INSTANCE_ID (a runner container ID or the
// instance Name) to the instance-name label that scopes an allocation's
// teardown (ADR-004). It first resolves the owned runner container (validating
// ownership, so a foreign container is never acted on); if none is found, it
// falls back to treating instanceID as an instance-name and checking for any
// lingering managed network/volume under that name — so a DeleteInstance for an
// allocation whose runner container already exited still reaps its network and
// volumes. found=false means nothing managed exists for this ID/name at all.
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
			// A managed runner missing its instance-name label should be
			// impossible (baseLabels always sets it), but fall back to the
			// GARM instance Name rather than tearing down nothing.
			name = instanceID
		}
		return name, true, nil
	}

	// No owned runner container. The instanceID may be a GARM instance Name
	// whose runner already exited, leaving a lingering network/volume — look
	// for any managed job-scoped resource under that name.
	lingering, err := p.hasManagedAllocationResources(ctx, instanceID)
	if err != nil {
		return "", false, err
	}
	if lingering {
		return instanceID, true, nil
	}
	return "", false, nil
}
