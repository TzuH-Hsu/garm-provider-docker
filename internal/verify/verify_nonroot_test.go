//go:build dockerverify

// This file adds the M1 cross-family (F1/F2/F4/F6) real-daemon verification
// harness, exercised as the NON-ROOT runner user over a real workspace
// bind-mount — the two conditions the earlier root-run harness masked. It is
// gated behind the same `dockerverify` build tag as the WP2/WP3 harnesses (a
// live daemon, image builds, and outbound internet for the nested pulls). Run
// it explicitly with:
//
//	go test -tags dockerverify -v -run TestVerifyM1NonRootDindAllocation ./internal/verify/
//
// It drives the REAL provider binary through a full privileged-sidecar DinD
// allocation and asserts, with actual command output executed AS THE RUNNER
// USER (uid/gid 1001, not root):
//   - F1: `docker info` AND `docker run --rm hello-world` succeed as the runner
//     user over the shared dind socket (the root-run test hid the permission gap);
//   - F2: a file written in the runner workspace is visible AND modifiable
//     through a nested `docker run -v <workspacepath>:/w …` (and vice-versa),
//     proving the workspace is shared into the daemon;
//   - the W3 red lines still hold (nested workload invisible to the host; no
//     host docker.sock; creds tmpfs-only; no token in env);
//   - F4/F6 teardown: DeleteInstance reaps runner+dind+volumes+network (repeat
//     -> exit 30), AND delete-by-provider_id after the runner is removed out of
//     band still reaps the privileged sidecar + volumes (no leak).
//
// Everything is scoped to a unique controller-id and torn down by label; it
// snapshots host containers/networks/volumes before/after and asserts they are
// identical, so it never touches foreign resources.
package verify

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudbase/garm-provider-common/params"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

const nrInstance = "nr-verify-01"

func nrNet() string      { return nrInstance + "-net" }
func nrDindName() string { return nrInstance + "-dind" }

func nrBootstrap(metadataURL string, caBundle []byte) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name:             nrInstance,
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

// buildNonRootRunnerImage builds the runner image the F1 harness runs, layered
// on the REAL production runner image (runner-images/noble — its real Dockerfile
// and real entrypoint.sh) so F1 is proven against the production code VERBATIM,
// not a re-implementation with softened error handling (N3). The base ships the
// genuine `runner` user, gosu, the docker CLI, and getent/groupadd/usermod, and
// its /entrypoint.sh is the ACTUAL runner-images/noble/entrypoint.sh whose
// ensure_docker_socket_group carries the F1 fix.
//
// The ONLY deviation from production is the wrapper entrypoint: it SOURCES the
// real /entrypoint.sh (whose main is guarded to run only when EXECUTED directly,
// never when sourced) and invokes the REAL ensure_docker_socket_group under that
// file's own `set -euo pipefail`, then sleeps as PID 1 (root) so the harness can
// `docker exec -u runner` in. Production would `exec ./run.sh` here, but the fake
// JIT credentials cannot drive the real .NET runner, so we sleep instead. This is
// load-bearing: were the F1 bug present (the un-guarded getent probe aborting
// under set -e on a clean image with no GID 2000), sourcing + calling the real
// ensure_docker_socket_group would abort HERE, the container would exit, and
// every `docker exec -u runner` below would fail — so the harness would have
// caught F1 rather than masking it behind an Alpine `|| true` re-implementation.
func buildNonRootRunnerImage(t *testing.T, root, tag string) {
	t.Helper()

	// 1. Build the REAL production runner image (real Dockerfile + entrypoint).
	nobleTag := "garm-nr-noble-real:latest"
	if out, err := dockerTry("build", "-t", nobleTag, filepath.Join(root, "runner-images", "noble")); err != nil {
		t.Fatalf("build real runner-images/noble image: %v\n%s", err, out)
	}

	// 2. Layer a wrapper that runs ONLY the real ensure_docker_socket_group
	//    (sourced from the real entrypoint) then sleeps, so the harness can exec
	//    as the non-root runner and prove F1 over the shared dind socket.
	dir := t.TempDir()
	wrapper := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"# Source the REAL production entrypoint (main is guarded not to run when\n" +
		"# sourced) and invoke the REAL ensure_docker_socket_group under its own\n" +
		"# `set -euo pipefail`: this IS the F1 code path, verbatim.\n" +
		"# shellcheck source=/dev/null\n" +
		"source /entrypoint.sh\n" +
		"ensure_docker_socket_group\n" +
		"# prepare_workdir equivalent: the workspace volume mounts root-owned.\n" +
		"mkdir -p /actions-runner/_work\n" +
		"chown runner:runner /actions-runner/_work\n" +
		"exec sleep infinity\n"
	writeFile(t, filepath.Join(dir, "test-entrypoint.sh"), wrapper)
	dockerfile := "FROM " + nobleTag + "\n" +
		"COPY --chmod=0755 test-entrypoint.sh /test-entrypoint.sh\n" +
		"ENTRYPOINT [\"/test-entrypoint.sh\"]\n"
	writeFile(t, filepath.Join(dir, "Dockerfile"), dockerfile)
	if out, err := dockerTry("build", "-t", tag, dir); err != nil {
		t.Fatalf("build non-root runner wrapper image: %v\n%s", err, out)
	}
}

// execRunner runs `docker exec -u runner <container> <args...>` — i.e. AS THE
// NON-ROOT runner user (1001), with its /etc/group memberships (including the
// dockersock group the entrypoint added), which is what makes F1 meaningful.
func execRunner(container string, args ...string) (string, error) {
	full := append([]string{"exec", "-u", "runner", container}, args...)
	return dockerTry(full...)
}

func TestVerifyM1NonRootDindAllocation(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)

	// --- foreign snapshot (must be identical after) --------------------------
	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")
	beforeC := snapshotIDs(t, "ps", "-a", "--format", "{{.Names}}")
	beforeN := snapshotIDs(t, "network", "ls", "--format", "{{.Name}}")
	beforeV := snapshotIDs(t, "volume", "ls", "--format", "{{.Name}}")
	t.Logf("[snapshot] before: %d containers, %d networks, %d volumes", len(beforeC), len(beforeN), len(beforeV))

	// --- build the non-root runner image + the real provider binary ----------
	runnerTag := "garm-nr-verify-cli:latest"
	buildNonRootRunnerImage(t, root, runnerTag)

	// N3: GID 2000 (spec.DindSocketGID) must be ABSENT in the freshly built image
	// BEFORE the entrypoint runs. The F1 regression relies on the base image NOT
	// pre-declaring this group — ensure_docker_socket_group must CREATE it, and
	// the F1 bug was exactly that create path aborting under `set -e` on a clean
	// image with no GID 2000. If a future base-image change pre-created GID 2000,
	// F1 would go green without ever exercising the group-create path this harness
	// guards; assert its absence so that silent weakening fails loudly here.
	// `getent group 2000` exits non-zero when the group is absent (the wanted
	// state); a zero exit means it already exists.
	if out, err := dockerTry("run", "--rm", "--entrypoint", "getent", runnerTag, "group", spec.DindSocketGID); err == nil {
		t.Fatalf("[N3] GID %s already exists in the freshly built image before the entrypoint runs (getent: %q); the F1 group-create path would be silently skipped", spec.DindSocketGID, strings.TrimSpace(out))
	}
	t.Logf("[N3] GID %s absent in the freshly built image pre-entrypoint — the F1 group-create path stays load-bearing", spec.DindSocketGID)

	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	// --- provider config: privileged-sidecar, pinned dind digest -------------
	configDir := t.TempDir()
	configFile := filepath.Join(configDir, "config.toml")
	writeFile(t, configFile, strings.Join([]string{
		`docker_host = "unix:///var/run/docker.sock"`,
		`runner_image = "` + runnerTag + `"`,
		`allow_unpinned_runner_image = true`,
		// The ceiling defaults to ["none"] (fail-closed, ADR-001 2026-07-26
		// Amendment), so a DinD mode must be widened in explicitly or the
		// config would not load at all.
		`allowed_dind_modes = ["none", "privileged-sidecar"]`,
		`dind_mode = "privileged-sidecar"`,
		`dind_image = "` + dindImageDigest + `"`,
		`storage_driver = "overlay2"`,
		`[resources]`,
		`dind_memory = "2GiB"`,
		// M2 persistent cache off: this test verifies the non-root DinD topology
		// and teardown in isolation, and cache volumes deliberately survive
		// DeleteInstance (covered by TestVerifyM2W1CacheHit), so leaving it on
		// would break the post-delete zero-managed-resources assertions.
		`[cache]`,
		`enabled = false`,
		"",
	}, "\n"))

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
		assertSnapshotIdentical(t, "containers", beforeC, "ps", "-a", "--format", "{{.Names}}")
		assertSnapshotIdentical(t, "networks", beforeN, "network", "ls", "--format", "{{.Name}}")
		assertSnapshotIdentical(t, "volumes", beforeV, "volume", "ls", "--format", "{{.Name}}")
	}()

	// =========================================================================
	// CreateInstance
	// =========================================================================
	b := nrBootstrap(srv.URL, caBundle)
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
	// F6: provider_id is the stable instance NAME, not a container id.
	if created.ProviderID != nrInstance {
		t.Errorf("[F6] provider_id=%q, want the instance name %q", created.ProviderID, nrInstance)
	}
	runnerName := spec.RunnerContainerName(nrInstance)
	t.Logf("[create] provider_id=%s; runner container=%s; sidecar=%s", created.ProviderID, runnerName, nrDindName())

	// F4: the three job-scoped volumes carry generation-nonce names the harness
	// does not know up front, so discover their ACTUAL names by resource label.
	nrWorkspace := volumeNameByResource(t, controllerID, "workspace")
	nrSocket := volumeNameByResource(t, controllerID, "socket")
	nrDindState := volumeNameByResource(t, controllerID, "dind-state")
	t.Logf("[create] generation-nonce volume names: workspace=%s socket=%s dind-state=%s", nrWorkspace, nrSocket, nrDindState)

	// F1 topology: the runner carries the DinD socket GID as a supplementary
	// group, and dockerd was launched with the matching --group.
	groupAdd := dockerOut(t, "inspect", runnerName, "-f", "{{json .HostConfig.GroupAdd}}")
	t.Logf("[F1] runner HostConfig.GroupAdd=%s (want to include %s)", groupAdd, spec.DindSocketGID)
	if !strings.Contains(groupAdd, spec.DindSocketGID) {
		t.Errorf("[F1] runner GroupAdd=%s, want it to include the DinD socket GID %s", groupAdd, spec.DindSocketGID)
	}
	dindCmd := dockerOut(t, "inspect", nrDindName(), "-f", "{{json .Config.Cmd}}")
	if !strings.Contains(dindCmd, "--group="+spec.DindSocketGID) {
		t.Errorf("[F1] dind cmd=%s, want --group=%s", dindCmd, spec.DindSocketGID)
	}

	if !waitForDindReady(t, nrDindName()) {
		t.Fatalf("dind daemon never became ready")
	}

	// =========================================================================
	// F1 — as the NON-ROOT runner user: `docker info` and `docker run hello-world`
	// =========================================================================
	whoami, _ := execRunner(runnerName, "id")
	t.Logf("[F1] runner-user identity: %s", strings.TrimSpace(whoami))
	if info, err := execRunner(runnerName, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Errorf("[F1] `docker info` as the runner user FAILED (the permission gap): %v\n%s", err, info)
	} else {
		t.Logf("[F1] `docker info` as the runner user OK; dind ServerVersion=%s", strings.TrimSpace(info))
	}
	if hw, err := execRunner(runnerName, "docker", "run", "--rm", "hello-world"); err != nil {
		t.Errorf("[F1] `docker run --rm hello-world` as the runner user FAILED: %v\n%s", err, hw)
	} else {
		t.Logf("[F1] `docker run --rm hello-world` as the runner user OK: %s", firstLine(hw))
	}

	// =========================================================================
	// F2 — workspace is shared with the daemon: runner<->nested-container both ways
	// =========================================================================
	// Pre-pull busybox into the dind daemon so the nested `docker run` output is
	// just the bind-mounted file content, not interleaved pull progress.
	if out, err := execRunner(runnerName, "docker", "pull", "busybox"); err != nil {
		t.Fatalf("[F2] pre-pull busybox into the dind daemon failed: %v\n%s", err, out)
	}
	work := spec.RunnerWorkDir
	if out, err := execRunner(runnerName, "sh", "-c", "echo runner-wrote-this > "+work+"/f2.txt"); err != nil {
		t.Fatalf("[F2] runner could not write its workspace: %v\n%s", err, out)
	}
	// A nested container bind-mounting the workspace path must see the runner's file.
	seen, err := execRunner(runnerName, "docker", "run", "--rm", "-v", work+":/w", "busybox", "cat", "/w/f2.txt")
	if err != nil {
		t.Errorf("[F2] nested container could not read the workspace bind: %v\n%s", err, seen)
	} else if strings.TrimSpace(seen) != "runner-wrote-this" {
		t.Errorf("[F2] nested container saw %q through the workspace bind, want %q — the daemon resolved an EMPTY path, not the runner's files", strings.TrimSpace(seen), "runner-wrote-this")
	} else {
		t.Logf("[F2] nested container read the runner-written file through -v %s:/w", work)
	}
	// And a write from the nested container is visible back in the runner workspace.
	if out, err := execRunner(runnerName, "docker", "run", "--rm", "-v", work+":/w", "busybox", "sh", "-c", "echo nested-wrote-this > /w/f2b.txt"); err != nil {
		t.Errorf("[F2] nested container could not write the workspace bind: %v\n%s", err, out)
	}
	if back, err := execRunner(runnerName, "cat", work+"/f2b.txt"); err != nil {
		t.Errorf("[F2] runner could not read the nested-written file back: %v\n%s", err, back)
	} else if strings.TrimSpace(back) != "nested-wrote-this" {
		t.Errorf("[F2] runner read %q, want the nested container's write %q", strings.TrimSpace(back), "nested-wrote-this")
	} else {
		t.Logf("[F2] runner read back the nested-container-written file — workspace is shared both ways")
	}

	// =========================================================================
	// W3 red lines still hold
	// =========================================================================
	nested := "nr-nested-busybox"
	if out, err := execRunner(runnerName, "docker", "run", "-d", "--name", nested, "busybox", "sleep", "300"); err != nil {
		t.Errorf("could not start a nested container via the runner: %v\n%s", err, out)
	}
	if hostPS := dockerOut(t, "ps", "-a", "--format", "{{.Names}}"); strings.Contains(hostPS, nested) {
		t.Errorf("[W3] ISOLATION BREACH: nested container %q is visible on the HOST daemon", nested)
	} else {
		t.Logf("[W3] nested workload invisible to the host daemon (isolated in the sidecar)")
	}
	runnerMounts := dockerOut(t, "inspect", runnerName, "-f", "{{json .Mounts}}")
	if strings.Contains(runnerMounts, "/var/run/docker.sock") {
		t.Errorf("[W3] runner has a host docker.sock bind: %s", runnerMounts)
	}
	dindMounts := dockerOut(t, "inspect", nrDindName(), "-f", "{{json .Mounts}}")
	if strings.Contains(dindMounts, "/var/run/docker.sock") {
		t.Errorf("[W3] sidecar has a host docker.sock bind: %s", dindMounts)
	}
	env := dockerOut(t, "inspect", runnerName, "-f", "{{range .Config.Env}}{{println .}}{{end}}")
	if strings.Contains(env, instanceToken) || strings.Contains(env, srv.URL) {
		t.Errorf("[W3] runner env leaks the instance token or metadata URL")
	}
	// Credentials are on the tmpfs, not the writable layer.
	if _, err := execRunner(runnerName, "test", "-e", spec.CredentialDir+"/.delivered"); err != nil {
		t.Errorf("[W3] credential delivery marker missing under %s", spec.CredentialDir)
	}
	t.Logf("[W3] no host socket on runner/sidecar; no token/metadata in env; creds present on the tmpfs")

	// =========================================================================
	// F4/F6 (part 1) — DeleteInstance reaps everything; repeat -> exit 30
	// =========================================================================
	_, delCode := runProvider(t, bin, configFile, controllerID, "DeleteInstance", nrInstance, nil)
	if delCode != 0 {
		t.Errorf("[F4] DeleteInstance exit=%d, want 0", delCode)
	}
	assertGone(t, "runner", "inspect", runnerName)
	assertGone(t, "sidecar", "inspect", nrDindName())
	assertGone(t, "network", "network", "inspect", nrNet())
	assertGone(t, "workspace volume", "volume", "inspect", nrWorkspace)
	assertGone(t, "socket volume", "volume", "inspect", nrSocket)
	assertGone(t, "dind-state volume", "volume", "inspect", nrDindState)
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("[F4] %d managed resources remain after delete, want 0", n)
	} else {
		t.Logf("[F4] DeleteInstance removed runner+sidecar+network+all volumes")
	}
	if _, del2 := runProvider(t, bin, configFile, controllerID, "DeleteInstance", nrInstance, nil); del2 != 30 {
		t.Errorf("[F4] repeat DeleteInstance exit=%d, want 30 (already gone)", del2)
	} else {
		t.Logf("[F4] repeat DeleteInstance correctly returned exit 30")
	}

	// =========================================================================
	// F6 (part 2) — remove ONLY the runner out of band, then delete-by-provider_id
	// must still reap the PRIVILEGED sidecar + volumes (no leak)
	// =========================================================================
	b2 := nrBootstrap(srv.URL, caBundle)
	stdout2, code2 := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &b2)
	if code2 != 0 {
		t.Fatalf("[F6] re-create exit=%d, want 0; stdout=%s", code2, stdout2)
	}
	var created2 params.ProviderInstance
	if err := json.Unmarshal([]byte(stdout2), &created2); err != nil {
		t.Fatalf("[F6] parse re-create stdout: %v", err)
	}
	if !waitForDindReady(t, nrDindName()) {
		t.Fatalf("[F6] dind daemon never became ready on re-create")
	}
	// The re-create is a NEW generation with a NEW nonce, so re-discover the
	// (differently-named) volumes by label before asserting they are reaped.
	nrWorkspace2 := volumeNameByResource(t, controllerID, "workspace")
	nrSocket2 := volumeNameByResource(t, controllerID, "socket")
	nrDindState2 := volumeNameByResource(t, controllerID, "dind-state")
	// Remove ONLY the runner container out of band, leaving the privileged
	// sidecar + network + volumes behind — exactly the leak F6 closes.
	if out, err := dockerTry("rm", "-f", runnerName); err != nil {
		t.Fatalf("[F6] out-of-band runner removal failed: %v\n%s", err, out)
	}
	if _, err := dockerTry("inspect", nrDindName()); err != nil {
		t.Fatalf("[F6] precondition: sidecar should still exist after removing only the runner: %v", err)
	}
	// DeleteInstance by the ORIGINAL provider_id (the instance name).
	_, delCode2 := runProvider(t, bin, configFile, controllerID, "DeleteInstance", created2.ProviderID, nil)
	if delCode2 != 0 {
		t.Errorf("[F6] DeleteInstance(provider_id=%q) after out-of-band runner removal exit=%d, want 0", created2.ProviderID, delCode2)
	}
	assertGone(t, "leaked sidecar", "inspect", nrDindName())
	assertGone(t, "network", "network", "inspect", nrNet())
	assertGone(t, "workspace volume", "volume", "inspect", nrWorkspace2)
	assertGone(t, "socket volume", "volume", "inspect", nrSocket2)
	assertGone(t, "dind-state volume", "volume", "inspect", nrDindState2)
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("[F6] %d managed resources leaked after delete-by-provider_id with the runner gone, want 0", n)
	} else {
		t.Logf("[F6] delete-by-provider_id reaped the privileged sidecar + network + volumes even with the runner already gone — no leak")
	}
}

// firstLine returns the first non-empty line of s (for tidy hello-world logging).
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			return strings.TrimSpace(ln)
		}
	}
	return ""
}
