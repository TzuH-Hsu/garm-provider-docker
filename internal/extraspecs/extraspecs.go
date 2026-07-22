// Package extraspecs parses, schema-validates, and resolves a pool's
// extra_specs — the GARM-admin trust tier (ADR-005). It embeds a published
// draft-07 JSON Schema (schema.json), validates every payload against it
// FAIL-CLOSED with github.com/xeipuuv/gojsonschema (the garm-provider-lxd
// pattern), enforces the reserved-environment denylist (F8) that the schema
// alone cannot express, and resolves the accepted overrides against the live
// provider config's EXISTING ceiling/flavor/memory machinery
// (config.EffectiveDindMode, config.EffectiveRunnerImage,
// config.Effective{Runner,Dind}MemoryBytes) rather than re-implementing any of
// those bounds here.
package extraspecs

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xeipuuv/gojsonschema"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
)

// schemaJSON is the published draft-07 JSON Schema for extra_specs, embedded so
// the validated contract and the schema GetExtraSpecsJSONSchema returns are the
// exact same bytes (ADR-005: documentation is generated from the schema, not
// maintained separately, to avoid drift).
//
//go:embed schema.json
var schemaJSON string

// compiledSchema is schemaJSON compiled once at package init. A malformed
// embedded schema is a developer error (the file ships in the binary), so it
// panics at init rather than deferring to first use.
var compiledSchema = mustCompileSchema()

func mustCompileSchema() *gojsonschema.Schema {
	s, err := gojsonschema.NewSchema(gojsonschema.NewStringLoader(schemaJSON))
	if err != nil {
		panic(fmt.Sprintf("extraspecs: embedded schema.json is not a valid JSON Schema: %v", err))
	}
	return s
}

// SchemaJSON returns the raw published extra_specs JSON Schema bytes, for the
// v0.1.1 GetExtraSpecsJSONSchema self-description command (ADR-005).
func SchemaJSON() string { return schemaJSON }

// ExtraSpecs is the accepted extra_specs surface (ADR-005's allowlist). Every
// field is optional; an empty payload ({}) resolves to pure config defaults, so
// a pool that sets no extra_specs behaves exactly as before extra_specs existed.
// There is deliberately NO field for a raw image reference, docker_host, the
// privileged flag, or a Docker label: those are structurally absent from the
// schema (additionalProperties:false), not merely rejected.
type ExtraSpecs struct {
	// Flavor selects a named [flavors.<name>] entry — the only image-varying
	// channel (ADR-002). Validated for existence in Resolve.
	Flavor string `json:"flavor,omitempty"`

	// DindMode is the requested DinD mode, bounded by allowed_dind_modes in
	// Resolve via the single ceiling enforcement point config.EffectiveDindMode.
	DindMode string `json:"dind_mode,omitempty"`

	// RunnerMemory/DindMemory are human byte-size overrides, bounded by the
	// effective configured ceilings in Resolve (reject-not-clamp).
	RunnerMemory string `json:"runner_memory,omitempty"`
	DindMemory   string `json:"dind_memory,omitempty"`

	// StorageDriver overrides the DinD sidecar's --storage-driver (overlay2|vfs,
	// enum-checked by the schema).
	StorageDriver string `json:"storage_driver,omitempty"`

	// RunnerLabels are extra GitHub Actions runner labels appended to the
	// runner's label set (effective in non-JIT mode only — see schema.json).
	RunnerLabels []string `json:"runner_labels,omitempty"`

	// ExtraEnv are extra runner-container environment variables. Reserved names
	// (reservedEnvName) are rejected in Parse (F8); provider-injected values
	// always win any remaining collision, resolved at merge time in the provider.
	ExtraEnv map[string]string `json:"extra_env,omitempty"`
}

// reservedEnvPrefixes and reservedEnvExact are the F8 reserved-name denylist
// (ADR-005): names extra_env may NEVER set, because the provider itself relies
// on them for the runner contract (ADR-002) or connectivity control. This is
// enforced in Go, not the schema, because JSON Schema's propertyNames/pattern
// cannot express a case-insensitive prefix denylist under RE2 (no negative
// lookahead). Matching is case-insensitive so a lower/mixed-case spelling
// (docker_host, Runner_Ephemeral) cannot slip a reserved name past the check.
var (
	reservedEnvPrefixes = []string{"RUNNER_", "DOCKER_", "ACTIONS_RUNNER_INPUT_"}
	reservedEnvExact    = []string{"JIT_CONFIG_ENABLED", "GITHUB_URL"}
)

// reservedEnvName reports whether name is on the F8 reserved denylist.
func reservedEnvName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	for _, p := range reservedEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	for _, e := range reservedEnvExact {
		if upper == e {
			return true
		}
	}
	return false
}

// Parse schema-validates raw (draft-07, additionalProperties:false) FAIL-CLOSED,
// decodes it into an ExtraSpecs, and enforces the reserved-env denylist (F8).
// It performs no config-dependent bounding — that is Resolve's job — so it is a
// pure structural gate usable wherever only the shape matters (e.g. the
// v0.1.1 ValidatePoolInfo self-check also calls Resolve for the ceilings).
//
// An empty/nil payload is treated as {} (garm-provider-common initializes a
// missing extra_specs to {} anyway), so a pool with no extra_specs parses to
// the zero ExtraSpecs.
func Parse(raw json.RawMessage) (ExtraSpecs, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}

	result, err := compiledSchema.Validate(gojsonschema.NewBytesLoader(raw))
	if err != nil {
		// A load error means the bytes are not even valid JSON — fail closed.
		return ExtraSpecs{}, fmt.Errorf("extra_specs is not valid JSON: %w", err)
	}
	if !result.Valid() {
		msgs := make([]string, 0, len(result.Errors()))
		for _, e := range result.Errors() {
			msgs = append(msgs, e.String())
		}
		return ExtraSpecs{}, fmt.Errorf("extra_specs failed schema validation: %s", strings.Join(msgs, "; "))
	}

	// DisallowUnknownFields is redundant with the schema's additionalProperties
	// :false, kept as a second, structural line of defense so a future schema
	// edit that relaxed additionalProperties could not silently let an unknown
	// key populate a field the Go struct does not model.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s ExtraSpecs
	if err := dec.Decode(&s); err != nil {
		return ExtraSpecs{}, fmt.Errorf("failed to decode extra_specs: %w", err)
	}

	for name := range s.ExtraEnv {
		if reservedEnvName(name) {
			return ExtraSpecs{}, fmt.Errorf(
				"extra_specs.extra_env may not set the reserved variable %q: the provider owns %v and prefixes %v (F8); provider-injected environment always wins",
				name, reservedEnvExact, reservedEnvPrefixes)
		}
	}

	return s, nil
}

// Resolved is the outcome of bounding an ExtraSpecs against the live provider
// config: the concrete values CreateInstance provisions with. Every field has
// already been ceiling/flavor/denylist checked, so the provider can consume it
// directly without re-validating.
type Resolved struct {
	// Flavor is the validated flavor name ("" = config default), threaded into
	// config.Effective* so the flavor's own image/memory overrides apply.
	Flavor string

	// DindMode is the effective DinD mode, already bounded by allowed_dind_modes.
	DindMode string

	// RunnerImage is the image to pull/run (flavor-resolved).
	RunnerImage string

	// RunnerMemoryBytes/DindMemoryBytes are the effective limits (0 = unlimited),
	// each the extra_specs override when within the ceiling, else the ceiling.
	RunnerMemoryBytes int64
	DindMemoryBytes   int64

	// StorageDriver is the effective DinD --storage-driver (extra_specs override
	// or config default).
	StorageDriver string

	// ExtraRunnerLabels are appended to the runner's label set (non-JIT only).
	ExtraRunnerLabels []string

	// ExtraEnv are the allowlisted extra environment variables (reserved names
	// already rejected in Parse). The provider merges these AFTER its own
	// injected env so provider-injected values win any remaining collision.
	ExtraEnv map[string]string
}

// Resolve bounds an ExtraSpecs against cfg, reusing the config package's
// EXISTING ceiling/flavor/memory machinery rather than re-deriving any bound:
//
//   - dind_mode  -> config.EffectiveDindMode(s.DindMode): the single, deliberate
//     enforcement point for the allowed_dind_modes ceiling (ADR-005). M3 threads
//     the pool's requested mode through here as the poolMode argument, exactly as
//     ADR-005 mandates, rather than adding a second parallel ceiling check.
//   - flavor     -> membership in cfg.Flavors, then config.EffectiveRunnerImage
//     and config.Effective{Runner,Dind}MemoryBytes(flavor) for image/ceiling.
//   - memory     -> the effective configured value is the CEILING; an override is
//     rejected if it exceeds it (reject-not-clamp, owner ruling resolving ADR-005's
//     open question), otherwise used.
//
// Every failure is a fail-closed error returned BEFORE the provider performs any
// Docker operation, so an over-broad or escalating extra_specs never leaves a
// partial allocation behind (it surfaces to GARM as provider_fault).
func (s ExtraSpecs) Resolve(cfg config.Config) (Resolved, error) {
	if s.Flavor != "" {
		if _, ok := cfg.Flavors[s.Flavor]; !ok {
			return Resolved{}, fmt.Errorf("extra_specs.flavor %q is not a configured flavor (define [flavors.%s] in the provider config)", s.Flavor, s.Flavor)
		}
	}

	dindMode, err := cfg.EffectiveDindMode(s.DindMode)
	if err != nil {
		return Resolved{}, fmt.Errorf("extra_specs.dind_mode rejected: %w", err)
	}

	runnerCeiling, err := cfg.EffectiveRunnerMemoryBytes(s.Flavor)
	if err != nil {
		return Resolved{}, fmt.Errorf("failed to resolve the runner_memory ceiling: %w", err)
	}
	runnerMemoryBytes, err := boundMemory("runner_memory", s.RunnerMemory, runnerCeiling)
	if err != nil {
		return Resolved{}, err
	}

	dindCeiling, err := cfg.EffectiveDindMemoryBytes(s.Flavor)
	if err != nil {
		return Resolved{}, fmt.Errorf("failed to resolve the dind_memory ceiling: %w", err)
	}
	dindMemoryBytes, err := boundMemory("dind_memory", s.DindMemory, dindCeiling)
	if err != nil {
		return Resolved{}, err
	}

	storageDriver := cfg.StorageDriver
	if s.StorageDriver != "" {
		storageDriver = s.StorageDriver
	}

	return Resolved{
		Flavor:            s.Flavor,
		DindMode:          dindMode,
		RunnerImage:       cfg.EffectiveRunnerImage(s.Flavor),
		RunnerMemoryBytes: runnerMemoryBytes,
		DindMemoryBytes:   dindMemoryBytes,
		StorageDriver:     storageDriver,
		ExtraRunnerLabels: s.RunnerLabels,
		ExtraEnv:          s.ExtraEnv,
	}, nil
}

// boundMemory returns the effective memory limit for a field: the ceiling when
// no override is given, else the parsed override — REJECTED if it exceeds a
// finite ceiling (reject-not-clamp). A ceiling of 0 means the operator set no
// limit (unlimited), so any finite override is within it and accepted.
func boundMemory(field, override string, ceilingBytes int64) (int64, error) {
	if override == "" {
		return ceilingBytes, nil
	}
	req, err := config.ParseByteSize(override)
	if err != nil {
		return 0, fmt.Errorf("extra_specs.%s %q: %w", field, override, err)
	}
	if ceilingBytes > 0 && req > ceilingBytes {
		return 0, fmt.Errorf(
			"extra_specs.%s %q (%d bytes) exceeds the configured ceiling of %d bytes; extra_specs may only request within the operator's limit (reject-not-clamp)",
			field, override, req, ceilingBytes)
	}
	return req, nil
}
