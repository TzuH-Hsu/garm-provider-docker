// Package provider implements garm-provider-common's ExternalProvider
// interface for a single Docker host.
//
// Only GetVersion is functional as of M0 WP1. The remaining lifecycle
// methods (CreateInstance, DeleteInstance, GetInstance, ListInstances,
// RemoveAllInstances, Start, Stop) are filled in incrementally by later
// work packages; today they return an explicit "not implemented" error so
// the type still satisfies the v0.1.0 ExternalProvider interface that
// execution.Run compiles and dispatches against.
package provider

import (
	executionv010 "github.com/cloudbase/garm-provider-common/execution/v0.1.0"
)

// Provider implements executionv010.ExternalProvider.
type Provider struct{}

// New constructs a Provider.
func New() *Provider {
	return &Provider{}
}

// Compile-time assertion that *Provider satisfies the interface main.go
// wires into execution.Run. GARM only requires the v0.1.1 method surface
// (GetSupportedInterfaceVersions, ValidatePoolInfo, GetConfigJSONSchema,
// GetExtraSpecsJSONSchema) when GARM_INTERFACE_VERSION=v0.1.1 is set; that
// surface is deferred to WP8 per ADR-005.
var _ executionv010.ExternalProvider = (*Provider)(nil)
