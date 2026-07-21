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

func nrNet() string       { return nrInstance + "-net" }
func nrWorkspace() string { return nrInstance + "-workspace" }
func nrSocket() string    { return nrInstance + "-socket" }
func nrDindState() string { return nrInstance + "-dind-state" }
func nrDindName() string  { return nrInstance + "-dind" }

func nrBootstrap(metadataURL string, caBundle []byte) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name:             nrInstance,
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

// buildNonRootRunnerImage builds a docker-CLI runner image that (unlike the WP3
// sleep image) has a real `runner` user at uid/gid 1001 and an entrypoint that
// mirrors the production entrypoint's ensure_docker_socket_group step: it adds
// the runner user to a group with the provider-supplied DOCKER_SOCK_GID before
// the container settles, so `docker exec -u runner` picks up that membership and
// a non-root `docker` call can reach the shared dind socket (F1). The entrypoint
// stays PID 1 as root (sleep) so the harness can `docker exec -u runner` into it.
func buildNonRootRunnerImage(t *testing.T, tag string) {
	t.Helper()
	dir := t.TempDir()
	// busybox addgroup/adduser; DindSocketGID is unlikely to collide in this
	// minimal image, so a plain create-then-add is enough.
	entry := "#!/bin/sh\n" +
		"set -eu\n" +
		"if [ -n \"${DOCKER_SOCK_GID:-}\" ]; then\n" +
		"  addgroup -g \"$DOCKER_SOCK_GID\" dockersock 2>/dev/null || true\n" +
		"  addgroup runner dockersock 2>/dev/null || true\n" +
		"fi\n" +
		// Mirror the production entrypoint's prepare_workdir: the workspace volume
		// mounts root-owned, so hand it to the runner user before it settles.
		"mkdir -p /actions-runner/_work\n" +
		"chown runner:runner /actions-runner/_work\n" +
		"exec sleep infinity\n"
	writeFile(t, filepath.Join(dir, "entrypoint.sh"), entry)
	dockerfile := "FROM docker:cli\n" +
		"RUN addgroup -g 1001 runner && adduser -D -u 1001 -G runner runner\n" +
		"COPY --chmod=0755 entrypoint.sh /entrypoint.sh\n" +
		"ENTRYPOINT [\"/entrypoint.sh\"]\n"
	writeFile(t, filepath.Join(dir, "Dockerfile"), dockerfile)
	if out, err := dockerTry("build", "-t", tag, dir); err != nil {
		t.Fatalf("build non-root runner image: %v\n%s", err, out)
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
	buildNonRootRunnerImage(t, runnerTag)

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
		`dind_mode = "privileged-sidecar"`,
		`dind_image = "` + dindImageDigest + `"`,
		`storage_driver = "overlay2"`,
		`[resources]`,
		`dind_memory = "2GiB"`,
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
	assertGone(t, "workspace volume", "volume", "inspect", nrWorkspace())
	assertGone(t, "socket volume", "volume", "inspect", nrSocket())
	assertGone(t, "dind-state volume", "volume", "inspect", nrDindState())
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
	assertGone(t, "workspace volume", "volume", "inspect", nrWorkspace())
	assertGone(t, "socket volume", "volume", "inspect", nrSocket())
	assertGone(t, "dind-state volume", "volume", "inspect", nrDindState())
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
