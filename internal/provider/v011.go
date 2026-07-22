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

// This file implements the four v0.1.1-only ExternalProvider methods (ADR-005):
// GetSupportedInterfaceVersions, ValidatePoolInfo, GetConfigJSONSchema, and
// GetExtraSpecsJSONSchema. Together with the eight v0.1.0 methods they make
// *Provider satisfy executionv011.ExternalProvider (see provider.go's assertion),
// so the same binary serves v0.1.1 when GARM_INTERFACE_VERSION=v0.1.1 and v0.1.0
// by default — the top-level garm-provider-common execution package type-asserts
// the provider against the versioned interface, and a provider that implements
// v0.1.1 satisfies the v0.1.0 assertion too (v0.1.1 embeds the common interface).
//
// GARM's controller does not invoke these commands today (research.md §1.A), but
// implementing them makes the provider self-documenting: a GARM admin's
// `garm-cli pool update --extra-specs` can call ValidatePoolInfo to fail early,
// and the two schema commands publish the config/extra_specs contracts, matching
// the LXD provider's published-schema practice.

// GetSupportedInterfaceVersions reports the GARM external-provider interface
// versions this binary implements: both v0.1.0 and v0.1.1.
func (p *Provider) GetSupportedInterfaceVersions(_ context.Context) []string {
	return []string{execcommon.Version010, execcommon.Version011}
}

// GetConfigJSONSchema returns the provider config's published JSON Schema.
func (p *Provider) GetConfigJSONSchema(_ context.Context) (string, error) {
	return config.JSONSchema(), nil
}

// GetExtraSpecsJSONSchema returns the extra_specs published JSON Schema (the
// go:embed'ed draft-07 schema CreateInstance validates every payload against).
func (p *Provider) GetExtraSpecsJSONSchema(_ context.Context) (string, error) {
	return extraspecs.SchemaJSON(), nil
}

// ValidatePoolInfo validates a pool's extra_specs against the schema AND the
// live config's ceiling/allowlist/flavor rules (the extra_env operator allowlist
// and hard-reserved set included), returning nil on success and a descriptive
// error otherwise — the same Parse+Resolve path CreateInstance runs,
// so what this accepts is exactly what a create would accept (and what it
// rejects, a create would reject before any Docker op).
//
// The image and flavor positional args are the pool's BootstrapInstance.Image /
// .Flavor. image is deliberately ignored: image selection is named-flavor-only
// (ADR-002), and the flavor a pool actually uses in this provider is
// extra_specs.flavor, which Resolve validates. providerConfig is the config file
// path GARM passes (the same GARM_PROVIDER_CONFIG_FILE already loaded into
// p.cfg); it is reloaded when set so validation runs against exactly that file,
// and its own load/validate errors surface here too. extraSpecsStr is the pool's
// extra_specs as a string (raw JSON, or base64 JSON as GARM's controller path
// encodes it — see decodePoolExtraSpecs).
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

// decodePoolExtraSpecs normalizes the extra_specs string ValidatePoolInfo
// receives into raw JSON. It accepts an empty string ({}), raw JSON directly
// (what a direct caller and this repo's tests pass), and base64-encoded JSON as
// a fallback (GARM_POOL_EXTRASPECS is base64 JSON per research.md §1.A). It fails
// closed if the value is neither valid JSON nor valid base64-of-JSON.
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
