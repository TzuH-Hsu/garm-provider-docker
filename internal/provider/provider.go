// Package provider implements garm-provider-common's ExternalProvider
// interface for a single Docker host.
package provider

import (
	executionv010 "github.com/cloudbase/garm-provider-common/execution/v0.1.0"
	"github.com/docker/docker/api/types/filters"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// Provider implements executionv010.ExternalProvider against one Docker
// host. Its dependencies are injected (rather than constructed internally)
// so the lifecycle methods can be table-tested against docker.FakeClient.
//
// The one-shot subprocess model (ADR-004) means a Provider holds no
// cross-call state: controllerID and cfg come from the process's
// environment/config at construction, and every "what do I own" question is
// answered by a Docker label query at call time.
type Provider struct {
	cli          docker.Client
	cfg          config.Config
	controllerID string
}

// New constructs a Provider from its injected dependencies: the Docker
// client, the parsed provider config, and the GARM controller ID (from
// GARM_CONTROLLER_ID) that scopes every managed resource.
func New(cli docker.Client, cfg config.Config, controllerID string) *Provider {
	return &Provider{
		cli:          cli,
		cfg:          cfg,
		controllerID: controllerID,
	}
}

// Compile-time assertion that *Provider satisfies the interface main.go
// wires into execution.Run. GARM only requires the v0.1.1 method surface
// (GetSupportedInterfaceVersions, ValidatePoolInfo, GetConfigJSONSchema,
// GetExtraSpecsJSONSchema) when GARM_INTERFACE_VERSION=v0.1.1 is set; that
// surface is deferred per ADR-005, and current GARM leaves the interface
// version defaulting to v0.1.0.
var _ executionv010.ExternalProvider = (*Provider)(nil)

// managedByInstanceNameFilter builds the Docker label filter that selects
// this controller's managed runner container(s) for a given instance name.
// It is the shared basis for CreateInstance's duplicate detection and the
// ID-or-name resolver's label-filter fallback (ADR-004), so both key off
// the same label set.
func (p *Provider) managedByInstanceNameFilter(instanceName string) filters.Args {
	return filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+p.controllerID),
		filters.Arg("label", spec.LabelInstanceName+"="+instanceName),
	)
}
