//go:build dockerverify

// Package verify holds the M1-WP2 real-daemon verification harness. It is
// gated behind the `dockerverify` build tag so it never runs in the normal
// `go test ./...` gate: it requires a live Docker daemon and builds/pulls
// images. Run it explicitly against a real daemon with:
//
//	go test -tags dockerverify -v -run TestVerifyM1WP2Allocation ./internal/verify/
//
// It drives the REAL provider binary (built here) through a full none-mode
// allocation, with a local fake metadata HTTPS server and a tiny sleep-
// entrypoint runner image, and asserts the ADR-001/ADR-004 topology on the live
// daemon. Every resource it creates is scoped to a unique controller-id and
// torn down at the end; it never touches foreign resources.
package verify

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cloudbase/garm-provider-common/params"
)

const (
	instanceName  = "wp2-verify-01"
	poolID        = "wp2-verify-pool"
	instanceToken = "wp2-verify-instance-token"
	bearer        = "Bearer " + instanceToken
)

// derived resource names must match internal/spec's name builders.
func jobNetworkName() string   { return instanceName + "-net" }
func workspaceVolName() string { return instanceName + "-workspace" }

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller for repo root")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	return root
}

func randControllerID(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "wp2-verify-" + hex.EncodeToString(b[:])
}

// dockerOut runs `docker <args...>` and returns trimmed stdout, failing on
// error. It is the read-mostly inspection channel that produces the evidence.
func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := dockerTry(args...)
	if err != nil {
		t.Fatalf("docker %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

func dockerTry(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// newMetadataServer serves the three JIT credential files over HTTPS, asserting
// the Bearer instance token on every request.
func newMetadataServer(t *testing.T) *httptest.Server {
	t.Helper()
	bodies := map[string]string{
		"/credentials/runner":                "VERIFY-RUNNER-FILE",
		"/credentials/credentials":           "VERIFY-CREDENTIALS-FILE",
		"/credentials/credentials_rsaparams": "VERIFY-RSAPARAMS-FILE",
	}
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != bearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

func caBundlePEM(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("metadata server has no certificate")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

func bootstrap(metadataURL string, caBundle []byte) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name:             instanceName,
		RepoURL:          "https://github.com/example-org/example-repo",
		MetadataURL:      metadataURL,
		InstanceToken:    instanceToken,
		CACertBundle:     caBundle,
		OSType:           params.Linux,
		OSArch:           params.Amd64,
		PoolID:           poolID,
		JitConfigEnabled: true,
	}
}

// runProvider execs the REAL provider binary with the GARM execution
// environment and (for CreateInstance) a stdin bootstrap payload, returning the
// stdout and the process exit code (which the provider maps from its error via
// execution.ResolveErrorToExitCode: duplicate=31, not-found=30).
func runProvider(t *testing.T, bin, configFile, controllerID, command, instanceID string, b *params.BootstrapInstance) (string, int) {
	t.Helper()
	cmd := exec.Command(bin)
	env := append(os.Environ(),
		"GARM_COMMAND="+command,
		"GARM_CONTROLLER_ID="+controllerID,
		"GARM_POOL_ID="+poolID,
		"GARM_PROVIDER_CONFIG_FILE="+configFile,
	)
	if instanceID != "" {
		env = append(env, "GARM_INSTANCE_ID="+instanceID)
	}
	cmd.Env = env
	if b != nil {
		payload, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal bootstrap: %v", err)
		}
		cmd.Stdin = bytes.NewReader(payload)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("exec provider (%s): %v", command, err)
		}
	}
	t.Logf("[cmd] GARM_COMMAND=%s GARM_INSTANCE_ID=%q -> exit=%d\n  stdout: %s\n  stderr: %s",
		command, instanceID, exitCode, strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()))
	return strings.TrimSpace(stdout.String()), exitCode
}

func TestVerifyM1WP2Allocation(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)

	// --- foreign-resource snapshot (must be untouched throughout) ------------
	foreignBefore := dockerOut(t, "ps", "-a", "--format", "{{.Names}}")
	t.Logf("[snapshot] containers before:\n%s", foreignBefore)
	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")

	// --- build the sleep runner image ----------------------------------------
	imageTag := "garm-wp2-verify-sleep:latest"
	buildSleepImage(t, imageTag)

	// --- build the REAL provider binary --------------------------------------
	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	// --- provider config (unpinned local sleep image) ------------------------
	configDir := t.TempDir()
	configFile := filepath.Join(configDir, "config.toml")
	// [network] is deliberately omitted so this run exercises config.Load's
	// actual defaults (ADR-001, amended 2026-07-21): enable_job_network=true,
	// internal=false. See TestVerifyOwnerRulingEgressAndIsolation for the
	// internal=true opt-in path and the cross-allocation isolation check.
	writeFile(t, configFile, fmt.Sprintf(`docker_host = "unix:///var/run/docker.sock"
runner_image = %q
allow_unpinned_runner_image = true
`, imageTag))

	// --- fake metadata HTTPS server ------------------------------------------
	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	// Cleanup: label-scoped teardown of everything for THIS controller-id, plus
	// a leftover assertion. Never touches foreign resources.
	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
	}()

	// =========================================================================
	// CreateInstance
	// =========================================================================
	b := bootstrap(srv.URL, caBundle)
	stdout, code := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &b)
	if code != 0 {
		t.Fatalf("CreateInstance exit=%d, want 0; stdout=%s", code, stdout)
	}
	var created params.ProviderInstance
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("parse CreateInstance stdout %q: %v", stdout, err)
	}
	if created.Status != params.InstanceRunning {
		t.Fatalf("CreateInstance status=%q, want running", created.Status)
	}
	containerID := created.ProviderID
	t.Logf("[create] provider_id=%s status=%s", containerID, created.Status)

	// (a) labeled job network exists and the runner is attached to it.
	netInternal := dockerOut(t, "network", "inspect", jobNetworkName(), "-f", "{{.Internal}}")
	netLabels := dockerOut(t, "network", "inspect", jobNetworkName(), "-f",
		"managed={{index .Labels \"garm.docker/managed\"}} instance={{index .Labels \"garm.docker/instance-name\"}} resource={{index .Labels \"garm.docker/resource\"}} nonce={{index .Labels \"garm.docker/create-nonce\"}}")
	t.Logf("[a] network %s Internal=%s labels: %s", jobNetworkName(), netInternal, netLabels)
	attached := dockerOut(t, "inspect", containerID, "-f", "{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}")
	t.Logf("[a] runner attached networks: %s", attached)
	if !strings.Contains(attached, jobNetworkName()) {
		t.Errorf("(a) runner not attached to %s (attached: %q)", jobNetworkName(), attached)
	}
	if !strings.Contains(netLabels, "resource=job-network") || !strings.Contains(netLabels, "instance="+instanceName) {
		t.Errorf("(a) job network labels incorrect: %s", netLabels)
	}

	// (f) network is internal:false (no [network] block => config.Load's
	// default, ADR-001 amended 2026-07-21) — the job network has a normal
	// route to the external network.
	if netInternal != "false" {
		t.Errorf("(f) job network Internal=%s, want false (2026-07-21 owner ruling default)", netInternal)
	}
	// Stronger proof: the runner must actually be able to reach the public
	// internet — this is the whole point of the default flip (a real runner
	// needs this to register with GitHub; DinD needs it for registry pulls).
	// Bounded so it cannot hang.
	egressOut, egressErr := dockerTry("exec", containerID, "sh", "-c", "wget -qO- -T 5 https://api.github.com/zen")
	if egressErr != nil {
		t.Errorf("(f) runner could NOT reach the external network on an internal=false job network: %v\n%s", egressErr, egressOut)
	} else {
		t.Logf("[f] outbound egress works on the default internal=false network; api.github.com/zen replied: %q", strings.TrimSpace(egressOut))
	}

	// (b) workspace volume exists, labeled, mounted at the runner workdir.
	volLabels := dockerOut(t, "volume", "inspect", workspaceVolName(), "-f",
		"managed={{index .Labels \"garm.docker/managed\"}} resource={{index .Labels \"garm.docker/resource\"}} instance={{index .Labels \"garm.docker/instance-name\"}}")
	t.Logf("[b] workspace volume %s labels: %s", workspaceVolName(), volLabels)
	mountTarget := dockerOut(t, "inspect", containerID, "-f",
		"{{range .Mounts}}{{if eq .Name \""+workspaceVolName()+"\"}}{{.Destination}}{{end}}{{end}}")
	t.Logf("[b] workspace mount destination: %s", mountTarget)
	if mountTarget != "/actions-runner/_work" {
		t.Errorf("(b) workspace volume mounted at %q, want /actions-runner/_work", mountTarget)
	}
	if !strings.Contains(volLabels, "resource=workspace") {
		t.Errorf("(b) workspace volume labels incorrect: %s", volLabels)
	}

	// M0 security: no host docker.sock mount, no token/metadata in env.
	mounts := dockerOut(t, "inspect", containerID, "-f", "{{json .Mounts}}")
	if strings.Contains(mounts, "/var/run/docker.sock") {
		t.Errorf("[security] runner has a host docker.sock mount: %s", mounts)
	}
	envDump := dockerOut(t, "inspect", containerID, "-f", "{{range .Config.Env}}{{println .}}{{end}}")
	if strings.Contains(envDump, instanceToken) || strings.Contains(envDump, srv.URL) {
		t.Errorf("[security] runner env leaks the instance token or metadata URL")
	}
	t.Logf("[security] no host socket mount; no token/metadata-url in env (M0 property preserved)")

	// credential delivery landed in the tmpfs (files present, contents not printed).
	credLs, _ := dockerTry("exec", containerID, "ls", "-la", "/run/garm")
	t.Logf("[creds] /run/garm listing:\n%s", credLs)
	for _, f := range []string{"runner", "credentials", "credentials_rsaparams", ".delivered"} {
		if _, err := dockerTry("exec", containerID, "test", "-e", "/run/garm/"+f); err != nil {
			t.Errorf("[creds] /run/garm/%s missing", f)
		}
	}

	// =========================================================================
	// (c) duplicate create → exit 31, first allocation intact
	// =========================================================================
	b2 := bootstrap(srv.URL, caBundle)
	dupOut, dupCode := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &b2)
	if dupCode != 31 {
		t.Errorf("(c) duplicate CreateInstance exit=%d, want 31; stdout=%s", dupCode, dupOut)
	} else {
		t.Logf("[c] duplicate CreateInstance correctly returned exit 31")
	}
	// The first allocation is intact.
	if _, err := dockerTry("inspect", containerID); err != nil {
		t.Errorf("(c) first runner container removed by the duplicate attempt: %v", err)
	}
	if _, err := dockerTry("network", "inspect", jobNetworkName()); err != nil {
		t.Errorf("(c) first job network removed by the duplicate attempt: %v", err)
	}

	// =========================================================================
	// (d) DeleteInstance removes container+network+volume; repeat → exit 30
	// =========================================================================
	_, delCode := runProvider(t, bin, configFile, controllerID, "DeleteInstance", instanceName, nil)
	if delCode != 0 {
		t.Errorf("(d) DeleteInstance exit=%d, want 0", delCode)
	}
	assertGone(t, "container", "inspect", containerID)
	assertGone(t, "network", "network", "inspect", jobNetworkName())
	assertGone(t, "volume", "volume", "inspect", workspaceVolName())
	t.Logf("[d] DeleteInstance removed container + network + volume")
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("(d) %d managed resources remain for this controller after delete, want 0", n)
	}

	// Idempotent repeat → exit 30.
	_, del2Code := runProvider(t, bin, configFile, controllerID, "DeleteInstance", instanceName, nil)
	if del2Code != 30 {
		t.Errorf("(d) repeat DeleteInstance exit=%d, want 30 (already gone)", del2Code)
	} else {
		t.Logf("[d] repeat DeleteInstance correctly returned exit 30")
	}

	// =========================================================================
	// (e) creation guard: a bad-metadata create leaves zero orphans
	// =========================================================================
	badBundle := bootstrap(srv.URL, caBundle)
	badBundle.InstanceToken = "wrong-token" // metadata server returns 401 → fetch fails
	guardOut, guardCode := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &badBundle)
	if guardCode == 0 {
		t.Errorf("(e) bad-metadata CreateInstance unexpectedly succeeded: %s", guardOut)
	} else {
		t.Logf("[e] bad-metadata CreateInstance failed as expected (exit=%d)", guardCode)
	}
	// The creation guard must have rolled back the claim network + workspace
	// volume it created before the failed fetch.
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("(e) creation guard left %d orphaned resources, want 0", n)
	} else {
		t.Logf("[e] creation guard left zero orphaned resources of any kind")
	}
}

// --- helpers -----------------------------------------------------------------

func buildSleepImage(t *testing.T, tag string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM alpine:3.20\nENTRYPOINT [\"sleep\", \"infinity\"]\n")
	cmd := exec.Command("docker", "build", "-t", tag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sleep image: %v\n%s", err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertForeignPresent(t *testing.T, name string) {
	t.Helper()
	if _, err := dockerTry("inspect", name); err != nil {
		t.Errorf("[foreign] protected container %q is missing/modified: %v", name, err)
	}
}

func assertGone(t *testing.T, kind string, args ...string) {
	t.Helper()
	if _, err := dockerTry(args...); err == nil {
		t.Errorf("(d) %s still present after delete", kind)
	}
}

// controllerResourceCount counts every managed container/network/volume for a
// controller-id, used to assert zero leftovers.
func controllerResourceCount(t *testing.T, controllerID string) int {
	t.Helper()
	label := "label=garm.docker/controller-id=" + controllerID
	count := func(out string) int {
		out = strings.TrimSpace(out)
		if out == "" {
			return 0
		}
		return len(strings.Split(out, "\n"))
	}
	c := count(dockerOut(t, "ps", "-aq", "--filter", label))
	n := count(dockerOut(t, "network", "ls", "-q", "--filter", label))
	v := count(dockerOut(t, "volume", "ls", "-q", "--filter", label))
	return c + n + v
}

// cleanupController removes every resource for a controller-id, label-scoped, so
// the harness never leaves anything behind and never touches foreign resources.
func cleanupController(t *testing.T, controllerID string) {
	t.Helper()
	label := "label=garm.docker/controller-id=" + controllerID
	for _, id := range lines(dockerOut(t, "ps", "-aq", "--filter", label)) {
		_, _ = dockerTry("rm", "-f", "-v", id)
	}
	for _, id := range lines(dockerOut(t, "network", "ls", "-q", "--filter", label)) {
		_, _ = dockerTry("network", "rm", id)
	}
	for _, id := range lines(dockerOut(t, "volume", "ls", "-q", "--filter", label)) {
		_, _ = dockerTry("volume", "rm", id)
	}
	time.Sleep(50 * time.Millisecond)
}

func lines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
