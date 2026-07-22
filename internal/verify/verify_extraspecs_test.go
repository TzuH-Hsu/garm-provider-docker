//go:build dockerverify

// This file adds the M3-W1 real-daemon verification harness for extra_specs and
// the v0.1.1 self-description commands, gated behind the same `dockerverify`
// build tag as the WP2/WP3 harnesses. Run it explicitly against a real daemon:
//
//	go test -tags dockerverify -v -run TestVerifyM3W1ExtraSpecs ./internal/verify/
//
// It drives the REAL provider binary and proves, on the live daemon:
//
//	(a) an extra_specs payload selecting a named flavor + a dind_mode within the
//	    ceiling brings the runner up on that flavor's image with the selected
//	    DinD mode (a privileged sidecar + DOCKER_HOST + socket mount that the
//	    default none-mode config would NOT have produced);
//	(b) an extra_specs dind_mode OUTSIDE allowed_dind_modes fails closed before
//	    any Docker op (zero containers/networks/volumes);
//	(c) an extra_specs reserved override (extra_env RUNNER_EPHEMERAL, and a raw
//	    image) is rejected before any Docker op;
//	(d) GetExtraSpecsJSONSchema / GetConfigJSONSchema / GetSupportedInterfaceVersions
//	    print valid output under GARM_INTERFACE_VERSION=v0.1.1, and ValidatePoolInfo
//	    accepts a good extra_specs and rejects a ceiling-violating / reserved one.
//
// Everything is scoped to a unique controller-id and torn down at the end; the
// host container/network/volume NAME sets are snapshotted before/after and
// asserted identical, so no foreign resource is ever touched.
package verify

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudbase/garm-provider-common/params"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

const m3Instance = "m3-verify-01"

func m3Bootstrap(metadataURL string, caBundle []byte, extraSpecs string) params.BootstrapInstance {
	b := params.BootstrapInstance{
		Name:             m3Instance,
		RepoURL:          "https://github.com/example-org/example-repo",
		MetadataURL:      metadataURL,
		InstanceToken:    instanceToken,
		CACertBundle:     caBundle,
		OSType:           params.Linux,
		OSArch:           params.Amd64,
		PoolID:           poolID,
		JitConfigEnabled: true,
	}
	if extraSpecs != "" {
		b.ExtraSpecs = json.RawMessage(extraSpecs)
	}
	return b
}

// runProviderSelfDescribe execs the binary for a v0.1.1 self-description command
// (no stdin, no instance id) with GARM_INTERFACE_VERSION=v0.1.1 and, when set,
// GARM_POOL_EXTRASPECS. Returns stdout and the exit code.
func runProviderSelfDescribe(t *testing.T, bin, configFile, controllerID, command, extraSpecs string) (string, int) {
	t.Helper()
	env := append(os.Environ(),
		"GARM_INTERFACE_VERSION=v0.1.1",
		"GARM_COMMAND="+command,
		"GARM_CONTROLLER_ID="+controllerID,
		"GARM_POOL_ID="+poolID,
		"GARM_PROVIDER_CONFIG_FILE="+configFile,
	)
	if extraSpecs != "" {
		env = append(env, "GARM_POOL_EXTRASPECS="+extraSpecs)
	}
	cmd := exec.Command(bin)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("exec provider (%s): %v", command, err)
		}
	}
	return strings.TrimSpace(string(out)), code
}

func TestVerifyM3W1ExtraSpecs(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)

	// --- foreign snapshot (identical after) ----------------------------------
	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")
	beforeC := snapshotIDs(t, "ps", "-a", "--format", "{{.Names}}")
	beforeN := snapshotIDs(t, "network", "ls", "--format", "{{.Name}}")
	beforeV := snapshotIDs(t, "volume", "ls", "--format", "{{.Name}}")
	t.Logf("[snapshot] before: %d containers, %d networks, %d volumes", len(beforeC), len(beforeN), len(beforeV))

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
		assertSnapshotIdentical(t, "containers", beforeC, "ps", "-a", "--format", "{{.Names}}")
		assertSnapshotIdentical(t, "networks", beforeN, "network", "ls", "--format", "{{.Name}}")
		assertSnapshotIdentical(t, "volumes", beforeV, "volume", "ls", "--format", "{{.Name}}")
	}()

	// --- build two distinguishable cli-runner images + the provider binary ---
	baseTag := "garm-m3-verify-base:latest"
	flavorTag := "garm-m3-verify-flavor:latest"
	buildCliRunnerImage(t, baseTag)
	buildCliRunnerImage(t, flavorTag)

	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	// Config: default dind_mode none, ceiling allows none + privileged-sidecar,
	// a [flavors.big] that overrides the runner image. sysbox-runc is deliberately
	// NOT in the ceiling (scenario b). Cache off (keeps the resource assertions clean).
	configFile := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, configFile, `docker_host = "unix:///var/run/docker.sock"
runner_image = "`+baseTag+`"
allow_unpinned_runner_image = true
dind_mode = "none"
allowed_dind_modes = ["none", "privileged-sidecar"]
dind_image = "`+dindImageDigest+`"
storage_driver = "overlay2"

[cache]
enabled = false

[flavors.big]
runner_image = "`+flavorTag+`"
`)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	runnerName := spec.RunnerContainerName(m3Instance)
	dindName := spec.DindContainerName(m3Instance)

	// =========================================================================
	// (a) flavor + dind_mode within the ceiling -> flavor image + privileged sidecar
	// =========================================================================
	b := m3Bootstrap(srv.URL, caBundle, `{"flavor": "big", "dind_mode": "privileged-sidecar"}`)
	stdout, code := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &b)
	if code != 0 {
		t.Fatalf("(a) CreateInstance exit=%d, want 0; out=%s", code, stdout)
	}
	var created params.ProviderInstance
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("(a) parse CreateInstance stdout %q: %v", stdout, err)
	}
	if created.Status != params.InstanceRunning {
		t.Errorf("(a) status=%q, want running", created.Status)
	}

	// The flavor's image took effect (NOT the top-level base image).
	runnerImage := dockerOut(t, "inspect", runnerName, "-f", "{{.Config.Image}}")
	if runnerImage != flavorTag {
		t.Errorf("(a) runner image=%q, want the flavor image %q (extra_specs.flavor override)", runnerImage, flavorTag)
	} else {
		t.Logf("[a] runner runs the flavor image %q (flavor override took effect)", flavorTag)
	}

	// dind_mode=privileged-sidecar took effect: a privileged sidecar exists that
	// the default none-mode config would NOT have created.
	if _, err := dockerTry("inspect", dindName); err != nil {
		t.Errorf("(a) no DinD sidecar %q — the extra_specs.dind_mode override did not take effect: %v", dindName, err)
	} else {
		priv := dockerOut(t, "inspect", dindName, "-f", "{{.HostConfig.Privileged}}")
		if priv != "true" {
			t.Errorf("(a) sidecar Privileged=%q, want true", priv)
		}
		role := dockerOut(t, "inspect", dindName, "-f", "{{index .Config.Labels \"garm.docker/role\"}}")
		t.Logf("[a] privileged DinD sidecar present (Privileged=%s, role=%s) — dind_mode override took effect", priv, role)
	}

	// The runner is wired to the sidecar's daemon over the shared socket volume.
	env := dockerOut(t, "inspect", runnerName, "-f", "{{range .Config.Env}}{{println .}}{{end}}")
	if !strings.Contains(env, "DOCKER_HOST="+spec.DindDockerHost) {
		t.Errorf("(a) runner env missing DOCKER_HOST=%s: %s", spec.DindDockerHost, env)
	}
	socketDest := dockerOut(t, "inspect", runnerName, "-f",
		"{{range .Mounts}}{{if eq .Destination \""+spec.DindSocketDir+"\"}}yes{{end}}{{end}}")
	if socketDest != "yes" {
		t.Errorf("(a) runner has no shared socket mount at %s", spec.DindSocketDir)
	}
	// Security: still no host docker.sock mount, no token/metadata in env.
	mounts := dockerOut(t, "inspect", runnerName, "-f", "{{json .Mounts}}")
	if strings.Contains(mounts, "/var/run/docker.sock") {
		t.Errorf("[security] runner has a host docker.sock mount: %s", mounts)
	}
	if strings.Contains(env, instanceToken) || strings.Contains(env, srv.URL) {
		t.Errorf("[security] runner env leaks the instance token or metadata URL")
	}
	t.Logf("[a] runner DOCKER_HOST=%s + socket mount at %s; no host socket, no creds in env", spec.DindDockerHost, spec.DindSocketDir)

	// Tear (a) down so the resource assertions below start clean.
	if _, delCode := runProvider(t, bin, configFile, controllerID, "DeleteInstance", m3Instance, nil); delCode != 0 {
		t.Errorf("(a) DeleteInstance exit=%d, want 0", delCode)
	}
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("(a) %d managed resources remain after delete, want 0", n)
	}

	// =========================================================================
	// (b) dind_mode OUTSIDE the ceiling -> fail closed, zero resources
	// =========================================================================
	bad := m3Bootstrap(srv.URL, caBundle, `{"dind_mode": "sysbox-runc"}`) // not in allowed_dind_modes
	out, code := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &bad)
	if code == 0 {
		t.Errorf("(b) out-of-ceiling dind_mode unexpectedly succeeded: %s", out)
	} else {
		t.Logf("[b] out-of-ceiling extra_specs.dind_mode failed closed (exit=%d)", code)
	}
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("(b) ceiling violation left %d resources, want 0 (must fail before any Docker op)", n)
	}
	if _, err := dockerTry("inspect", spec.JobNetworkName(m3Instance)); err == nil {
		t.Errorf("(b) a job network was created despite the ceiling violation")
	}

	// =========================================================================
	// (c) reserved overrides -> rejected, zero resources
	// =========================================================================
	for _, es := range []string{
		`{"extra_env": {"RUNNER_EPHEMERAL": "false"}}`,
		`{"image": "attacker/evil:latest"}`,
	} {
		bc := m3Bootstrap(srv.URL, caBundle, es)
		out, code := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &bc)
		if code == 0 {
			t.Errorf("(c) reserved override %s unexpectedly succeeded: %s", es, out)
		} else {
			t.Logf("[c] reserved override %s rejected (exit=%d)", es, code)
		}
		if n := controllerResourceCount(t, controllerID); n != 0 {
			t.Errorf("(c) reserved override %s left %d resources, want 0", es, n)
		}
	}

	// =========================================================================
	// (d) v0.1.1 self-description under GARM_INTERFACE_VERSION=v0.1.1
	// =========================================================================
	schemaOut, schemaCode := runProviderSelfDescribe(t, bin, configFile, controllerID, "GetExtraSpecsJSONSchema", "")
	if schemaCode != 0 {
		t.Errorf("(d) GetExtraSpecsJSONSchema exit=%d, want 0: %s", schemaCode, schemaOut)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(schemaOut), &schema); err != nil {
		t.Errorf("(d) GetExtraSpecsJSONSchema did not print valid JSON: %v\n%s", err, schemaOut)
	} else if schema["$schema"] != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("(d) GetExtraSpecsJSONSchema is not draft-07: %v", schema["$schema"])
	} else {
		t.Logf("[d] GetExtraSpecsJSONSchema printed a valid draft-07 schema")
	}

	cfgSchemaOut, cfgSchemaCode := runProviderSelfDescribe(t, bin, configFile, controllerID, "GetConfigJSONSchema", "")
	if cfgSchemaCode != 0 || !json.Valid([]byte(cfgSchemaOut)) {
		t.Errorf("(d) GetConfigJSONSchema exit=%d valid=%v: %s", cfgSchemaCode, json.Valid([]byte(cfgSchemaOut)), cfgSchemaOut)
	}

	versionsOut, versionsCode := runProviderSelfDescribe(t, bin, configFile, controllerID, "GetSupportedInterfaceVersions", "")
	if versionsCode != 0 || !strings.Contains(versionsOut, "v0.1.1") {
		t.Errorf("(d) GetSupportedInterfaceVersions exit=%d out=%q, want it to contain v0.1.1", versionsCode, versionsOut)
	} else {
		t.Logf("[d] GetSupportedInterfaceVersions = %s", versionsOut)
	}

	// ValidatePoolInfo accepts a good extra_specs...
	_, okCode := runProviderSelfDescribe(t, bin, configFile, controllerID, "ValidatePoolInfo", `{"flavor": "big", "dind_mode": "privileged-sidecar"}`)
	if okCode != 0 {
		t.Errorf("(d) ValidatePoolInfo of a good extra_specs exit=%d, want 0", okCode)
	} else {
		t.Logf("[d] ValidatePoolInfo accepted a good extra_specs")
	}
	// ...rejects a ceiling-violating one...
	if _, c := runProviderSelfDescribe(t, bin, configFile, controllerID, "ValidatePoolInfo", `{"dind_mode": "sysbox-runc"}`); c == 0 {
		t.Errorf("(d) ValidatePoolInfo accepted a ceiling-violating dind_mode, want reject")
	} else {
		t.Logf("[d] ValidatePoolInfo rejected a ceiling-violating extra_specs (exit=%d)", c)
	}
	// ...and a reserved override.
	if _, c := runProviderSelfDescribe(t, bin, configFile, controllerID, "ValidatePoolInfo", `{"extra_env": {"DOCKER_HOST": "tcp://evil:2375"}}`); c == 0 {
		t.Errorf("(d) ValidatePoolInfo accepted a reserved extra_env override, want reject")
	} else {
		t.Logf("[d] ValidatePoolInfo rejected a reserved extra_env override (exit=%d)", c)
	}
}
