package extraspecs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
)

// testConfig is a hand-built config with a ceiling, a storage driver, memory
// ceilings, and one named flavor that overrides both the runner image and the
// runner-memory ceiling. Resolve does not require Validate(), so this is fine
// as an in-test fixture.
func testConfig() config.Config {
	return config.Config{
		RunnerImage:      "ghcr.io/example/runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DindMode:         config.DindModeNone,
		AllowedDindModes: []string{config.DindModeNone, config.DindModePrivilegedSidecar},
		StorageDriver:    "overlay2",
		Resources:        config.Resources{RunnerMemory: "8GiB", DindMemory: "4GiB"},
		Flavors: map[string]config.Flavor{
			"big": {
				RunnerImage:  "ghcr.io/example/runner-big@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				RunnerMemory: "16GiB",
			},
		},
	}
}

// --- Parse: schema accept/reject matrix -------------------------------------

func TestParseAcceptsEmptyAndNil(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("{}"), json.RawMessage("  ")} {
		got, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q) unexpected error: %v", string(raw), err)
		}
		if got.Flavor != "" || got.DindMode != "" || got.RunnerMemory != "" || got.DindMemory != "" ||
			got.StorageDriver != "" || got.RunnerLabels != nil || got.ExtraEnv != nil {
			t.Errorf("Parse(%q) = %+v, want zero ExtraSpecs", string(raw), got)
		}
	}
}

func TestParseAcceptsValidPayload(t *testing.T) {
	raw := json.RawMessage(`{
		"flavor": "big",
		"dind_mode": "privileged-sidecar",
		"runner_memory": "4GiB",
		"dind_memory": "2GiB",
		"storage_driver": "vfs",
		"runner_labels": ["self-hosted", "gpu"],
		"extra_env": {"MY_FLAG": "on", "TZ": "UTC"}
	}`)
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse valid payload: %v", err)
	}
	if got.Flavor != "big" || got.DindMode != "privileged-sidecar" || got.RunnerMemory != "4GiB" ||
		got.DindMemory != "2GiB" || got.StorageDriver != "vfs" {
		t.Errorf("Parse scalar fields wrong: %+v", got)
	}
	if len(got.RunnerLabels) != 2 || got.RunnerLabels[0] != "self-hosted" {
		t.Errorf("Parse runner_labels wrong: %v", got.RunnerLabels)
	}
	if got.ExtraEnv["MY_FLAG"] != "on" || got.ExtraEnv["TZ"] != "UTC" {
		t.Errorf("Parse extra_env wrong: %v", got.ExtraEnv)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"unknown property (raw image ref, structurally absent)", `{"image": "evil/image:latest"}`},
		{"unknown property docker_host", `{"docker_host": "tcp://evil:2375"}`},
		{"unknown property privileged", `{"privileged": true}`},
		{"dind_mode not in enum", `{"dind_mode": "rootless"}`},
		{"storage_driver not in enum", `{"storage_driver": "btrfs"}`},
		{"runner_memory bad shape", `{"runner_memory": "lots"}`},
		{"dind_memory bad shape", `{"dind_memory": "4 gigglebytes"}`},
		{"flavor wrong type", `{"flavor": 7}`},
		{"runner_labels wrong item type", `{"runner_labels": [1, 2]}`},
		{"extra_env value wrong type", `{"extra_env": {"K": 5}}`},
		{"malformed json", `{"flavor": "big"`},
		{"not an object", `["a","b"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(json.RawMessage(tc.raw)); err == nil {
				t.Errorf("Parse(%s) = nil error, want a fail-closed rejection", tc.raw)
			}
		})
	}
}

// --- Parse: reserved-env denylist (F8) --------------------------------------

func TestParseRejectsReservedEnvNames(t *testing.T) {
	reserved := []string{
		"RUNNER_EPHEMERAL",
		"RUNNER_TOOL_CACHE",
		"DOCKER_HOST",
		"docker_host", // case-insensitive: a lowercase spelling must not slip past
		"Docker_Sock_Gid",
		"JIT_CONFIG_ENABLED",
		"GITHUB_URL",
		"ACTIONS_RUNNER_INPUT_JITCONFIG",
	}
	for _, name := range reserved {
		t.Run(name, func(t *testing.T) {
			raw := json.RawMessage(`{"extra_env": {"` + name + `": "x"}}`)
			_, err := Parse(raw)
			if err == nil {
				t.Fatalf("Parse extra_env %q = nil error, want reject (F8)", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q should name the rejected variable %q", err.Error(), name)
			}
		})
	}
}

func TestParseAcceptsAllowlistedEnv(t *testing.T) {
	raw := json.RawMessage(`{"extra_env": {"MY_CI_FLAG": "true", "TZ": "UTC", "NODE_ENV": "test"}}`)
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse allowlisted extra_env: %v", err)
	}
	if len(got.ExtraEnv) != 3 {
		t.Errorf("want 3 extra_env entries, got %v", got.ExtraEnv)
	}
}

// --- Resolve: flavor --------------------------------------------------------

func TestResolveFlavorSelectsImageAndMemory(t *testing.T) {
	cfg := testConfig()
	specs, err := Parse(json.RawMessage(`{"flavor": "big"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	res, err := specs.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.RunnerImage != cfg.Flavors["big"].RunnerImage {
		t.Errorf("RunnerImage = %q, want the flavor's image %q", res.RunnerImage, cfg.Flavors["big"].RunnerImage)
	}
	// flavor "big" raises the runner ceiling to 16GiB; with no runner_memory
	// override the effective limit is that ceiling.
	if want := int64(16) * 1024 * 1024 * 1024; res.RunnerMemoryBytes != want {
		t.Errorf("RunnerMemoryBytes = %d, want the flavor ceiling %d", res.RunnerMemoryBytes, want)
	}
}

func TestResolveRejectsUnknownFlavor(t *testing.T) {
	cfg := testConfig()
	specs, err := Parse(json.RawMessage(`{"flavor": "nope"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := specs.Resolve(cfg); err == nil {
		t.Fatal("Resolve with an unknown flavor = nil error, want reject")
	}
}

func TestResolveDefaultImageWhenNoFlavor(t *testing.T) {
	cfg := testConfig()
	specs, _ := Parse(json.RawMessage(`{}`))
	res, err := specs.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.RunnerImage != cfg.RunnerImage {
		t.Errorf("RunnerImage = %q, want top-level %q", res.RunnerImage, cfg.RunnerImage)
	}
}

// --- Resolve: dind_mode ceiling ---------------------------------------------

func TestResolveDindModeWithinCeiling(t *testing.T) {
	cfg := testConfig() // allows none + privileged-sidecar
	specs, _ := Parse(json.RawMessage(`{"dind_mode": "privileged-sidecar"}`))
	res, err := specs.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve within ceiling: %v", err)
	}
	if res.DindMode != config.DindModePrivilegedSidecar {
		t.Errorf("DindMode = %q, want %q", res.DindMode, config.DindModePrivilegedSidecar)
	}
}

func TestResolveDindModeOutsideCeilingRejected(t *testing.T) {
	cfg := testConfig() // does NOT allow sysbox-runc
	specs, _ := Parse(json.RawMessage(`{"dind_mode": "sysbox-runc"}`))
	_, err := specs.Resolve(cfg)
	if err == nil {
		t.Fatal("Resolve dind_mode outside ceiling = nil error, want reject")
	}
	if !strings.Contains(err.Error(), "sysbox-runc") {
		t.Errorf("error %q should name the rejected mode", err.Error())
	}
}

func TestResolveDindModeEmptyFallsThroughToConfigDefault(t *testing.T) {
	cfg := testConfig() // default dind_mode none
	specs, _ := Parse(json.RawMessage(`{}`))
	res, err := specs.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.DindMode != config.DindModeNone {
		t.Errorf("DindMode = %q, want config default %q", res.DindMode, config.DindModeNone)
	}
}

// --- Resolve: memory ceiling (reject-not-clamp) -----------------------------

func TestResolveMemoryWithinAndAtCeiling(t *testing.T) {
	cfg := testConfig() // runner ceiling 8GiB, dind ceiling 4GiB
	specs, _ := Parse(json.RawMessage(`{"runner_memory": "8GiB", "dind_memory": "1GiB"}`))
	res, err := specs.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve at/within ceiling: %v", err)
	}
	if want := int64(8) * 1024 * 1024 * 1024; res.RunnerMemoryBytes != want {
		t.Errorf("RunnerMemoryBytes = %d, want %d (equal to ceiling is allowed)", res.RunnerMemoryBytes, want)
	}
	if want := int64(1) * 1024 * 1024 * 1024; res.DindMemoryBytes != want {
		t.Errorf("DindMemoryBytes = %d, want %d", res.DindMemoryBytes, want)
	}
}

func TestResolveRunnerMemoryOverCeilingRejected(t *testing.T) {
	cfg := testConfig() // runner ceiling 8GiB
	specs, _ := Parse(json.RawMessage(`{"runner_memory": "16GiB"}`))
	_, err := specs.Resolve(cfg)
	if err == nil {
		t.Fatal("Resolve runner_memory over ceiling = nil error, want reject (reject-not-clamp)")
	}
	if !strings.Contains(err.Error(), "runner_memory") || !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("error %q should explain the ceiling rejection", err.Error())
	}
}

func TestResolveDindMemoryOverCeilingRejected(t *testing.T) {
	cfg := testConfig() // dind ceiling 4GiB
	specs, _ := Parse(json.RawMessage(`{"dind_memory": "8GiB"}`))
	if _, err := specs.Resolve(cfg); err == nil {
		t.Fatal("Resolve dind_memory over ceiling = nil error, want reject")
	}
}

func TestResolveMemoryOverRaisedFlavorCeilingRejected(t *testing.T) {
	cfg := testConfig() // flavor "big" raises runner ceiling to 16GiB
	// 16GiB is within the flavor ceiling (accepted), 17GiB exceeds it (rejected).
	okSpecs, _ := Parse(json.RawMessage(`{"flavor": "big", "runner_memory": "16GiB"}`))
	if _, err := okSpecs.Resolve(cfg); err != nil {
		t.Errorf("16GiB within the flavor ceiling should be accepted, got: %v", err)
	}
	badSpecs, _ := Parse(json.RawMessage(`{"flavor": "big", "runner_memory": "17GiB"}`))
	if _, err := badSpecs.Resolve(cfg); err == nil {
		t.Error("17GiB over the flavor ceiling should be rejected")
	}
}

func TestResolveMemoryUnlimitedCeilingAcceptsAnyOverride(t *testing.T) {
	cfg := testConfig()
	cfg.Resources = config.Resources{} // no configured limit => unlimited ceiling (0)
	specs, _ := Parse(json.RawMessage(`{"runner_memory": "64GiB"}`))
	res, err := specs.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve against an unlimited ceiling should accept any override: %v", err)
	}
	if want := int64(64) * 1024 * 1024 * 1024; res.RunnerMemoryBytes != want {
		t.Errorf("RunnerMemoryBytes = %d, want %d", res.RunnerMemoryBytes, want)
	}
}

// --- Resolve: storage_driver + passthrough ----------------------------------

func TestResolveStorageDriverOverrideAndDefault(t *testing.T) {
	cfg := testConfig() // config default overlay2
	overSpecs, _ := Parse(json.RawMessage(`{"storage_driver": "vfs"}`))
	res, err := overSpecs.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.StorageDriver != "vfs" {
		t.Errorf("StorageDriver = %q, want the override vfs", res.StorageDriver)
	}
	defSpecs, _ := Parse(json.RawMessage(`{}`))
	res2, _ := defSpecs.Resolve(cfg)
	if res2.StorageDriver != "overlay2" {
		t.Errorf("StorageDriver = %q, want config default overlay2", res2.StorageDriver)
	}
}

func TestResolvePassesThroughLabelsAndEnv(t *testing.T) {
	cfg := testConfig()
	specs, _ := Parse(json.RawMessage(`{"runner_labels": ["gpu"], "extra_env": {"FOO": "bar"}}`))
	res, err := specs.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.ExtraRunnerLabels) != 1 || res.ExtraRunnerLabels[0] != "gpu" {
		t.Errorf("ExtraRunnerLabels = %v, want [gpu]", res.ExtraRunnerLabels)
	}
	if res.ExtraEnv["FOO"] != "bar" {
		t.Errorf("ExtraEnv = %v, want FOO=bar", res.ExtraEnv)
	}
}

// --- published schema is valid, parseable JSON ------------------------------

func TestSchemaJSONIsValidParseableSchema(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(SchemaJSON()), &m); err != nil {
		t.Fatalf("SchemaJSON() is not valid JSON: %v", err)
	}
	if m["$schema"] != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("published schema is not draft-07: $schema=%v", m["$schema"])
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatal("published schema has no properties object")
	}
	for _, want := range []string{"flavor", "dind_mode", "runner_memory", "dind_memory", "storage_driver", "runner_labels", "extra_env"} {
		if _, ok := props[want]; !ok {
			t.Errorf("published schema is missing property %q", want)
		}
	}
	if m["additionalProperties"] != false {
		t.Errorf("published schema must set additionalProperties:false, got %v", m["additionalProperties"])
	}
}
