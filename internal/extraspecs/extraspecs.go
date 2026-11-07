// Package extraspecs validates a pool's extra_specs (Parse) and resolves them
// against the operator's ceilings, flavors and allowlist (Resolve). The trust
// model is in ADR-005 and the fields are in docs/config-reference.md.
package extraspecs

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/xeipuuv/gojsonschema"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
)

// schemaJSON is embedded so the schema Parse validates against and the one
// GetExtraSpecsJSONSchema returns are the same bytes.
//
//go:embed schema.json
var schemaJSON string

// compiledSchema is schemaJSON compiled at init. A malformed embedded schema is a
// build mistake, so it panics instead of failing on first use.
var compiledSchema = mustCompileSchema()

func mustCompileSchema() *gojsonschema.Schema {
	s, err := gojsonschema.NewSchema(gojsonschema.NewStringLoader(schemaJSON))
	if err != nil {
		panic(fmt.Sprintf("extraspecs: embedded schema.json is not a valid JSON Schema: %v", err))
	}
	return s
}

// SchemaJSON returns the published extra_specs JSON Schema, the same bytes Parse
// validates against.
func SchemaJSON() string { return schemaJSON }

// ExtraSpecs is the accepted extra_specs surface; every field is optional. There
// is deliberately no field for an image reference, docker_host, the privileged
// flag or a Docker label: the schema's additionalProperties:false keeps them out.
type ExtraSpecs struct {
	// Flavor names a [flavors.<name>] entry, the only way a pool varies the image
	// (ADR-002).
	Flavor string `json:"flavor,omitempty"`

	DindMode string `json:"dind_mode,omitempty"`

	RunnerMemory string `json:"runner_memory,omitempty"`
	DindMemory   string `json:"dind_memory,omitempty"`

	StorageDriver string `json:"storage_driver,omitempty"`

	// RunnerLabels are appended to the runner labels. They take effect without JIT
	// only.
	RunnerLabels []string `json:"runner_labels,omitempty"`

	// ExtraEnv holds extra runner-container variables. Provider-injected values win
	// any collision when the provider merges them.
	ExtraEnv map[string]string `json:"extra_env,omitempty"`
}

// envNameRE is the env-name charset, mirrored from the schema. Without it the key
// "JIT_CONFIG_ENABLED=false" reaches Docker as a variable named JIT_CONFIG_ENABLED,
// past the exact-name checks.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validEnvName(name string) bool { return envNameRE.MatchString(name) }

// reservedEnvPrefixes and reservedEnvExact are the names extra_env may never set,
// even if allowlisted; ADR-005 gives the reasons. They are matched in Go,
// case-insensitively, because RE2 cannot express that denylist in the schema.
var (
	reservedEnvPrefixes = []string{"RUNNER_", "DOCKER_", "ACTIONS_RUNNER_INPUT_", "GARM_", "LD_"}
	reservedEnvExact    = []string{
		"JIT_CONFIG_ENABLED", "GITHUB_URL", "RUN_AS_ROOT",
		"WAIT_FOR_DOCKER_SECONDS", "BASH_ENV", "ENV", "IFS", "PATH", "NODE_OPTIONS",
	}
)

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

// garmReservedKeys are extra_specs keys that GARM owns. Parse strips them before
// validation, so they are accepted and ignored (see "Keys GARM adds" in ADR-005).
var garmReservedKeys = []string{"runner_install_template"}

// stripGarmReservedKeys returns raw without the garmReservedKeys entries. A
// reserved key whose value is neither a string nor null is an error, since GARM
// encodes it as base64. Anything that is not a JSON object is returned unchanged.
func stripGarmReservedKeys(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return raw, nil
	}

	kept := make(map[string]json.RawMessage, len(fields))
	stripped := false
	for name, value := range fields {
		if !isGarmReservedKey(name) {
			kept[name] = value
			continue
		}
		var asString string
		if err := json.Unmarshal(value, &asString); err != nil {
			return nil, fmt.Errorf("extra_specs.%s is reserved for GARM and must be a JSON string (GARM encodes it as base64): %w", name, err)
		}
		stripped = true
	}
	if !stripped {
		return raw, nil
	}

	out, err := json.Marshal(kept)
	if err != nil {
		return nil, fmt.Errorf("failed to re-encode extra_specs after removing GARM-reserved keys: %w", err)
	}
	return out, nil
}

// isGarmReservedKey matches case-sensitively: a near-miss spelling stays an
// unknown key and is rejected.
func isGarmReservedKey(name string) bool {
	return slices.Contains(garmReservedKeys, name)
}

// Parse validates raw against the embedded schema, fail-closed, and decodes it. It
// also rejects malformed and hard-reserved extra_env names, but takes no config:
// the allowlist and ceilings are Resolve's. An empty payload is treated as {}.
func Parse(raw json.RawMessage) (ExtraSpecs, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}

	raw, err := stripGarmReservedKeys(raw)
	if err != nil {
		return ExtraSpecs{}, err
	}

	result, err := compiledSchema.Validate(gojsonschema.NewBytesLoader(raw))
	if err != nil {
		// A load error means the bytes are not valid JSON.
		return ExtraSpecs{}, fmt.Errorf("extra_specs is not valid JSON: %w", err)
	}
	if !result.Valid() {
		msgs := make([]string, 0, len(result.Errors()))
		for _, e := range result.Errors() {
			msgs = append(msgs, e.String())
		}
		return ExtraSpecs{}, fmt.Errorf("extra_specs failed schema validation: %s", strings.Join(msgs, "; "))
	}

	// A second line of defense behind additionalProperties:false: a schema edit
	// cannot let an unknown key through.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s ExtraSpecs
	if err := dec.Decode(&s); err != nil {
		return ExtraSpecs{}, fmt.Errorf("failed to decode extra_specs: %w", err)
	}

	for name := range s.ExtraEnv {
		// The charset check comes first so a malformed key is rejected on its shape.
		if !validEnvName(name) {
			return ExtraSpecs{}, fmt.Errorf(
				"extra_specs.extra_env key %q is not a valid environment variable name: names must match %s (no '=', whitespace, unicode, or leading digit) (H3)",
				name, envNameRE.String())
		}
		// Reserved names are rejected even if the operator allowlists them.
		if reservedEnvName(name) {
			return ExtraSpecs{}, fmt.Errorf(
				"extra_specs.extra_env may not set the hard-reserved variable %q: the provider hard-reserves %v and prefixes %v regardless of allowed_env (H2); provider-injected environment always wins",
				name, reservedEnvExact, reservedEnvPrefixes)
		}
	}

	return s, nil
}

// Resolved holds the values CreateInstance provisions with, already checked
// against the flavors, ceilings and allowlist.
type Resolved struct {
	Flavor string

	DindMode string

	RunnerImage string

	// RunnerMemoryBytes and DindMemoryBytes are the effective limits; 0 is unlimited.
	RunnerMemoryBytes int64
	DindMemoryBytes   int64

	StorageDriver string

	ExtraRunnerLabels []string

	// ExtraEnv is merged after the provider's own env, so provider values win.
	ExtraEnv map[string]string
}

// Resolve checks s against cfg and returns the values to provision with. A memory
// override above the effective ceiling is rejected, not clamped. Errors come back
// before any Docker operation, so a bad extra_specs leaves nothing allocated.
func (s ExtraSpecs) Resolve(cfg config.Config) (Resolved, error) {
	if s.Flavor != "" {
		if _, ok := cfg.Flavors[s.Flavor]; !ok {
			return Resolved{}, fmt.Errorf("extra_specs.flavor %q is not a configured flavor (define [flavors.%s] in the provider config)", s.Flavor, s.Flavor)
		}
	}

	// EffectiveDindMode is the single enforcement point for allowed_dind_modes (ADR-005).
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

	// The allowlist defaults to empty, so no extra env passes unless the operator
	// opts a name in. Names are sorted so the error is deterministic.
	if len(s.ExtraEnv) > 0 {
		names := make([]string, 0, len(s.ExtraEnv))
		for name := range s.ExtraEnv {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !cfg.ExtraSpecs.EnvAllowed(name) {
				return Resolved{}, fmt.Errorf(
					"extra_specs.extra_env key %q is not on the operator's allowlist ([extra_specs].allowed_env); extra env is fail-closed and rejected by default unless the operator opts the name in (H2)",
					name)
			}
		}
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

// boundMemory returns the ceiling when there is no override, else the override,
// rejected if it exceeds a finite ceiling (0 means no limit). A zero override is
// rejected too: Docker treats a 0 limit as unset, which would drop the ceiling.
func boundMemory(field, override string, ceilingBytes int64) (int64, error) {
	if override == "" {
		return ceilingBytes, nil
	}
	req, err := config.ParsePositiveByteSize(override)
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
