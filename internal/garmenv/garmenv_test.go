package garmenv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	execcommon "github.com/cloudbase/garm-provider-common/execution/common"
	"github.com/cloudbase/garm-provider-common/params"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/provider"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// These tests replay the exact environment and stdin GARM v0.2.1 hands an
// external provider for a SCALE-SET instance under interface v0.1.1, as read
// from cloudbase/garm@v0.2.1:
//
//   - every command: GARM_COMMAND, GARM_CONTROLLER_ID,
//     GARM_PROVIDER_CONFIG_FILE, GARM_POOL_EXTRASPECS, GARM_INTERFACE_VERSION
//     (runner/providers/v0.1.1/external.go NewProvider + each method).
//   - CreateInstance: GARM_POOL_ID = the scale-set pseudo pool ID
//     "<scaleSet.Name>-<entity.ID>" (workers/provider/instance_manager.go
//     pseudoPoolID), and GARM_POOL_EXTRASPECS = base64(bootstrap extra_specs),
//     which after util.MaybeAddWrapperToExtraSpecs carries
//     runner_install_template even when the admin set no extra_specs.
//   - Delete/GetInstance: GARM_INSTANCE_ID set, GARM_POOL_ID EMPTY, and
//     GARM_POOL_EXTRASPECS = base64("null") — PoolInfo is the zero params.Pool,
//     and json.Marshal of its nil json.RawMessage is "null".
//   - ListInstances: GARM_POOL_ID = the pseudo pool ID (workers/scaleset/
//     scaleset.go pseudoPoolID), GARM_POOL_EXTRASPECS = base64("null").

const (
	testControllerID = "6c1d3f57-5d0a-4f7e-9d2c-0b8f1e2a9c41"
	testEntityID     = "0d3a8f0e-47b1-4c9f-8a51-2f6e5b7c9d10"
	testScaleSetName = "docker-gate-scaleset"
	testInstanceName = "garm-scalesetrun1"
	testInstanceTok  = "synthetic-instance-jwt"
	// nullExtraSpecs is base64("null"): what GARM v0.2.1 puts in
	// GARM_POOL_EXTRASPECS when PoolInfo is the zero params.Pool.
	nullExtraSpecs = "bnVsbA=="
)

// testPseudoPoolID is GARM v0.2.1's scale-set pseudo pool ID shape.
var testPseudoPoolID = testScaleSetName + "-" + testEntityID

// runnerInstallWrapper stands in for GARM's rendered userdata wrapper. GARM
// stores it as []byte, so it reaches extra_specs as a base64 JSON string.
var runnerInstallWrapper = base64.StdEncoding.EncodeToString([]byte(
	"#!/bin/bash\nset -e\ncurl -s -H \"Authorization: Bearer $TOKEN\" \"$METADATA_URL/install-script\" | bash\n"))

// setBaseEnv sets the variables GARM v0.2.1 sends with every v0.1.1 command
// and returns the provider config path it points at.
func setBaseEnv(t *testing.T, command execcommon.ExecutionCommand) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("runner_image = \"ghcr.io/example/runner@sha256:deadbeef\"\n"), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	t.Setenv("GARM_INTERFACE_VERSION", execcommon.Version011)
	t.Setenv("GARM_COMMAND", string(command))
	t.Setenv("GARM_CONTROLLER_ID", testControllerID)
	t.Setenv("GARM_PROVIDER_CONFIG_FILE", cfgPath)
	t.Setenv("GARM_POOL_ID", "")
	t.Setenv("GARM_INSTANCE_ID", "")
	t.Setenv("GARM_POOL_EXTRASPECS", nullExtraSpecs)
	return cfgPath
}

// setScaleSetDeleteOrGetEnv reproduces GARM v0.2.1's Delete/GetInstance env
// for a scale-set instance: instance ID set, pool ID empty.
func setScaleSetDeleteOrGetEnv(t *testing.T, command execcommon.ExecutionCommand) {
	t.Helper()
	setBaseEnv(t, command)
	t.Setenv("GARM_INSTANCE_ID", testInstanceName)
}

// setScaleSetListEnv reproduces GARM v0.2.1's ListInstances env for a scale set.
func setScaleSetListEnv(t *testing.T) {
	t.Helper()
	setBaseEnv(t, execcommon.ListInstancesCommand)
	t.Setenv("GARM_POOL_ID", testPseudoPoolID)
}

// scaleSetBootstrap is the stdin BootstrapInstance GARM v0.2.1 sends for a
// scale-set CreateInstance whose scale set has NO admin extra_specs and a
// linked install template (the real-job gate's shape).
func scaleSetBootstrap(metadataURL string) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name:              testInstanceName,
		RepoURL:           "https://github.com/example-org/example-repo",
		MetadataURL:       metadataURL,
		CallbackURL:       metadataURL + "/callbacks",
		InstanceToken:     testInstanceTok,
		OSArch:            params.Amd64,
		OSType:            params.Linux,
		Flavor:            "default",
		Image:             "unused-by-this-provider",
		ExtraSpecs:        json.RawMessage(`{"runner_install_template":"` + runnerInstallWrapper + `"}`),
		PoolID:            testPseudoPoolID,
		GitHubRunnerGroup: "Default",
		JitConfigEnabled:  true,
	}
}

// setScaleSetCreateEnv reproduces GARM v0.2.1's CreateInstance env + stdin.
func setScaleSetCreateEnv(t *testing.T, bootstrap params.BootstrapInstance) {
	t.Helper()
	setBaseEnv(t, execcommon.CreateInstanceCommand)
	t.Setenv("GARM_POOL_ID", bootstrap.PoolID)
	t.Setenv("GARM_POOL_EXTRASPECS", base64.StdEncoding.EncodeToString(bootstrap.ExtraSpecs))
	payload, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatalf("marshal bootstrap: %v", err)
	}
	withStdin(t, payload)
}

// withStdin replaces os.Stdin with a pipe pre-filled with data for the test.
func withStdin(t *testing.T, data []byte) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("writing stdin pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing stdin pipe writer: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})
}

// garmAPI stands in for the two GARM v0.2.1 endpoints a provider touches with
// the instance token: the metadata service (the three JIT credential files)
// and the instance callback's status route. The status route models GARM's
// rules: the instance middleware accepts the token only while the runner is
// pending or installing (auth/instance_middleware.go), and the update is a
// checked transition (params.RunnerStatusTransitions, database/sql
// validateRunnerStatusTransition).
type garmAPI struct {
	*httptest.Server

	mu           sync.Mutex
	runnerStatus string
	reported     []string

	// onStatus, when set, runs after a status is accepted (the test's runner
	// "comes online" here).
	onStatus func(status string)
}

// garmRunnerStatusTransitions is GARM v0.2.1's params.RunnerStatusTransitions
// (params/params.go), restricted to the statuses this harness can reach.
var garmRunnerStatusTransitions = map[string][]string{
	"pending":    {"failed", "installing", "terminated", "pending"},
	"installing": {"failed", "idle", "terminated", "installing"},
	"idle":       {"offline", "active", "terminated", "idle"},
}

func (g *garmAPI) status() (string, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runnerStatus, append([]string(nil), g.reported...)
}

func newMetadataServer(t *testing.T) *garmAPI {
	t.Helper()
	files := map[string]string{
		"/credentials/runner":                "RUNNER-FILE",
		"/credentials/credentials":           "CREDENTIALS-FILE",
		"/credentials/credentials_rsaparams": "RSAPARAMS-FILE",
	}
	g := &garmAPI{runnerStatus: "pending"}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testInstanceTok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/callbacks/status" {
			g.handleStatus(t, w, r)
			return
		}
		body, ok := files[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *garmAPI) handleStatus(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var msg struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		AgentID *int64 `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if msg.AgentID != nil {
		t.Errorf("status update carries agent_id %d; GARM would reject a mismatch", *msg.AgentID)
	}
	g.mu.Lock()
	current := g.runnerStatus
	if current != "pending" && current != "installing" {
		g.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized) // the token is no longer accepted
		return
	}
	if !slices.Contains(garmRunnerStatusTransitions[current], msg.Status) {
		g.mu.Unlock()
		t.Errorf("invalid runner status transition from %s to %s", current, msg.Status)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	g.runnerStatus = msg.Status
	g.reported = append(g.reported, msg.Status)
	hook := g.onStatus
	g.mu.Unlock()
	if hook != nil {
		hook(msg.Status)
	}
	w.WriteHeader(http.StatusOK)
}

// runnerComesOnline makes the fake runner container print the actions/runner
// listening line once the provider has reported it installing, as the real
// runner does a few seconds after credential delivery.
func runnerComesOnline(g *garmAPI, fake *docker.FakeClient) {
	g.onStatus = func(status string) {
		if status == "installing" {
			fake.AppendLog(spec.RunnerContainerName(testInstanceName), docker.LogStdout, "2026-10-10 17:45:14Z: Listening for Jobs")
		}
	}
}

// assertRunnerIdle checks the provider walked GARM's runner status from
// pending through installing to idle — the only status from which GARM v0.2.1
// scale sets can move a runner to active when it picks up a job.
func assertRunnerIdle(t *testing.T, g *garmAPI) {
	t.Helper()
	final, reported := g.status()
	if final != "idle" || strings.Join(reported, ",") != "installing,idle" {
		t.Fatalf("runner status = %q after reports %v, want idle after [installing idle]", final, reported)
	}
}

// newFakeProvider builds a real *provider.Provider over the in-memory Docker
// fake, scoped to the same controller ID GARM sends.
func newFakeProvider(t *testing.T) (*provider.Provider, *docker.FakeClient) {
	t.Helper()
	cfg := config.Config{
		DockerHost:       "unix:///var/run/docker.sock",
		RunnerImage:      "ghcr.io/example/runner@sha256:deadbeef",
		DindMode:         config.DindModeNone,
		AllowedDindModes: []string{config.DindModeNone},
		Network:          config.Network{EnableJobNetwork: true},
	}
	fake := docker.NewFakeClient()
	p, err := provider.New(fake, cfg, testControllerID, provider.WithInProcessStatusWatcher())
	if err != nil {
		t.Fatalf("provider.New: %v", err)
	}
	return p, fake
}

// runCommand reads the environment the way main does and dispatches it.
func runCommand(t *testing.T, p *provider.Provider) (string, error) {
	t.Helper()
	env, err := GetEnvironment()
	if err != nil {
		return "", err
	}
	return env.Run(context.Background(), p)
}

// TestGarmV021ScaleSetLifecycleReplay drives one scale-set runner through
// Create -> List -> Get -> Delete -> List using exactly what GARM v0.2.1 sends.
// Before the fix, Create failed on the injected runner_install_template and
// Delete/Get failed environment validation with "missing pool ID".
func TestGarmV021ScaleSetLifecycleReplay(t *testing.T) {
	p, fake := newFakeProvider(t)
	md := newMetadataServer(t)
	runnerComesOnline(md, fake)

	setScaleSetCreateEnv(t, scaleSetBootstrap(md.URL))
	out, err := runCommand(t, p)
	if err != nil {
		t.Fatalf("CreateInstance (GARM v0.2.1 scale-set payload) = %v, want success", err)
	}
	assertRunnerIdle(t, md)
	var created params.ProviderInstance
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("decoding CreateInstance output %q: %v", out, err)
	}
	if created.Name != testInstanceName {
		t.Fatalf("created instance name = %q, want %q", created.Name, testInstanceName)
	}

	assertListed(t, p, 1)

	setScaleSetDeleteOrGetEnv(t, execcommon.GetInstanceCommand)
	out, err = runCommand(t, p)
	if err != nil {
		t.Fatalf("GetInstance (empty GARM_POOL_ID) = %v, want success", err)
	}
	var got params.ProviderInstance
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decoding GetInstance output %q: %v", out, err)
	}
	if got.Name != testInstanceName {
		t.Errorf("GetInstance name = %q, want %q", got.Name, testInstanceName)
	}

	setScaleSetDeleteOrGetEnv(t, execcommon.DeleteInstanceCommand)
	if _, err := runCommand(t, p); err != nil {
		t.Fatalf("DeleteInstance (empty GARM_POOL_ID) = %v, want success", err)
	}

	assertListed(t, p, 0)
}

// assertListed runs a GARM v0.2.1 scale-set ListInstances and checks the count.
func assertListed(t *testing.T, p *provider.Provider, want int) {
	t.Helper()
	setScaleSetListEnv(t)
	out, err := runCommand(t, p)
	if err != nil {
		t.Fatalf("ListInstances (pseudo pool ID) = %v, want success", err)
	}
	var list []params.ProviderInstance
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("decoding ListInstances output %q: %v", out, err)
	}
	if len(list) != want {
		t.Fatalf("ListInstances returned %d instances, want %d: %s", len(list), want, out)
	}
}

// TestGarmV021ScaleSetDeleteOfMissingInstanceIsNotFound: a scale-set delete
// for an instance that no longer exists reaches the provider (no env failure)
// and maps to GARM's idempotent not-found exit code.
func TestGarmV021ScaleSetDeleteOfMissingInstanceIsNotFound(t *testing.T) {
	p, _ := newFakeProvider(t)
	setScaleSetDeleteOrGetEnv(t, execcommon.DeleteInstanceCommand)
	_, err := runCommand(t, p)
	if err == nil {
		return // idempotent success is equally acceptable to GARM
	}
	if code := execcommon.ResolveErrorToExitCode(err); code != execcommon.ExitCodeNotFound {
		t.Fatalf("DeleteInstance of missing instance: exit code %d (%v), want %d", code, err, execcommon.ExitCodeNotFound)
	}
}

// TestGarmV021ScaleSetCreateRejectsAdminUnknownKey: the reserved-key allowance
// does not open the channel — an admin-supplied unknown key next to GARM's
// injected runner_install_template is still rejected (ADR-005 fail-closed).
func TestGarmV021ScaleSetCreateRejectsAdminUnknownKey(t *testing.T) {
	p, _ := newFakeProvider(t)
	md := newMetadataServer(t)
	b := scaleSetBootstrap(md.URL)
	b.ExtraSpecs = json.RawMessage(`{"privileged":true,"runner_install_template":"` + runnerInstallWrapper + `"}`)

	setScaleSetCreateEnv(t, b)
	_, err := runCommand(t, p)
	if err == nil {
		t.Fatal("CreateInstance with an unknown admin key = nil, want schema rejection")
	}
	if !strings.Contains(err.Error(), "privileged") {
		t.Errorf("error %q does not name the rejected key", err)
	}
}

// TestPoolIDRelaxationIsNarrow: the pool-ID relaxation covers exactly the four
// commands GARM v0.2.1 sends without one; every other library check stands.
func TestPoolIDRelaxationIsNarrow(t *testing.T) {
	for _, cmd := range poolIDOptionalCommands {
		t.Run(string(cmd)+"/no-pool-id-ok", func(t *testing.T) {
			setScaleSetDeleteOrGetEnv(t, cmd)
			env, err := GetEnvironment()
			if err != nil {
				t.Fatalf("GetEnvironment = %v, want success", err)
			}
			if env.EnvironmentV011.PoolID != "" {
				t.Errorf("PoolID = %q, want the empty value GARM sent (placeholder must not leak)", env.EnvironmentV011.PoolID)
			}
			if env.EnvironmentV011.InstanceID != testInstanceName {
				t.Errorf("InstanceID = %q, want %q", env.EnvironmentV011.InstanceID, testInstanceName)
			}
		})
		t.Run(string(cmd)+"/no-instance-id-rejected", func(t *testing.T) {
			setBaseEnv(t, cmd)
			if _, err := GetEnvironment(); err == nil || !strings.Contains(err.Error(), "missing instance ID") {
				t.Fatalf("GetEnvironment = %v, want missing instance ID", err)
			}
		})
	}

	t.Run("ListInstances/no-pool-id-rejected", func(t *testing.T) {
		setBaseEnv(t, execcommon.ListInstancesCommand)
		if _, err := GetEnvironment(); err == nil || !strings.Contains(err.Error(), "missing pool ID") {
			t.Fatalf("GetEnvironment = %v, want missing pool ID", err)
		}
	})

	t.Run("CreateInstance/no-pool-id-rejected", func(t *testing.T) {
		b := scaleSetBootstrap("https://metadata.invalid")
		setScaleSetCreateEnv(t, b)
		t.Setenv("GARM_POOL_ID", "")
		if _, err := GetEnvironment(); err == nil || !strings.Contains(err.Error(), "missing pool ID") {
			t.Fatalf("GetEnvironment = %v, want missing pool ID", err)
		}
	})

	t.Run("RemoveAllInstances/no-pool-id-ok", func(t *testing.T) {
		setBaseEnv(t, execcommon.RemoveAllInstancesCommand)
		if _, err := GetEnvironment(); err != nil {
			t.Fatalf("GetEnvironment = %v, want success", err)
		}
	})

	t.Run("missing-controller-id-rejected", func(t *testing.T) {
		setScaleSetDeleteOrGetEnv(t, execcommon.DeleteInstanceCommand)
		t.Setenv("GARM_CONTROLLER_ID", "")
		if _, err := GetEnvironment(); err == nil || !strings.Contains(err.Error(), "GARM_CONTROLLER_ID") {
			t.Fatalf("GetEnvironment = %v, want missing GARM_CONTROLLER_ID", err)
		}
	})

	t.Run("missing-config-file-rejected", func(t *testing.T) {
		setScaleSetDeleteOrGetEnv(t, execcommon.DeleteInstanceCommand)
		t.Setenv("GARM_PROVIDER_CONFIG_FILE", filepath.Join(t.TempDir(), "absent.toml"))
		if _, err := GetEnvironment(); err == nil || !strings.Contains(err.Error(), "config file") {
			t.Fatalf("GetEnvironment = %v, want config file access error", err)
		}
	})

	t.Run("unknown-command-rejected", func(t *testing.T) {
		setBaseEnv(t, execcommon.ExecutionCommand("NotACommand"))
		if _, err := GetEnvironment(); err == nil {
			t.Fatal("GetEnvironment = nil, want unknown command error")
		}
	})
}

// TestInterfaceV010IsDelegated: without GARM_INTERFACE_VERSION=v0.1.1 the
// library's own reader is used unchanged (v0.1.0 never needed a pool ID for
// DeleteInstance, so this succeeds there too).
func TestInterfaceV010IsDelegated(t *testing.T) {
	setScaleSetDeleteOrGetEnv(t, execcommon.DeleteInstanceCommand)
	t.Setenv("GARM_INTERFACE_VERSION", "")
	env, err := GetEnvironment()
	if err != nil {
		t.Fatalf("GetEnvironment (v0.1.0) = %v, want success", err)
	}
	if env.InterfaceVersion != "" || env.EnvironmentV010.InstanceID != testInstanceName {
		t.Errorf("env = %+v, want the library's v0.1.0 environment", env)
	}

	t.Setenv("GARM_INTERFACE_VERSION", "v9.9.9")
	if _, err := GetEnvironment(); err == nil {
		t.Fatal("GetEnvironment (unsupported version) = nil, want error")
	}
}

// unsetEnv removes key for the duration of the test (t.Setenv registers the
// restore; the variable is then genuinely absent, as when GARM never sets it).
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unsetting %s: %v", key, err)
	}
}

// setV010Env reproduces what GARM v0.2.1's v0.1.0 wrapper
// (runner/providers/v0.1.0/external.go) sends: GARM_INTERFACE_VERSION=v0.1.0
// (L54), GARM_COMMAND, GARM_CONTROLLER_ID, GARM_PROVIDER_CONFIG_FILE, and
// never GARM_POOL_EXTRASPECS. GARM_POOL_ID is sent only for CreateInstance
// (L78) and ListInstances (L205); Delete/Get/Start/Stop carry
// GARM_INSTANCE_ID and no pool ID at all (L127-135, L155-163).
func setV010Env(t *testing.T, command execcommon.ExecutionCommand, poolID, instanceID string) {
	t.Helper()
	setBaseEnv(t, command)
	t.Setenv("GARM_INTERFACE_VERSION", execcommon.Version010)
	unsetEnv(t, "GARM_POOL_EXTRASPECS")
	unsetEnv(t, "GARM_POOL_ID")
	unsetEnv(t, "GARM_INSTANCE_ID")
	if poolID != "" {
		t.Setenv("GARM_POOL_ID", poolID)
	}
	if instanceID != "" {
		t.Setenv("GARM_INSTANCE_ID", instanceID)
	}
}

// TestGarmV021ScaleSetLifecycleReplayV010 is the interface v0.1.0 twin of
// TestGarmV021ScaleSetLifecycleReplay — the recommended interface on GARM
// <= v0.2.1. The stdin payload still carries the injected
// runner_install_template (it is added before the interface wrapper runs);
// Get/Delete arrive with no GARM_POOL_ID variable at all.
func TestGarmV021ScaleSetLifecycleReplayV010(t *testing.T) {
	p, fake := newFakeProvider(t)
	md := newMetadataServer(t)
	runnerComesOnline(md, fake)
	b := scaleSetBootstrap(md.URL)

	setV010Env(t, execcommon.CreateInstanceCommand, b.PoolID, "")
	payload, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bootstrap: %v", err)
	}
	withStdin(t, payload)
	out, err := runCommand(t, p)
	if err != nil {
		t.Fatalf("CreateInstance (v0.1.0 scale-set payload) = %v, want success", err)
	}
	var created params.ProviderInstance
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.Name != testInstanceName {
		t.Fatalf("CreateInstance output %q (decode err %v), want instance %q", out, err, testInstanceName)
	}
	assertRunnerIdle(t, md)

	assertListedV010(t, p, 1)

	setV010Env(t, execcommon.GetInstanceCommand, "", testInstanceName)
	out, err = runCommand(t, p)
	if err != nil {
		t.Fatalf("GetInstance (v0.1.0, no GARM_POOL_ID) = %v, want success", err)
	}
	var got params.ProviderInstance
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Name != testInstanceName {
		t.Fatalf("GetInstance output %q (decode err %v), want instance %q", out, err, testInstanceName)
	}

	setV010Env(t, execcommon.DeleteInstanceCommand, "", testInstanceName)
	if _, err := runCommand(t, p); err != nil {
		t.Fatalf("DeleteInstance (v0.1.0, no GARM_POOL_ID) = %v, want success", err)
	}

	assertListedV010(t, p, 0)
}

// assertListedV010 runs a GARM v0.2.1 v0.1.0 scale-set ListInstances.
func assertListedV010(t *testing.T, p *provider.Provider, want int) {
	t.Helper()
	setV010Env(t, execcommon.ListInstancesCommand, testPseudoPoolID, "")
	out, err := runCommand(t, p)
	if err != nil {
		t.Fatalf("ListInstances (v0.1.0, pseudo pool ID) = %v, want success", err)
	}
	var list []params.ProviderInstance
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("decoding ListInstances output %q: %v", out, err)
	}
	if len(list) != want {
		t.Fatalf("ListInstances returned %d instances, want %d: %s", len(list), want, out)
	}
}
