//go:build dockerverify

// This file adds the M3-W2 integration-scenario-audit gap-fill: the plan.md
// M3 acceptance scenarios NOT already covered by the existing dockerverify
// harness (verify_test.go's TestVerifyM1WP2Allocation and its siblings). Each
// new test's doc comment states exactly what pre-existing coverage it is
// NOT duplicating.
//
// # M3 acceptance-criterion -> proving-test matrix
//
// (plan.md §3 M3, item 3's bullet list, plus §4's headline restart criterion)
//
//   - lifecycle (create/attach/mount/security/credential-delivery/delete)
//     -> TestVerifyM1WP2Allocation (verify_test.go) — PRE-EXISTING, unchanged.
//   - cancellation (delete mid-run)
//     -> TestVerifyM3CancelDeleteInstanceIdempotent (this file) — NEW.
//   - timeout (metadata withholds JIT config, simulating GARM's own delete)
//     -> TestVerifyM3CredentialTimeoutThenSimulatedGARMDelete (this file) — NEW.
//   - image-pull failure (creation guard leaves zero orphans)
//     -> TestVerifyM3ImagePullFailureLeavesNoOrphans (this file) — NEW.
//     (a UNIT-level equivalent already existed:
//     TestCreateInstancePullFailureLeavesNoLeftovers, internal/provider/
//     create_test.go — this adds the live-daemon proof plan.md's M3 item 2
//     specifically asks for: "CI runs docker:dind ... tests exec the
//     provider binary directly".)
//   - simulated provider crash (orphan sweep collects it on the next
//     Create/List)
//     -> TestVerifyM3CrashOrphanSweptOnNextList (this file) — NEW.
//   - ID-or-name resolution
//     -> PRE-EXISTING: TestVerifyM1WP2Allocation resolves by NAME throughout
//     (GARM_INSTANCE_ID is always the instance name in this provider — F6,
//     research.md §1.E — so "ID-or-name" collapses to "the instance-name
//     label lookup", exercised by every test in this package); ID-shaped-name
//     collision-safety is unit-tested in internal/provider/resolver_test.go
//     paths (TestResolveRejectsUnmanaged and friends).
//   - both idempotency exit codes (30, 31)
//     -> PRE-EXISTING: TestVerifyM1WP2Allocation (c) duplicate create -> 31,
//     (d) repeat delete -> 30.
//   - concurrent-creation race
//     -> covered at the unit level (internal/topology/*_test.go's
//     inflightGrace/nonce tests) per plan.md's own note that this is a
//     WP2/ADR-004 property; no live-daemon-only failure mode was found that
//     unit coverage misses, so no new live test was added for it here.
//   - runner-callback visibility enumeration
//     -> out of scope for this audit (enable_runner_callbacks is not
//     implemented in this provider as of M3-W2; flagged in the final report
//     rather than silently skipped).
//   - foreign-resource non-interference
//     -> PRE-EXISTING (partial): every test in this package snapshots/asserts
//     hbot-lab-mongodb/hummingbot presence and, in several tests, a foreign
//     VOLUME. NEW, comprehensive: TestVerifyM3ForeignResourceNonInterference
//     (this file) additionally seeds a foreign CONTAINER, VOLUME, and NETWORK
//     with zero garm.docker labels, drives DeleteInstance + the opportunistic
//     sweep + RemoveAllInstances against them all, and structurally proves
//     (by reading this repo's own source) that no Docker prune endpoint is
//     even reachable from production code.
//   - M3 headline acceptance: "a provider or GARM restart neither disrupts a
//     currently running job nor causes double-provisioning" (plan.md §4)
//     -> NEW: TestVerifyM3RestartNoDisruptionNoDoubleProvision (this file).
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
	"testing"
	"time"

	"github.com/cloudbase/garm-provider-common/params"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// buildM3ProviderBinary builds the real provider binary once per test,
// mirroring every other test in this package.
func buildM3ProviderBinary(t *testing.T) string {
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

// noCacheConfig writes a minimal config with the persistent cache DISABLED,
// so controllerResourceCount's "zero leftover resources" assertions are not
// muddied by cache volumes, which deliberately survive DeleteInstance/rollback
// by design (ADR-003) — the same reason TestVerifyM1WP2Allocation's own config
// disables the cache.
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

// runProviderKillMidway starts a REAL DeleteInstance subprocess for
// instanceID and SIGKILLs it after killAfter — simulating a cancellation
// (GARM's own exec timing out) or a provider-process crash mid-teardown. It
// returns whether the kill actually raced a still-running process (false
// means the subprocess had already exited on its own before the kill fired,
// in which case there is nothing left to prove about mid-run interruption for
// this particular attempt, but the caller's retry-converges assertion still
// holds either way).
func runProviderKillMidway(t *testing.T, bin, configFile, controllerID, instanceID string, killAfter time.Duration) bool {
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
	time.Sleep(killAfter)
	killErr := cmd.Process.Kill()
	_ = cmd.Wait() // reap regardless of whether Kill raced a natural exit
	return killErr == nil
}

// =============================================================================
// image-pull failure: the creation guard leaves zero orphans.
// =============================================================================

// TestVerifyM3ImagePullFailureLeavesNoOrphans drives CreateInstance with a
// config runner_image pointing at an UNREACHABLE registry host
// (127.0.0.1:39999 — connection refused, no network dependency, fast) so the
// pull genuinely fails on the real daemon (not a stubbed/fake failure, unlike
// the unit-level TestCreateInstancePullFailureLeavesNoLeftovers). Credential
// fetch succeeds first (CreateInstance fetches credentials BEFORE pulling the
// image, create.go step 3 vs step 4), so this specifically exercises the
// image-pull failure branch of the creation guard, not the credential-fetch
// branch TestVerifyM1WP2Allocation's (e) already covers.
func TestVerifyM3ImagePullFailureLeavesNoOrphans(t *testing.T) {
	controllerID := randControllerID(t)
	bin := buildM3ProviderBinary(t)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	// A syntactically valid, digest-pinned reference (config.Load requires
	// digest-pinning unless allow_unpinned_runner_image is set) at a host that
	// refuses connections immediately — no real image ever needs to exist.
	badImage := "127.0.0.1:39999/does-not-exist@sha256:" + strings.Repeat("0", 64)
	cfg := noCacheConfig(t, badImage)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
	}()

	b := bootstrapFor("m3-pullfail-01", srv.URL, caBundle)
	out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
	if code == 0 {
		t.Fatalf("CreateInstance with an unreachable registry unexpectedly succeeded: %s", out)
	}
	// Not a not-found/duplicate condition — a generic pull failure — so exit
	// must be 1 (execcommon's catch-all), not 30/31 (exit-code taxonomy audit,
	// internal/provider/taxonomy.go).
	if code != 1 {
		t.Errorf("CreateInstance image-pull-failure exit=%d, want 1 (generic failure, not 30/31)", code)
	}
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("image-pull-failure creation guard left %d orphaned resources, want 0", n)
	} else {
		t.Logf("image-pull failure left zero orphaned resources (network+volume rolled back before the pull's own failure)")
	}
}

// =============================================================================
// timeout: metadata withholds the JIT config; a simulated GARM delete after
// is idempotent.
// =============================================================================

// newHangingCredentialsServer serves an HTTPS endpoint that answers any
// non-/credentials/* path with 404 but BLOCKS every /credentials/* request
// until the client cancels it — modeling GARM's metadata service simply
// never delivering the JIT config, as opposed to TestVerifyM1WP2Allocation's
// (e), which uses a fast 401 for a WRONG token. The handler selects on
// r.Context().Done() so it never leaks a goroutine once the client (bounded
// by CreateInstance's own credentialFetchDeadline, 60s) gives up.
func newHangingCredentialsServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/credentials/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Minute): // safety valve well past the 60s deadline
		}
	}))
}

// TestVerifyM3CredentialTimeoutThenSimulatedGARMDelete serves a metadata
// endpoint that never responds to /credentials/* requests (withholding the
// JIT config entirely). fetchCredentials is bounded by CreateInstance's own
// credentialFetchDeadline (60s, create.go) — this test genuinely waits that
// out on the real daemon rather than mocking the clock, so it runs for
// roughly a minute. After the create fails and the guard rolls back, it
// invokes DeleteInstance for the same instance name — the exact shape of
// GARM's OWN reaper deleting an instance it gave up bootstrapping
// (research.md §1.F) — and asserts idempotent teardown: exit 30 (nothing
// left to delete), never an error.
func TestVerifyM3CredentialTimeoutThenSimulatedGARMDelete(t *testing.T) {
	controllerID := randControllerID(t)
	bin := buildM3ProviderBinary(t)

	imageTag := "garm-m3-timeout-sleep:latest"
	buildSleepImage(t, imageTag)
	cfg := noCacheConfig(t, imageTag)

	srv := newHangingCredentialsServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
	}()

	b := bootstrapFor("m3-timeout-01", srv.URL, caBundle)
	start := time.Now()
	out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
	elapsed := time.Since(start)
	t.Logf("CreateInstance with a withheld JIT config took %s to fail (bounded by the 60s credentialFetchDeadline)", elapsed)
	if code == 0 {
		t.Fatalf("CreateInstance with a withheld JIT config unexpectedly succeeded: %s", out)
	}
	if code != 1 {
		t.Errorf("timed-out CreateInstance exit=%d, want 1 (generic failure)", code)
	}
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Fatalf("timed-out create's guard left %d orphaned resources, want 0 (a simulated GARM delete has nothing to reconcile if this is nonzero)", n)
	}

	// Simulated GARM reaper delete: the creation guard already rolled
	// everything back, so this must be a clean idempotent no-op — exit 30.
	_, delCode := runProvider(t, bin, cfg, controllerID, "DeleteInstance", "m3-timeout-01", nil)
	if delCode != 30 {
		t.Errorf("simulated GARM delete after a timed-out create: exit=%d, want 30 (already gone, idempotent)", delCode)
	} else {
		t.Logf("simulated GARM delete after a timed-out create correctly returned exit 30 (idempotent teardown)")
	}
}

// =============================================================================
// cancel: DeleteInstance killed mid-run must still converge to a clean,
// idempotent teardown on retry.
// =============================================================================

// TestVerifyM3CancelDeleteInstanceIdempotent creates one allocation, starts a
// REAL DeleteInstance subprocess, and SIGKILLs it shortly after it begins
// (simulating GARM's own exec timing out, or a provider-process crash,
// mid-teardown — ADR-004's creation guard has no analogous "delete guard",
// so the safety property under test is idempotency-on-retry, not
// atomicity-of-a-single-run). It does not assert a specific mid-teardown
// state (the exact interruption point is inherently timing-dependent and not
// meaningful to pin down); it asserts the property that actually matters: a
// FRESH DeleteInstance run after the kill converges to zero leftover
// resources, with no dangling half-removed state that a retry cannot clean up.
func TestVerifyM3CancelDeleteInstanceIdempotent(t *testing.T) {
	controllerID := randControllerID(t)
	bin := buildM3ProviderBinary(t)

	imageTag := "garm-m3-cancel-sleep:latest"
	buildSleepImage(t, imageTag)
	cfg := noCacheConfig(t, imageTag)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
	}()

	b := bootstrapFor("m3-cancel-01", srv.URL, caBundle)
	out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
	if code != 0 {
		t.Fatalf("CreateInstance exit=%d, want 0; stdout=%s", code, out)
	}

	// Start a real DeleteInstance subprocess and kill it shortly after launch
	// — simulating a cancellation/crash mid-teardown.
	killedLive := runProviderKillMidway(t, bin, cfg, controllerID, "m3-cancel-01", 60*time.Millisecond)
	t.Logf("first DeleteInstance was interrupted mid-run (killed a still-running process=%v)", killedLive)

	// A fresh, uninterrupted DeleteInstance must now converge to a clean
	// state — either it finds resources still there and removes them (exit
	// 0), or the killed run already finished the job before being reaped
	// (exit 30, already gone). Both are acceptable; any OTHER code is not —
	// that would mean the kill left an unrecoverable wedge.
	_, retryCode := runProvider(t, bin, cfg, controllerID, "DeleteInstance", "m3-cancel-01", nil)
	if retryCode != 0 && retryCode != 30 {
		t.Errorf("retry DeleteInstance after a mid-run kill: exit=%d, want 0 or 30 (idempotent convergence)", retryCode)
	}

	// Whichever path the retry took, one more DeleteInstance must now be a
	// clean no-op, and zero resources may remain.
	_, finalCode := runProvider(t, bin, cfg, controllerID, "DeleteInstance", "m3-cancel-01", nil)
	if finalCode != 30 {
		t.Errorf("final DeleteInstance after retry: exit=%d, want 30 (fully converged)", finalCode)
	}
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("cancel+retry sequence left %d resources behind, want 0", n)
	} else {
		t.Logf("cancel+retry sequence converged to zero leftover resources")
	}
}

// =============================================================================
// provider-crash orphan: a runner that exits without ever being
// DeleteInstance'd is collected by the next opportunistic sweep.
// =============================================================================

// TestVerifyM3CrashOrphanSweptOnNextList models a provider (or GARM) that
// crashed after a job's ephemeral runner naturally exited (every real
// actions/runner is single-job-ephemeral — research.md §3.A) but BEFORE
// DeleteInstance was ever called for it: it creates one allocation normally,
// then stops the runner container directly via the Docker CLI (never through
// DeleteInstance — modeling the runner process exiting on its own after its
// one job, exactly like a real ephemeral runner), then waits past the
// exited-runner sweep grace window (internal/topology's exitedRunnerGrace,
// currently 2 minutes — this test intentionally does NOT mock the clock, to
// prove the real end-to-end wiring; the grace window's own boundary
// conditions are already exhaustively unit-tested with a fake clock in
// internal/topology/topology_test.go). A later, unrelated ListInstances call
// (simulating GARM's routine poll — the provider's normal invocation
// pattern, not a special "recovery" command) must have collected the
// abandoned allocation's container, volume, and network — proving the
// opportunistic sweep, not a manual rescue, is what reclaims a genuinely
// crash-abandoned allocation.
func TestVerifyM3CrashOrphanSweptOnNextList(t *testing.T) {
	controllerID := randControllerID(t)
	bin := buildM3ProviderBinary(t)

	imageTag := "garm-m3-crash-sleep:latest"
	buildSleepImage(t, imageTag)
	cfg := noCacheConfig(t, imageTag)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
	}()

	b := bootstrapFor("m3-crash-01", srv.URL, caBundle)
	out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
	if code != 0 {
		t.Fatalf("CreateInstance exit=%d, want 0; stdout=%s", code, out)
	}
	var created params.ProviderInstance
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("parse CreateInstance stdout: %v", err)
	}
	runnerName := spec.RunnerContainerName(created.Name)

	// The runner exits on its own (an ephemeral runner's normal end-of-job
	// behavior) — NEVER via DeleteInstance. This is the "crash" this test
	// models: whatever would have called DeleteInstance next never did.
	if out, err := dockerTry("stop", runnerName); err != nil {
		t.Fatalf("stop runner to simulate its natural ephemeral exit: %v\n%s", err, out)
	}
	t.Logf("runner %s stopped directly (simulating its own ephemeral exit, never via DeleteInstance) — waiting past the exited-runner sweep grace window", runnerName)

	// exitedRunnerGrace is unexported in internal/topology; its value is
	// documented (and unit-tested) as 2 minutes as of M2/M3. Wait
	// comfortably past it on the real clock.
	const exitedRunnerGraceApprox = 2 * time.Minute
	time.Sleep(exitedRunnerGraceApprox + 15*time.Second)

	// A ROUTINE, unrelated ListInstances call — not a special recovery
	// command — is what triggers the opportunistic sweep (ADR-004).
	if _, code := runProvider(t, bin, cfg, controllerID, "ListInstances", "", nil); code != 0 {
		t.Fatalf("ListInstances (sweep trigger) exit=%d, want 0", code)
	}

	assertGone(t, "container", "inspect", runnerName)
	assertGone(t, "network", "network", "inspect", spec.JobNetworkName("m3-crash-01"))
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("after the sweep, %d resources remain for the crash-abandoned allocation, want 0", n)
	} else {
		t.Logf("the opportunistic sweep (triggered by a routine ListInstances) collected the crash-abandoned allocation")
	}
}

// =============================================================================
// restart / no double-provisioning: the M3 headline acceptance criterion.
// =============================================================================

// TestVerifyM3RestartNoDisruptionNoDoubleProvision is plan.md §4's headline
// M3 acceptance criterion, made concrete: "a provider or GARM restart neither
// interrupts a currently running job nor causes double-provisioning." Every
// GARM command is already a fresh subprocess invocation in this provider's
// one-shot model (ADR-004) — there is no in-memory state to lose on a
// restart — so this test proves that property explicitly: it creates one
// allocation, then drives ListInstances and GetInstance from BRAND-NEW
// subprocess invocations (exactly what a restarted provider binary's next
// invocation looks like) and asserts the running allocation is correctly
// and fully visible, then attempts a duplicate CreateInstance from yet
// another fresh invocation and asserts it is rejected (exit 31) WITHOUT
// disturbing the original allocation's container identity or running state.
func TestVerifyM3RestartNoDisruptionNoDoubleProvision(t *testing.T) {
	controllerID := randControllerID(t)
	bin := buildM3ProviderBinary(t)

	imageTag := "garm-m3-restart-sleep:latest"
	buildSleepImage(t, imageTag)
	cfg := noCacheConfig(t, imageTag)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
	}()

	b := bootstrapFor("m3-restart-01", srv.URL, caBundle)
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

	// --- "provider restart": a FRESH subprocess invocation of ListInstances ---
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
		if list[i].Name == "m3-restart-01" {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatalf("m3-restart-01 not visible in a fresh-process ListInstances after the (simulated) restart: %+v", list)
	}
	if found.Status != params.InstanceRunning {
		t.Errorf("post-restart ListInstances status=%q, want running", found.Status)
	}
	if found.ProviderID != "m3-restart-01" {
		t.Errorf("post-restart ListInstances provider_id=%q, want the stable instance name (F6)", found.ProviderID)
	}

	// --- a FRESH subprocess GetInstance ---
	getOut, getCode := runProvider(t, bin, cfg, controllerID, "GetInstance", "m3-restart-01", nil)
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

	// --- a FRESH subprocess duplicate CreateInstance must be rejected ---
	dupB := bootstrapFor("m3-restart-01", srv.URL, caBundle)
	dupOut, dupCode := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &dupB)
	if dupCode != 31 {
		t.Errorf("post-restart duplicate CreateInstance exit=%d, want 31; stdout=%s", dupCode, dupOut)
	} else {
		t.Logf("post-restart duplicate CreateInstance correctly returned exit 31 — no double-provisioning")
	}

	// --- the ORIGINAL allocation must be completely undisturbed throughout ---
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

// =============================================================================
// foreign-resource non-interference (comprehensive).
// =============================================================================

// TestVerifyM3ForeignResourceNonInterference seeds a foreign, completely
// UNLABELED container, volume, and network (no garm.docker/* labels at all —
// distinct from TestVerifyM2CacheSafety's foreign-squatter tests, which seed
// a foreign volume at a name this provider WOULD claim; these seed random
// names this provider has no reason to ever touch), then drives
// DeleteInstance, the opportunistic sweep (via ListInstances), and
// RemoveAllInstances — the full set of destructive/near-destructive
// operations this provider exposes — against a real managed allocation
// running alongside them. It asserts all three foreign resources are BYTE-
// IDENTICAL (same Docker ID) before and after every step, and additionally
// proves — by reading this repository's own internal/docker/client.go source
// — that no Docker prune endpoint (ContainersPrune/VolumesPrune/
// NetworksPrune/ImagesPrune) is even reachable from production code, and that
// no production file shells out to the docker CLI at all (grep for os/exec
// outside *_test.go), which is what makes "docker system prune is never
// invoked" true STRUCTURALLY rather than merely "not observed in this one
// test run."
func TestVerifyM3ForeignResourceNonInterference(t *testing.T) {
	controllerID := randControllerID(t)
	bin := buildM3ProviderBinary(t)

	imageTag := "garm-m3-foreign-sleep:latest"
	buildSleepImage(t, imageTag)
	cfg := noCacheConfig(t, imageTag)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	// --- seed foreign, completely unmanaged resources -------------------------
	foreignSuffix := randHex(t)
	foreignContainer := "m3-foreign-container-" + foreignSuffix
	foreignVolume := "m3-foreign-volume-" + foreignSuffix
	foreignNetwork := "m3-foreign-network-" + foreignSuffix

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
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
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

	// --- managed allocation A: create then DeleteInstance ---------------------
	bA := bootstrapFor("m3-foreign-a", srv.URL, caBundle)
	if _, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &bA); code != 0 {
		t.Fatalf("CreateInstance A failed, exit=%d", code)
	}
	assertForeignUnchanged("after CreateInstance A")

	if _, code := runProvider(t, bin, cfg, controllerID, "DeleteInstance", "m3-foreign-a", nil); code != 0 {
		t.Fatalf("DeleteInstance A failed, exit=%d", code)
	}
	assertForeignUnchanged("after DeleteInstance A")

	// --- managed allocation B: create, then RemoveAllInstances ----------------
	bB := bootstrapFor("m3-foreign-b", srv.URL, caBundle)
	if _, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &bB); code != 0 {
		t.Fatalf("CreateInstance B failed, exit=%d", code)
	}
	assertForeignUnchanged("after CreateInstance B")

	// A couple of routine ListInstances calls exercise the opportunistic sweep
	// and cache GC without disturbing anything foreign.
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

	// --- structural proof: no Docker prune endpoint is reachable, and no ------
	// --- production file shells out to the docker CLI at all. -----------------
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
// imports "os/exec" — this provider's production code must never shell out
// to the docker CLI (it talks to the daemon exclusively through
// internal/docker.Client / the moby SDK), so ANY match here would already be
// a structural violation regardless of what it's used for.
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
