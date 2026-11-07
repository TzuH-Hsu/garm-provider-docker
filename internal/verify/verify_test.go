//go:build dockerverify

// Package verify is the live-daemon integration suite. It sits behind the
// `dockerverify` build tag, so `go test ./...` skips it: it needs a real Docker
// daemon, builds and pulls images, and needs outbound internet access from the
// Docker host, because it checks real egress to api.github.com instead of
// reading a network's Internal flag. Run it explicitly:
//
//	go test -tags dockerverify -v -run TestVerifyM1WP2Allocation ./internal/verify/
//
// The tests drive the real provider binary, built here, against a local fake
// metadata HTTPS server and a small sleep-entrypoint runner image. Each test
// scopes what it creates to a unique controller ID, tears it down at the end, and
// checks that containers it did not create are untouched.
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

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

const (
	instanceName  = "wp2-verify-01"
	poolID        = "wp2-verify-pool"
	instanceToken = "wp2-verify-instance-token"
	bearer        = "Bearer " + instanceToken
)

// jobNetworkName must match internal/spec's stable claim-marker network name.
func jobNetworkName() string { return instanceName + "-net" }

// volumeNameByResource returns the name of the one managed volume with the given
// controller-id and resource labels, and fails unless exactly one matches. Volume
// names embed the allocation's create nonce (<instance>-<nonce>-<kind>), which the
// test cannot know in advance.
func volumeNameByResource(t *testing.T, controllerID, resource string) string {
	t.Helper()
	out := dockerOut(t, "volume", "ls",
		"--filter", "label=garm.docker/controller-id="+controllerID,
		"--filter", "label=garm.docker/resource="+resource,
		"--format", "{{.Name}}")
	names := lines(out)
	if len(names) != 1 {
		t.Fatalf("want exactly one %q volume for controller %s, got %v", resource, controllerID, names)
	}
	return names[0]
}

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

// dockerOut runs `docker <args...>` and returns trimmed stdout, failing the test
// on error.
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

// hostOSArch maps the test process's GOARCH to the params.OSArch that GARM would
// put in a BootstrapInstance for this host. The suite runs the provider against
// the local daemon, so a fixed arch would test a platform the daemon is not
// running. Any GOARCH other than amd64 and arm64 panics: falling back to amd64
// would report a platform as verified when it was not.
func hostOSArch() params.OSArch {
	switch runtime.GOARCH {
	case "amd64":
		return params.Amd64
	case "arm64":
		return params.Arm64
	default:
		panic("verify harness: unsupported host GOARCH " + runtime.GOARCH +
			"; only linux/amd64 and linux/arm64 are supported platforms")
	}
}

func bootstrap(metadataURL string, caBundle []byte) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name:             instanceName,
		RepoURL:          "https://github.com/example-org/example-repo",
		MetadataURL:      metadataURL,
		InstanceToken:    instanceToken,
		CACertBundle:     caBundle,
		OSType:           params.Linux,
		OSArch:           hostOSArch(),
		PoolID:           poolID,
		JitConfigEnabled: true,
	}
}

// runProvider runs the provider binary with the GARM execution environment and,
// for CreateInstance, a bootstrap payload on stdin. It returns stdout and the
// exit code: 31 for a duplicate instance and 30 for a missing one, as mapped by
// execution.ResolveErrorToExitCode.
func runProvider(t *testing.T, bin, configFile, controllerID, command, instanceID string, b *params.BootstrapInstance) (string, int) {
	stdout, _, code := runProviderIO(t, bin, configFile, controllerID, command, instanceID, b)
	return stdout, code
}

// runProviderIO is runProvider plus the provider's stderr, where its log lines go.
func runProviderIO(t *testing.T, bin, configFile, controllerID, command, instanceID string, b *params.BootstrapInstance) (string, string, int) {
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
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), exitCode
}

// TestVerifyM1WP2Allocation runs the full create, duplicate-create, delete,
// repeat-delete and failed-create sequence against a real daemon. It proves the
// job network, workspace volume, runner security properties, exit codes and
// zero-orphan teardown, and that unrelated containers stay untouched. Only a real
// daemon can show the actual network, mount and sysctl state, and real egress.
func TestVerifyM1WP2Allocation(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)
	canaries := ensureForeignCanaries(t)

	foreignBefore := dockerOut(t, "ps", "-a", "--format", "{{.Names}}")
	t.Logf("[snapshot] containers before:\n%s", foreignBefore)
	assertForeignPresent(t, canaries[0])
	assertForeignPresent(t, canaries[1])

	imageTag := "garm-wp2-verify-sleep:latest"
	buildSleepImage(t, imageTag)

	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	configDir := t.TempDir()
	configFile := filepath.Join(configDir, "config.toml")
	// [network] is omitted so the run uses config.Load's defaults:
	// enable_job_network=true, internal=false (ADR-001). The internal=true opt-in
	// is covered by TestVerifyOwnerRulingEgressAndIsolation.
	// The cache is off because repo-scoped cache volumes survive DeleteInstance and
	// would break the zero-managed-resources assertions below.
	// TestVerifyM2W1CacheHit covers the cache.
	writeFile(t, configFile, fmt.Sprintf(`docker_host = "unix:///var/run/docker.sock"
runner_image = %q
allow_unpinned_runner_image = true

[cache]
enabled = false
`, imageTag))

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, canaries[0])
		assertForeignPresent(t, canaries[1])
	}()

	// CreateInstance
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

	// (f) With no [network] block the job network is internal=false, so it has a
	// route to the external network.
	if netInternal != "false" {
		t.Errorf("(f) job network Internal=%s, want false (2026-07-21 owner ruling default)", netInternal)
	}
	// A runner needs real egress to register with GitHub, and DinD needs it for
	// registry pulls, so check reachability, not just the flag. The wget timeout
	// keeps it from hanging.
	egressOut, egressErr := dockerTry("exec", containerID, "sh", "-c", "wget -qO- -T 5 https://api.github.com/zen")
	if egressErr != nil {
		t.Errorf("(f) runner could NOT reach the external network on an internal=false job network: %v\n%s", egressErr, egressOut)
	} else {
		t.Logf("[f] outbound egress works on the default internal=false network; api.github.com/zen replied: %q", strings.TrimSpace(egressOut))
	}

	// (b) The workspace volume exists, is labeled, and is mounted at the runner
	// workdir. Its name embeds the create nonce, so look it up by label.
	wsVol := volumeNameByResource(t, controllerID, "workspace")
	volLabels := dockerOut(t, "volume", "inspect", wsVol, "-f",
		"managed={{index .Labels \"garm.docker/managed\"}} resource={{index .Labels \"garm.docker/resource\"}} instance={{index .Labels \"garm.docker/instance-name\"}}")
	t.Logf("[b] workspace volume %s labels: %s", wsVol, volLabels)
	mountTarget := dockerOut(t, "inspect", containerID, "-f",
		"{{range .Mounts}}{{if eq .Name \""+wsVol+"\"}}{{.Destination}}{{end}}{{end}}")
	t.Logf("[b] workspace mount destination: %s", mountTarget)
	if mountTarget != "/actions-runner/_work" {
		t.Errorf("(b) workspace volume mounted at %q, want /actions-runner/_work", mountTarget)
	}
	if !strings.Contains(volLabels, "resource=workspace") {
		t.Errorf("(b) workspace volume labels incorrect: %s", volLabels)
	}

	// ::1 must not be usable as `localhost` in the runner, so a server bound to
	// localhost lands on 127.0.0.1, which clients that skip ::1 can reach. Normally
	// the runner carries the IPv6 sysctl and lo has IPv6 disabled. On a host whose
	// kernel has no IPv6 the daemon cannot apply the sysctl, the provider recreates
	// the runner without it, and /proc/sys/net/ipv6 is absent: ::1 cannot exist, so
	// that is reported, not failed. If the daemon refused the sysctl for another
	// reason, the provider also retries without it and warns that IPv6 may stay
	// enabled; that case fails here. /etc/hosts is only logged: Engine 25 and
	// earlier keep a dead `::1 localhost` line even with the sysctl, and 26+ omits it.
	ipv6Sysctl := dockerOut(t, "inspect", containerID, "-f", "{{index .HostConfig.Sysctls \"net.ipv6.conf.all.disable_ipv6\"}}")
	loOut, loErr := dockerTry("exec", containerID, "sh", "-c", "if [ -d /proc/sys/net/ipv6 ]; then cat /proc/sys/net/ipv6/conf/lo/disable_ipv6; else echo absent; fi")
	loIPv6 := strings.TrimSpace(loOut)
	switch {
	case ipv6Sysctl == "1":
		// The normal case: the sysctl was applied.
	case loErr == nil && loIPv6 == "absent":
		t.Logf("[ipv6] WARN: runner created without net.ipv6.conf.all.disable_ipv6: the Docker host has no IPv6, so the provider retried without the sysctl")
	default:
		t.Errorf("(ipv6) runner HostConfig.Sysctls net.ipv6.conf.all.disable_ipv6 = %q although the host has IPv6, want 1: degraded, the daemon refused the sysctl and the provider retried without it (see its warning), or the spec no longer sets it", ipv6Sysctl)
	}
	switch {
	case loErr == nil && (loIPv6 == "1" || loIPv6 == "absent"):
		// ::1 is not usable: IPv6 is off on lo, or the kernel has none.
	case loErr == nil && loIPv6 == "0":
		t.Errorf("(ipv6) IPv6 is enabled on the runner's lo, so ::1 is usable as localhost and a server bound to localhost may be unreachable via 127.0.0.1 (degraded: the IPv6 sysctl was not applied)")
	default:
		t.Errorf("(ipv6) cannot read the runner's lo disable_ipv6 (%q, err %v); cannot tell whether ::1 is usable as localhost", loIPv6, loErr)
	}
	hostsFile, _ := dockerTry("exec", containerID, "cat", "/etc/hosts")
	t.Logf("[ipv6] sysctl=%q lo.disable_ipv6=%s; /etc/hosts (informational):\n%s", ipv6Sysctl, loIPv6, hostsFile)

	// The runner has no host docker.sock mount and no token or metadata URL in its env.
	mounts := dockerOut(t, "inspect", containerID, "-f", "{{json .Mounts}}")
	if strings.Contains(mounts, "/var/run/docker.sock") {
		t.Errorf("[security] runner has a host docker.sock mount: %s", mounts)
	}
	envDump := dockerOut(t, "inspect", containerID, "-f", "{{range .Config.Env}}{{println .}}{{end}}")
	if strings.Contains(envDump, instanceToken) || strings.Contains(envDump, srv.URL) {
		t.Errorf("[security] runner env leaks the instance token or metadata URL")
	}
	t.Logf("[security] no host socket mount; no token/metadata-url in env (M0 property preserved)")

	// The credential files landed in the tmpfs. Only their presence is checked,
	// so the contents never reach the log.
	credLs, _ := dockerTry("exec", containerID, "ls", "-la", "/run/garm")
	t.Logf("[creds] /run/garm listing:\n%s", credLs)
	for _, f := range []string{"runner", "credentials", "credentials_rsaparams", ".delivered"} {
		if _, err := dockerTry("exec", containerID, "test", "-e", "/run/garm/"+f); err != nil {
			t.Errorf("[creds] /run/garm/%s missing", f)
		}
	}

	// (c) A duplicate create exits 31 and leaves the first allocation intact.
	b2 := bootstrap(srv.URL, caBundle)
	dupOut, dupCode := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &b2)
	if dupCode != 31 {
		t.Errorf("(c) duplicate CreateInstance exit=%d, want 31; stdout=%s", dupCode, dupOut)
	} else {
		t.Logf("[c] duplicate CreateInstance correctly returned exit 31")
	}
	if _, err := dockerTry("inspect", containerID); err != nil {
		t.Errorf("(c) first runner container removed by the duplicate attempt: %v", err)
	}
	if _, err := dockerTry("network", "inspect", jobNetworkName()); err != nil {
		t.Errorf("(c) first job network removed by the duplicate attempt: %v", err)
	}

	// (d) DeleteInstance removes the container, network and volume; a repeat exits 30.
	_, delCode := runProvider(t, bin, configFile, controllerID, "DeleteInstance", instanceName, nil)
	if delCode != 0 {
		t.Errorf("(d) DeleteInstance exit=%d, want 0", delCode)
	}
	assertGone(t, "container", "inspect", containerID)
	assertGone(t, "network", "network", "inspect", jobNetworkName())
	assertGone(t, "volume", "volume", "inspect", wsVol)
	t.Logf("[d] DeleteInstance removed container + network + volume")
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("(d) %d managed resources remain for this controller after delete, want 0", n)
	}

	_, del2Code := runProvider(t, bin, configFile, controllerID, "DeleteInstance", instanceName, nil)
	if del2Code != 30 {
		t.Errorf("(d) repeat DeleteInstance exit=%d, want 30 (already gone)", del2Code)
	} else {
		t.Logf("[d] repeat DeleteInstance correctly returned exit 30")
	}

	// (e) A create whose metadata fetch fails leaves zero orphans.
	badBundle := bootstrap(srv.URL, caBundle)
	badBundle.InstanceToken = "wrong-token" // metadata server returns 401 → fetch fails
	guardOut, guardCode := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &badBundle)
	if guardCode == 0 {
		t.Errorf("(e) bad-metadata CreateInstance unexpectedly succeeded: %s", guardOut)
	} else {
		t.Logf("[e] bad-metadata CreateInstance failed as expected (exit=%d)", guardCode)
	}
	// The failed create must have rolled back the claim network and workspace
	// volume it made before the fetch.
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("(e) creation guard left %d orphaned resources, want 0", n)
	} else {
		t.Logf("[e] creation guard left zero orphaned resources of any kind")
	}
}

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

// foreignCanaryImage is already a dependency of the suite: buildSleepImage's
// Dockerfile starts from it.
const foreignCanaryImage = "alpine:3.20"

// foreignCanaryPrefix names every canary. Nothing is ever deleted by name or by
// this prefix; it only lets a canary left by an aborted run be found with
// `docker ps -a --filter name=garm-verify-foreign-canary-` and removed by hand.
const foreignCanaryPrefix = "garm-verify-foreign-canary-"

// ensureForeignCanaries creates two long-lived containers owned by the suite. They
// stand in for the unrelated containers that every test must prove it never
// touches, so the suite does not depend on what else runs on the host.
//
// The canaries carry no garm.docker/* labels, so no label-keyed filter used by the
// provider or by cleanupController can match them. Names are unique per call,
// because the suite does not promise that only one run uses a host at a time, and
// a fixed name could not tell this call's canary from another run's.
//
// t.Cleanup removes each canary by the container ID that `docker run` printed,
// never by name, so only a container this call created is deleted. The cleanup is
// registered before the first create, so a failure partway through still removes
// what exists. A failed removal is logged, not fatal.
//
// t.Cleanup runs after the test and all its defers, so the teardown defers that
// assert the canaries are present run before they are removed.
func ensureForeignCanaries(t *testing.T) []string {
	t.Helper()
	var suffixBytes [4]byte
	if _, err := rand.Read(suffixBytes[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	suffix := hex.EncodeToString(suffixBytes[:])
	names := []string{
		foreignCanaryPrefix + suffix + "-a",
		foreignCanaryPrefix + suffix + "-b",
	}

	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			if out, err := dockerTry("rm", "-f", id); err != nil {
				t.Logf("[foreign-canary] failed to remove canary (id=%s): %v\n%s\nremove it manually: docker rm -f %s", id, err, out, id)
			}
		}
	})

	for _, name := range names {
		id, err := runForeignCanary(name)
		if err != nil {
			t.Fatalf("create foreign canary %q: %v", name, err)
		}
		ids = append(ids, id)
	}
	return names
}

// runForeignCanary runs `docker run -d --name name <foreignCanaryImage> sleep
// infinity` and returns the container ID docker printed on success.
//
// It does not use dockerTry, which merges stdout and stderr: when the image is not
// cached, `docker run` writes a pull trace to stderr, and merging it would corrupt
// the ID that is the only thing printed to stdout. Only stdout is parsed as the ID.
func runForeignCanary(name string) (string, error) {
	cmd := exec.Command("docker", "run", "-d", "--name", name, foreignCanaryImage, "sleep", "infinity")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, stderr.String())
	}
	id := strings.TrimSpace(string(out))
	if len(id) != 64 {
		return "", fmt.Errorf("docker run -d printed %q on stdout, want a 64-character hex container ID (stderr: %s)", id, strings.TrimSpace(stderr.String()))
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", fmt.Errorf("docker run -d printed %q, which is not valid hex: %w", id, err)
	}
	return id, nil
}

func assertGone(t *testing.T, kind string, args ...string) {
	t.Helper()
	if _, err := dockerTry(args...); err == nil {
		t.Errorf("(d) %s still present after delete", kind)
	}
}

// controllerResourceCount counts the managed containers, networks and volumes
// labeled with controllerID, to assert nothing is left over.
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
	// The throttle stamps are never-started containers kept on purpose so the
	// sweep, the GC and the diagnostics prune run at most once per interval. They
	// are not allocation resources and are not counted.
	c := 0
	for _, role := range lines(dockerOut(t, "ps", "-a", "--filter", label, "--format", `{{.Label "garm.docker/role"}}`)) {
		if role != spec.RoleThrottleStamp && role != spec.RoleDiagPruneStamp {
			c++
		}
	}
	n := count(dockerOut(t, "network", "ls", "-q", "--filter", label))
	v := count(dockerOut(t, "volume", "ls", "-q", "--filter", label))
	return c + n + v
}

// cleanupController removes every resource labeled with controllerID and nothing
// else.
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
