package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	execcommon "github.com/cloudbase/garm-provider-common/execution/common"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/extraspecs"
)

// The v0.1.1-only ExternalProvider methods (ADR-005). GARM v0.2.1 does not call them
// (docs/garm-contract.md), but they publish the config and extra_specs contracts.

// GetSupportedInterfaceVersions reports the GARM external-provider interface
// versions this binary implements: both v0.1.0 and v0.1.1.
func (p *Provider) GetSupportedInterfaceVersions(_ context.Context) []string {
	return []string{execcommon.Version010, execcommon.Version011}
}

// GetConfigJSONSchema returns the provider config's published JSON Schema.
func (p *Provider) GetConfigJSONSchema(_ context.Context) (string, error) {
	return config.JSONSchema(), nil
}

// GetExtraSpecsJSONSchema returns the draft-07 extra_specs schema CreateInstance
// validates every payload against.
func (p *Provider) GetExtraSpecsJSONSchema(_ context.Context) (string, error) {
	return extraspecs.SchemaJSON(), nil
}

// ValidatePoolInfo runs a pool's extra_specs through the same Parse and Resolve path as
// CreateInstance. A create can still reject what this accepts: a DinD mode with no
// dind_image configured is checked only in CreateInstance. The image and flavor
// arguments are ignored: an image is chosen by extra_specs.flavor, which Resolve
// validates (ADR-002). A non-empty providerConfig is loaded and used instead of p.cfg.
// extraSpecsStr is raw JSON or base64 JSON (decodePoolExtraSpecs).
func (p *Provider) ValidatePoolInfo(_ context.Context, _ /*image*/, _ /*flavor*/, providerConfig, extraSpecsStr string) error {
	cfg := p.cfg
	if providerConfig != "" {
		loaded, err := config.Load(providerConfig)
		if err != nil {
			return fmt.Errorf("failed to load provider config %q: %w", providerConfig, err)
		}
		cfg = loaded
	}

	raw, err := decodePoolExtraSpecs(extraSpecsStr)
	if err != nil {
		return fmt.Errorf("invalid extra_specs: %w", err)
	}
	specs, err := extraspecs.Parse(raw)
	if err != nil {
		return err
	}
	if _, err := specs.Resolve(cfg); err != nil {
		return err
	}
	return nil
}

// decodePoolExtraSpecs normalizes an extra_specs string to raw JSON. Empty becomes {},
// JSON is taken as is, and anything else must be base64 of JSON, the form of
// GARM_POOL_EXTRASPECS.
func decodePoolExtraSpecs(s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage("{}"), nil
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("extra_specs is neither valid JSON nor base64: %w", err)
	}
	if !json.Valid(decoded) {
		return nil, fmt.Errorf("extra_specs base64 payload is not valid JSON")
	}
	return json.RawMessage(decoded), nil
}
