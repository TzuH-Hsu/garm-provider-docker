//go:build dockerverify

// Failure and recovery scenarios beyond the lifecycle test in verify_test.go:
// image-pull failure, a credential timeout, a mid-teardown kill, a crash-abandoned
// allocation, a provider restart, and foreign-resource non-interference. Each needs
// a real daemon, because it checks what the daemon still holds after a failure.
//
// Covered elsewhere:
//   - ID-or-name resolution: GARM_INSTANCE_ID is always the instance name, so every
//     test here uses the name lookup. Collisions with ID-shaped names are unit-tested
//     in internal/provider/lifecycle_test.go.
//   - Exit codes 30 and 31: TestVerifyAllocation (repeat delete, duplicate create).
//   - Concurrent creation: unit tests in internal/topology. No failure mode was found
//     that only a live daemon shows.
package verify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudbase/garm-provider-common/params"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

func buildProviderBinary(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}
	return bin
}

// noCacheConfig writes a config with the cache disabled. Cache volumes survive
// DeleteInstance and rollback (ADR-003) and would break the zero-leftover
// assertions.
func noCacheConfig(t *testing.T, imageTag string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, f, fmt.Sprintf(`docker_host = "unix:///var/run/docker.sock"
runner_image = %q
allow_unpinned_runner_image = true

[cache]
enabled = false
`, imageTag))
	return f
}

// teardownBarrierMarker is what the trap-term runner image prints on SIGTERM.
// DeleteInstance stops the runner before removing it, so seeing the marker proves
// teardown entered its first destructive operation. The image ignores SIGTERM, so
// the stop then blocks for the daemon's stop timeout (about 10s), a wide window
// with the provider provably mid-teardown.
const teardownBarrierMarker = "GARM-TEARDOWN-BARRIER-SIGTERM"

// buildTrapTermImage builds an alpine image whose PID 1 shell traps SIGTERM, prints
// teardownBarrierMarker and keeps running. busybox tar keeps credential delivery
// working during create.
func buildTrapTermImage(t *testing.T, tag string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Dockerfile"),
		"FROM alpine:3.20\n"+
			"ENTRYPOINT [\"sh\",\"-c\",\"trap 'echo "+teardownBarrierMarker+"' TERM; while true; do sleep 1; done\"]\n")
	cmd := exec.Command("docker", "build", "-t", tag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build trap-term image: %v\n%s", err, out)
	}
}

// runDeleteKillOnTeardownBarrier starts a DeleteInstance subprocess, waits for
// teardownBarrierMarker in the runner's logs, then SIGKILLs it, so the kill lands
// mid-teardown without relying on a fixed sleep. It fails the test unless SIGKILL
// caused the exit: a clean exit means the kill came too late. killedLive reports
// that the kill hit a running process and sawBarrier that the marker was seen;
// callers assert both.
func runDeleteKillOnTeardownBarrier(t *testing.T, bin, configFile, controllerID, instanceID, runnerName string) (killedLive, sawBarrier bool) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"GARM_COMMAND=DeleteInstance",
		"GARM_CONTROLLER_ID="+controllerID,
		"GARM_POOL_ID="+poolID,
		"GARM_PROVIDER_CONFIG_FILE="+configFile,
		"GARM_INSTANCE_ID="+instanceID,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start DeleteInstance subprocess: %v", err)
	}
	// Poll for the marker for less than the ~10s stop timeout, so the kill lands
	// while ContainerStop is still blocked.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if logs, err := dockerTry("logs", runnerName); err == nil && strings.Contains(logs, teardownBarrierMarker) {
			sawBarrier = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	killErr := cmd.Process.Kill()
	waitErr := cmd.Wait() // reap, and verify below that SIGKILL was the actual cause of death
	if waitErr == nil {
		t.Fatalf("DeleteInstance subprocess exited cleanly (Wait returned nil) — the SIGKILL landed too late, so this run proves nothing")
	}
	exitErr, ok := waitErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("Wait returned a non-ExitError: %v (%T)", waitErr, waitErr)
	}
	ws, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("DeleteInstance subprocess was not terminated by SIGKILL: sys=%#v", exitErr.ProcessState.Sys())
	}
	return killErr == nil, sawBarrier
}

// TestVerifyImagePullFailureLeavesNoOrphans proves a failed image pull leaves no
// network or volume behind. The runner image is on a registry that refuses
// connections, so the pull fails on the real daemon, not a stub. The pull comes
// after the claim network and workspace volume exist and before credentials are
// fetched, so this covers the guard's rollback on that path; (e) in
// TestVerifyAllocation covers the credential-fetch path. The unit-level
// counterpart is TestCreateInstancePullFailureLeavesNoLeftovers.
func TestVerifyImagePullFailureLeavesNoOrphans(t *testing.T) {
	controllerID := randControllerID(t)
	canaries := ensureForeignCanaries(t)
	bin := buildProviderBinary(t)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	// A digest-pinned reference (config.Load requires one unless
	// allow_unpinned_runner_image is set) on a host that refuses connections. No real
	// image needs to exist.
	badImage := "127.0.0.1:39999/does-not-exist@sha256:" + strings.Repeat("0", 64)
	cfg := noCacheConfig(t, badImage)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, canaries[0])
		assertForeignPresent(t, canaries[1])
	}()

	b := bootstrapFor("pullfail-01", srv.URL, caBundle)
	out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
	if code == 0 {
		t.Fatalf("CreateInstance with an unreachable registry unexpectedly succeeded: %s", out)
	}
	// A generic pull failure, not a not-found or duplicate, so the exit is 1, not 30 or 31.
	if code != 1 {
		t.Errorf("CreateInstance image-pull-failure exit=%d, want 1 (generic failure, not 30/31)", code)
	}
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("image-pull-failure creation guard left %d orphaned resources, want 0", n)
	} else {
		t.Logf("image-pull failure left zero orphaned resources (network+volume rolled back before the pull's own failure)")
	}
}

// newHangingCredentialsServer serves HTTPS that answers 404 outside /credentials/
// and blocks every /credentials/ request until the client gives up, as if GARM's
// metadata service never delivered the JIT config. The returned func reports
// whether a /credentials/ request ever arrived, so a test can tell that the
// timeout hit the credential path and not an earlier step.
func newHangingCredentialsServer(t *testing.T) (*httptest.Server, func() bool) {
	t.Helper()
	var mu sync.Mutex
	credsRequested := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/credentials/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		credsRequested = true
		mu.Unlock()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Minute): // safety valve well past the 60s deadline
		}
	}))
	return srv, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return credsRequested
	}
}

// TestVerifyCredentialTimeoutThenSimulatedGARMDelete proves that a metadata
// endpoint that never delivers the JIT config makes CreateInstance fail at the 60s
// credential deadline with nothing left behind, and that the DeleteInstance GARM
// then issues is a clean exit-30 no-op. It waits out the real deadline on a real
// daemon, so it takes about a minute. It checks the cause is the credential
// timeout: the server received a /credentials/ request, the create took about 60s,
// and stderr names the credential fetch.
func TestVerifyCredentialTimeoutThenSimulatedGARMDelete(t *testing.T) {
	controllerID := randControllerID(t)
	canaries := ensureForeignCanaries(t)
	bin := buildProviderBinary(t)

	imageTag := "garm-timeout-sleep:latest"
	buildSleepImage(t, imageTag)
	cfg := noCacheConfig(t, imageTag)

	srv, credsRequested := newHangingCredentialsServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, canaries[0])
		assertForeignPresent(t, canaries[1])
	}()

	b := bootstrapFor("timeout-01", srv.URL, caBundle)
	start := time.Now()
	out, stderr, code := runProviderIO(t, bin, cfg, controllerID, "CreateInstance", "", &b)
	elapsed := time.Since(start)
	t.Logf("CreateInstance with a withheld JIT config took %s to fail (bounded by the 60s credentialFetchDeadline)", elapsed)
	if code == 0 {
		t.Fatalf("CreateInstance with a withheld JIT config unexpectedly succeeded: %s", out)
	}
	if code != 1 {
		t.Errorf("timed-out CreateInstance exit=%d, want 1 (generic failure)", code)
	}

	// (1) The server must have received the /credentials/ request that then hung.
	if !credsRequested() {
		t.Errorf("the metadata server never received a /credentials/ request — the create failed BEFORE the credential fetch, so this does not prove the credential timeout was the cause")
	}

	// (2) The create must have waited out the deadline, not failed early. The upper
	// bound is loose to allow for build, pull and teardown time.
	const deadline = 60 * time.Second
	if elapsed < deadline-5*time.Second {
		t.Errorf("CreateInstance failed after only %s, well before the %s credential deadline — the timeout was NOT the cause", elapsed, deadline)
	}
	if elapsed > deadline+45*time.Second {
		t.Errorf("CreateInstance took %s, far beyond the %s deadline — something other than the bounded credential fetch is at play", elapsed, deadline)
	}

	// (3) stderr must name the credential fetch ("failed to fetch credentials") and
	// give a timeout indication.
	lowerErr := strings.ToLower(stderr)
	if !strings.Contains(lowerErr, "fetch credentials") {
		t.Errorf("provider stderr does not name the credential-fetch failure cause; got:\n%s", stderr)
	}
	if !strings.Contains(lowerErr, "deadline") && !strings.Contains(lowerErr, "timeout") && !strings.Contains(lowerErr, "giving up") && !strings.Contains(lowerErr, "context") {
		t.Errorf("provider stderr does not indicate a timeout/deadline as the cause; got:\n%s", stderr)
	}

	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Fatalf("timed-out create's guard left %d orphaned resources, want 0 (a simulated GARM delete has nothing to reconcile if this is nonzero)", n)
	}

	// The create rolled everything back, so GARM's delete must be a clean no-op: exit 30.
	_, delCode := runProvider(t, bin, cfg, controllerID, "DeleteInstance", "timeout-01", nil)
	if delCode != 30 {
		t.Errorf("simulated GARM delete after a timed-out create: exit=%d, want 30 (already gone, idempotent)", delCode)
	} else {
		t.Logf("simulated GARM delete after a timed-out create correctly returned exit 30 (idempotent teardown)")
	}
}

// TestVerifyCancelDeleteInstanceIdempotent proves that a DeleteInstance killed
// mid-teardown (GARM's exec timing out, or a provider crash) is recovered by
// retrying it. Creation has a rollback guard and deletion has none, so retry
// idempotency is the safety property. The runner image ignores SIGTERM, so
// teardown's first destructive step, ContainerStop, logs a marker and blocks for
// about 10s, and the kill is timed to that marker. The test asserts the marker was
// seen, the kill hit a running process, and the runner container still existed
// afterwards, then that a fresh DeleteInstance converges to zero leftovers.
func TestVerifyCancelDeleteInstanceIdempotent(t *testing.T) {
	controllerID := randControllerID(t)
	canaries := ensureForeignCanaries(t)
	bin := buildProviderBinary(t)

	imageTag := "garm-cancel-trapterm:latest"
	buildTrapTermImage(t, imageTag)
	cfg := noCacheConfig(t, imageTag)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, canaries[0])
		assertForeignPresent(t, canaries[1])
	}()

	b := bootstrapFor("cancel-01", srv.URL, caBundle)
	out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
	if code != 0 {
		t.Fatalf("CreateInstance exit=%d, want 0; stdout=%s", code, out)
	}
	runnerName := spec.RunnerContainerName("cancel-01")

	// Kill DeleteInstance mid-teardown, gated on the SIGTERM marker.
	killedLive, sawBarrier := runDeleteKillOnTeardownBarrier(t, bin, cfg, controllerID, "cancel-01", runnerName)
	if !sawBarrier {
		t.Fatalf("never observed teardown enter its first destructive op (SIGTERM to the runner %q) before the kill — the barrier did not engage, so this run would prove nothing", runnerName)
	}
	if !killedLive {
		t.Fatalf("SIGKILL did not race a still-running DeleteInstance — the process had already exited, so nothing mid-teardown was interrupted")
	}
	// The runner must still exist: teardown was interrupted after ContainerStop began
	// and before ContainerRemove ran.
	if _, err := dockerTry("inspect", runnerName); err != nil {
		t.Fatalf("runner container %q is gone immediately after a mid-teardown kill — the kill did not actually interrupt an in-progress teardown: %v", runnerName, err)
	}
	t.Logf("SIGKILL landed mid-teardown: ContainerStop had delivered SIGTERM (barrier marker observed) but ContainerRemove had not completed (runner %q still present)", runnerName)

	// A fresh DeleteInstance must converge: exit 0 if resources remain, or 30 if the
	// killed run had already finished. Any other code means the kill left a wedge.
	_, retryCode := runProvider(t, bin, cfg, controllerID, "DeleteInstance", "cancel-01", nil)
	if retryCode != 0 && retryCode != 30 {
		t.Errorf("retry DeleteInstance after a mid-teardown kill: exit=%d, want 0 or 30 (idempotent convergence)", retryCode)
	}

	// A further delete must be a no-op, with zero resources left.
	_, finalCode := runProvider(t, bin, cfg, controllerID, "DeleteInstance", "cancel-01", nil)
	if finalCode != 30 {
		t.Errorf("final DeleteInstance after retry: exit=%d, want 30 (fully converged)", finalCode)
	}
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("cancel+retry sequence left %d resources behind, want 0", n)
	} else {
		t.Logf("cancel+retry sequence converged to zero leftover resources")
	}
}

// TestVerifyCrashOrphanSweptOnNextList proves that a runner that exited without
// DeleteInstance ever being called is collected, with its volume and network, by
// the opportunistic sweep on a later ListInstances. It stops the runner directly
// through the Docker CLI, as when an ephemeral runner ends after its one job, waits
// past the exited-runner grace window (2 minutes, in internal/topology) on the real
// clock, then runs a routine ListInstances. The window's boundaries are unit-tested
// in internal/topology/topology_test.go; this checks the wiring against a real
// daemon.
func TestVerifyCrashOrphanSweptOnNextList(t *testing.T) {
	controllerID := randControllerID(t)
	canaries := ensureForeignCanaries(t)
	bin := buildProviderBinary(t)

	imageTag := "garm-crash-sleep:latest"
	buildSleepImage(t, imageTag)
	cfg := noCacheConfig(t, imageTag)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, canaries[0])
		assertForeignPresent(t, canaries[1])
	}()

	b := bootstrapFor("crash-01", srv.URL, caBundle)
	out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
	if code != 0 {
		t.Fatalf("CreateInstance exit=%d, want 0; stdout=%s", code, out)
	}
	var created params.ProviderInstance
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("parse CreateInstance stdout: %v", err)
	}
	runnerName := spec.RunnerContainerName(created.Name)

	// The runner exits on its own, as an ephemeral runner does after its job.
	// DeleteInstance is never called.
	if out, err := dockerTry("stop", runnerName); err != nil {
		t.Fatalf("stop runner to simulate its natural ephemeral exit: %v\n%s", err, out)
	}
	t.Logf("runner %s stopped directly (simulating its own ephemeral exit, never via DeleteInstance) — waiting past the exited-runner sweep grace window", runnerName)

	// exitedRunnerGrace is unexported in internal/topology, so this mirrors its
	// 2-minute value and waits past it.
	const exitedRunnerGraceApprox = 2 * time.Minute
	time.Sleep(exitedRunnerGraceApprox + 15*time.Second)

	// A routine ListInstances triggers the opportunistic sweep (ADR-004).
	if _, code := runProvider(t, bin, cfg, controllerID, "ListInstances", "", nil); code != 0 {
		t.Fatalf("ListInstances (sweep trigger) exit=%d, want 0", code)
	}

	assertGone(t, "container", "inspect", runnerName)
	assertGone(t, "network", "network", "inspect", spec.JobNetworkName("crash-01"))
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("after the sweep, %d resources remain for the crash-abandoned allocation, want 0", n)
	} else {
		t.Logf("the opportunistic sweep (triggered by a routine ListInstances) collected the crash-abandoned allocation")
	}
}

// TestVerifyRestartNoDisruptionNoDoubleProvision proves a provider restart
// neither hides a running allocation nor allows a second one. Every command is a
// fresh subprocess and the provider keeps no in-memory state (ADR-004), so a new
// invocation is what a restarted provider looks like. The test creates an
// allocation, checks that ListInstances and GetInstance from new subprocesses
// report it running, then checks that a duplicate CreateInstance exits 31 and
// leaves the original container's ID and running state unchanged.
func TestVerifyRestartNoDisruptionNoDoubleProvision(t *testing.T) {
	controllerID := randControllerID(t)
	canaries := ensureForeignCanaries(t)
	bin := buildProviderBinary(t)

	imageTag := "garm-restart-sleep:latest"
	buildSleepImage(t, imageTag)
	cfg := noCacheConfig(t, imageTag)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, canaries[0])
		assertForeignPresent(t, canaries[1])
	}()

	b := bootstrapFor("restart-01", srv.URL, caBundle)
	out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
	if code != 0 {
		t.Fatalf("CreateInstance exit=%d, want 0; stdout=%s", code, out)
	}
	var created params.ProviderInstance
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("parse CreateInstance stdout: %v", err)
	}
	runnerName := spec.RunnerContainerName(created.Name)
	originalContainerID := dockerOut(t, "inspect", runnerName, "-f", "{{.Id}}")

	// A fresh ListInstances subprocess stands in for the restarted provider.
	listOut, listCode := runProvider(t, bin, cfg, controllerID, "ListInstances", "", nil)
	if listCode != 0 {
		t.Fatalf("post-restart ListInstances exit=%d, want 0", listCode)
	}
	var list []params.ProviderInstance
	if err := json.Unmarshal([]byte(listOut), &list); err != nil {
		t.Fatalf("parse ListInstances stdout %q: %v", listOut, err)
	}
	var found *params.ProviderInstance
	for i := range list {
		if list[i].Name == "restart-01" {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatalf("restart-01 not visible in a fresh-process ListInstances after the (simulated) restart: %+v", list)
	}
	if found.Status != params.InstanceRunning {
		t.Errorf("post-restart ListInstances status=%q, want running", found.Status)
	}
	if found.ProviderID != "restart-01" {
		t.Errorf("post-restart ListInstances provider_id=%q, want the stable instance name", found.ProviderID)
	}

	getOut, getCode := runProvider(t, bin, cfg, controllerID, "GetInstance", "restart-01", nil)
	if getCode != 0 {
		t.Fatalf("post-restart GetInstance exit=%d, want 0", getCode)
	}
	var got params.ProviderInstance
	if err := json.Unmarshal([]byte(getOut), &got); err != nil {
		t.Fatalf("parse GetInstance stdout %q: %v", getOut, err)
	}
	if got.Status != params.InstanceRunning {
		t.Errorf("post-restart GetInstance status=%q, want running", got.Status)
	}

	// A duplicate create from another fresh process must be rejected.
	dupB := bootstrapFor("restart-01", srv.URL, caBundle)
	dupOut, dupCode := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &dupB)
	if dupCode != 31 {
		t.Errorf("post-restart duplicate CreateInstance exit=%d, want 31; stdout=%s", dupCode, dupOut)
	} else {
		t.Logf("post-restart duplicate CreateInstance correctly returned exit 31 — no double-provisioning")
	}

	// The original allocation must be undisturbed.
	stillContainerID := dockerOut(t, "inspect", runnerName, "-f", "{{.Id}}")
	if stillContainerID != originalContainerID {
		t.Errorf("runner container identity changed across the restart/duplicate sequence: was %s, now %s — a restart or a rejected duplicate must never re-provision", originalContainerID, stillContainerID)
	}
	stillRunning := dockerOut(t, "inspect", runnerName, "-f", "{{.State.Running}}")
	if stillRunning != "true" {
		t.Errorf("runner is not running after the restart/duplicate sequence: State.Running=%s", stillRunning)
	}
	t.Logf("original allocation's container identity and running state are unchanged after simulated restart + duplicate-create attempt")
}

// TestVerifyForeignResourceNonInterference proves that DeleteInstance, the
// opportunistic sweep and RemoveAllInstances never touch a container, volume or
// network that carries no garm.docker labels. It seeds randomly named foreign ones
// (TestVerifyCacheSafety covers a foreign volume at a name the provider would
// claim), runs those operations next to real allocations, and checks the foreign
// Docker IDs are unchanged after every step. It also reads internal/docker/client.go
// to show that no Docker prune endpoint is reachable and that no production file
// imports os/exec, so a prune cannot run at all, rather than merely not having run.
func TestVerifyForeignResourceNonInterference(t *testing.T) {
	controllerID := randControllerID(t)
	canaries := ensureForeignCanaries(t)
	bin := buildProviderBinary(t)

	imageTag := "garm-foreign-sleep:latest"
	buildSleepImage(t, imageTag)
	cfg := noCacheConfig(t, imageTag)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	// Seed foreign, unmanaged resources.
	foreignSuffix := randHex(t)
	foreignContainer := "foreign-container-" + foreignSuffix
	foreignVolume := "foreign-volume-" + foreignSuffix
	foreignNetwork := "foreign-network-" + foreignSuffix

	if out, err := dockerTry("run", "-d", "--name", foreignContainer, "alpine:3.20", "sleep", "3600"); err != nil {
		t.Fatalf("seed foreign container: %v\n%s", err, out)
	}
	if out, err := dockerTry("volume", "create", foreignVolume); err != nil {
		t.Fatalf("seed foreign volume: %v\n%s", err, out)
	}
	if out, err := dockerTry("network", "create", foreignNetwork); err != nil {
		t.Fatalf("seed foreign network: %v\n%s", err, out)
	}
	foreignContainerID := dockerOut(t, "inspect", foreignContainer, "-f", "{{.Id}}")
	foreignVolumeID := dockerOut(t, "volume", "inspect", foreignVolume, "-f", "{{.Name}}")
	foreignNetworkID := dockerOut(t, "network", "inspect", foreignNetwork, "-f", "{{.Id}}")

	defer func() {
		_, _ = dockerTry("rm", "-f", foreignContainer)
		_, _ = dockerTry("volume", "rm", "-f", foreignVolume)
		_, _ = dockerTry("network", "rm", foreignNetwork)
		cleanupController(t, controllerID)
		assertForeignPresent(t, canaries[0])
		assertForeignPresent(t, canaries[1])
	}()

	assertForeignUnchanged := func(step string) {
		t.Helper()
		if got := dockerOut(t, "inspect", foreignContainer, "-f", "{{.Id}}"); got != foreignContainerID {
			t.Errorf("[%s] foreign container identity changed: was %s, now %s", step, foreignContainerID, got)
		}
		if got := dockerOut(t, "volume", "inspect", foreignVolume, "-f", "{{.Name}}"); got != foreignVolumeID {
			t.Errorf("[%s] foreign volume identity changed: was %s, now %s", step, foreignVolumeID, got)
		}
		if got := dockerOut(t, "network", "inspect", foreignNetwork, "-f", "{{.Id}}"); got != foreignNetworkID {
			t.Errorf("[%s] foreign network identity changed: was %s, now %s", step, foreignNetworkID, got)
		}
	}

	// Allocation A: create, then DeleteInstance.
	bA := bootstrapFor("foreign-a", srv.URL, caBundle)
	if _, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &bA); code != 0 {
		t.Fatalf("CreateInstance A failed, exit=%d", code)
	}
	assertForeignUnchanged("after CreateInstance A")

	if _, code := runProvider(t, bin, cfg, controllerID, "DeleteInstance", "foreign-a", nil); code != 0 {
		t.Fatalf("DeleteInstance A failed, exit=%d", code)
	}
	assertForeignUnchanged("after DeleteInstance A")

	// Allocation B: create, then RemoveAllInstances.
	bB := bootstrapFor("foreign-b", srv.URL, caBundle)
	if _, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &bB); code != 0 {
		t.Fatalf("CreateInstance B failed, exit=%d", code)
	}
	assertForeignUnchanged("after CreateInstance B")

	// Routine ListInstances calls exercise the opportunistic sweep and the cache GC.
	for i := 0; i < 2; i++ {
		if _, code := runProvider(t, bin, cfg, controllerID, "ListInstances", "", nil); code != 0 {
			t.Fatalf("ListInstances (sweep/GC trigger) #%d failed, exit=%d", i, code)
		}
	}
	assertForeignUnchanged("after ListInstances sweep/GC")

	if _, code := runProvider(t, bin, cfg, controllerID, "RemoveAllInstances", "", nil); code != 0 {
		t.Fatalf("RemoveAllInstances failed, exit=%d", code)
	}
	assertForeignUnchanged("after RemoveAllInstances")

	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("RemoveAllInstances left %d managed resources behind, want 0", n)
	}

	// No Docker prune endpoint is reachable, and no production file shells out to the
	// docker CLI.
	root := repoRoot(t)
	clientSrc, err := os.ReadFile(filepath.Join(root, "internal", "docker", "client.go"))
	if err != nil {
		t.Fatalf("read internal/docker/client.go: %v", err)
	}
	for _, forbidden := range []string{"ContainersPrune", "VolumesPrune", "NetworksPrune", "ImagesPrune", "Prune("} {
		if strings.Contains(string(clientSrc), forbidden) {
			t.Errorf("internal/docker/client.go's Client interface exposes %q — a Docker prune endpoint must never be reachable from this provider", forbidden)
		}
	}
	if grepProductionExecOfDocker(t, root) {
		t.Error("production (non-test) source shells out via os/exec — this provider must talk to the daemon only through internal/docker.Client, never the docker CLI")
	}
	t.Logf("structurally confirmed: no Prune* SDK method is exposed by internal/docker.Client, and no production file uses os/exec at all")
}

// grepProductionExecOfDocker reports whether any non-test .go file under root
// imports os/exec. Production code talks to the daemon only through
// internal/docker.Client, so any match is a violation.
func grepProductionExecOfDocker(t *testing.T, root string) bool {
	t.Helper()
	out, err := exec.Command("grep", "-rl", "--include=*.go", "os/exec", root).CombinedOutput()
	if err != nil {
		// grep exits 1 when there are no matches — that is the desired state.
		return false
	}
	for _, f := range lines(string(out)) {
		if !strings.HasSuffix(f, "_test.go") {
			return true
		}
	}
	return false
}
