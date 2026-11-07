// Package provider implements garm-provider-common's ExternalProvider
// interface for a single Docker host.
package provider

import (
	"fmt"

	executionv011 "github.com/cloudbase/garm-provider-common/execution/v0.1.1"
	"github.com/docker/docker/api/types/filters"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/topology"
)

// Provider implements executionv011.ExternalProvider against one Docker host. It holds
// no state across calls (ADR-004): every "what do I own" question is a Docker label
// query.
type Provider struct {
	cli          docker.Client
	cfg          config.Config
	controllerID string

	// topo owns the allocation network and volume lifecycle, teardown and orphan sweep.
	topo *topology.Manager

	// spawnStatusWatcher starts idle detection for a runner. It defaults to
	// startStatusWatcher; WithInProcessStatusWatcher replaces it.
	spawnStatusWatcher statusWatcherFunc
}

// New returns a Provider scoped to controllerID, the GARM controller that owns its
// managed resources. It rejects an empty or malformed allowed_dind_modes, so a Config
// that bypassed config.Load cannot silently permit every mode.
func New(cli docker.Client, cfg config.Config, controllerID string, opts ...Option) (*Provider, error) {
	if err := cfg.ValidateAllowedDindModes(); err != nil {
		return nil, fmt.Errorf("invalid provider config: %w", err)
	}
	p := &Provider{
		cli:          cli,
		cfg:          cfg,
		controllerID: controllerID,
		topo:         topology.New(cli, controllerID),
	}
	p.spawnStatusWatcher = p.startStatusWatcher
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// One binary serves both interface versions: v0.1.1 embeds the v0.1.0 methods, and
// GARM_INTERFACE_VERSION selects the dispatch.
var _ executionv011.ExternalProvider = (*Provider)(nil)

// managedByInstanceNameFilter selects this controller's job-scoped resources for an
// instance name, in any role, so hasManagedAllocationResources can see a lingering
// sidecar.
func (p *Provider) managedByInstanceNameFilter(instanceName string) filters.Args {
	return filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+p.controllerID),
		filters.Arg("label", spec.LabelInstanceName+"="+instanceName),
	)
}

// managedRunnerByInstanceNameFilter narrows managedByInstanceNameFilter to the runner:
// the sidecar shares the instance-name label, and a list could return it first.
func (p *Provider) managedRunnerByInstanceNameFilter(instanceName string) filters.Args {
	f := p.managedByInstanceNameFilter(instanceName)
	f.Add("label", spec.LabelRole+"="+spec.RoleRunner)
	return f
}
