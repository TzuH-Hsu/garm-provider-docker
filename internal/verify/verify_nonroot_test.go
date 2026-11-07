//go:build dockerverify

// A DinD allocation exercised as the non-root runner user (uid/gid 1001) over a real
// workspace bind mount. A test run as root, or without that bind mount, would hide
// the permission gaps checked here. It needs a live daemon, image builds, and
// outbound internet for the nested pulls. Run with:
//
//	go test -tags dockerverify -v -run TestVerifyNonRootDindAllocation ./internal/verify/
//
// The test drives the real provider binary through a full privileged-sidecar DinD
// allocation and checks, with real command output run as the runner user:
//   - `docker info` and `docker run --rm hello-world` succeed over the shared dind
//     socket, which needs the runner user to be in the socket's group;
//   - a file written in the runner workspace is visible and modifiable through a
//     nested `docker run -v <workspace path>:/w ...`, and the reverse, so the
//     workspace is shared into the daemon;
//   - the isolation properties still hold: the nested workload is invisible to the
//     host, no host docker.sock is mounted, credentials are tmpfs-only, and no token
//     is in the env;
//   - DeleteInstance removes the runner, sidecar, volumes and network (a repeat
//     exits 30), and deleting by provider_id after the runner was removed out of
//     band still removes the privileged sidecar and volumes, so nothing leaks.
//
// Everything is scoped to a unique controller ID and torn down by label. It
// snapshots host containers, networks and volumes before and after and requires
// them to match, so it never touches foreign resources.
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

const nrInstance = "nr-verify-01"

// runnerBaseImageEnv optionally names an already-built runner image (for
// example a released ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:... digest)
// to layer the test wrapper on, instead of building runner-images/noble from
// source. Either way the image carries the production entrypoint.
const runnerBaseImageEnv = "GARM_VERIFY_RUNNER_BASE_IMAGE"

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

// buildNonRootRunnerImage builds the runner image for this test on the production
// runner image (runner-images/noble: its real Dockerfile and entrypoint.sh), so the
// entrypoint code is tested verbatim and not reimplemented with softer error
// handling. The base ships the runner user, gosu, the docker CLI and
// getent/groupadd/usermod, and its ensure_docker_socket_group is the production
// function.
//
// The only deviation is the wrapper entrypoint. It sources the real /entrypoint.sh
// (whose main runs only when executed directly) and calls the real
// ensure_docker_socket_group under that file's own `set -euo pipefail`, then sleeps
// as PID 1 (root) so the test can `docker exec -u runner` in. Production would
// `exec ./run.sh`, but the fake JIT credentials cannot drive the real runner. This
// matters: if ensure_docker_socket_group aborted under set -e on an image with no
// GID 2000, the call would abort here, the container would exit, and every exec
// below would fail.
func buildNonRootRunnerImage(t *testing.T, root, tag string) {
	t.Helper()

	// 1. Build the production runner image, or use the already-built image that
	//    runnerBaseImageEnv names (for example a released, digest-pinned image).
	nobleTag := os.Getenv(runnerBaseImageEnv)
	if nobleTag != "" {
		if _, err := dockerTry("image", "inspect", nobleTag); err != nil {
			if out, err := dockerTry("pull", nobleTag); err != nil {
				t.Fatalf("pull %s=%s: %v\n%s", runnerBaseImageEnv, nobleTag, err, out)
			}
		}
		t.Logf("[image] layering the test wrapper on %s=%s instead of building runner-images/noble", runnerBaseImageEnv, nobleTag)
	} else {
		nobleTag = "garm-nr-noble-real:latest"
		if out, err := dockerTry(append([]string{"build", "-t", nobleTag}, runnerImageBuildArgs(root, "noble")...)...); err != nil {
			t.Fatalf("build real runner-images/noble image: %v\n%s", err, out)
		}
	}

	// 2. Layer a wrapper that runs only the real ensure_docker_socket_group, sourced
	//    from the real entrypoint, then sleeps, so the test can exec as the non-root
	//    runner over the shared dind socket.
	dir := t.TempDir()
	wrapper := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"# Source the REAL production entrypoint (main is guarded not to run when\n" +
		"# sourced) and invoke the REAL ensure_docker_socket_group under its own\n" +
		"# `set -euo pipefail`: this IS the group-create code path, verbatim.\n" +
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

// execRunner runs `docker exec -u runner` in container: as the non-root runner
// user (1001) with its /etc/group memberships, including the dockersock group the
// entrypoint added. That is what makes the socket-permission check meaningful.
func execRunner(container string, args ...string) (string, error) {
	full := append([]string{"exec", "-u", "runner", container}, args...)
	return dockerTry(full...)
}

func TestVerifyNonRootDindAllocation(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)

	// Foreign snapshot, compared after the run.
	canaries := ensureForeignCanaries(t)
	assertForeignPresent(t, canaries[0])
	assertForeignPresent(t, canaries[1])
	beforeC := snapshotIDs(t, "ps", "-a", "--format", "{{.Names}}")
	beforeN := snapshotIDs(t, "network", "ls", "--format", "{{.Name}}")
	beforeV := snapshotIDs(t, "volume", "ls", "--format", "{{.Name}}")
	t.Logf("[snapshot] before: %d containers, %d networks, %d volumes", len(beforeC), len(beforeN), len(beforeV))

	runnerTag := "garm-nr-verify-cli:latest"
	buildNonRootRunnerImage(t, root, runnerTag)

	// GID 2000 (spec.DindSocketGID) must be absent in the freshly built image before
	// the entrypoint runs. The group-create path in ensure_docker_socket_group is only
	// exercised if the base image does not already declare it; if a base-image change
	// added GID 2000, the test would pass without exercising that path. `getent group
	// 2000` exits non-zero when the group is absent, which is the wanted state.
	if out, err := dockerTry("run", "--rm", "--entrypoint", "getent", runnerTag, "group", spec.DindSocketGID); err == nil {
		t.Fatalf("GID %s already exists in the freshly built image before the entrypoint runs (getent: %q); the group-create path would be silently skipped", spec.DindSocketGID, strings.TrimSpace(out))
	}
	t.Logf("GID %s absent in the freshly built image pre-entrypoint — the group-create path stays load-bearing", spec.DindSocketGID)

	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	// Provider config: privileged-sidecar with a pinned dind digest.
	configDir := t.TempDir()
	configFile := filepath.Join(configDir, "config.toml")
	writeFile(t, configFile, strings.Join([]string{
		`docker_host = "unix:///var/run/docker.sock"`,
		`runner_image = "` + runnerTag + `"`,
		`allow_unpinned_runner_image = true`,
		// The ceiling defaults to ["none"] (fail-closed, ADR-001), so a DinD mode must
		// be widened explicitly or the config would not load.
		`allowed_dind_modes = ["none", "privileged-sidecar"]`,
		`dind_mode = "privileged-sidecar"`,
		`dind_image = "` + dindImageDigest + `"`,
		`storage_driver = "overlay2"`,
		`[resources]`,
		`dind_memory = "2GiB"`,
		// The cache is off: cache volumes survive DeleteInstance (TestVerifyCacheHit
		// covers them) and would break the zero-managed-resources assertions after delete.
		`[cache]`,
		`enabled = false`,
		"",
	}, "\n"))

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, canaries[0])
		assertForeignPresent(t, canaries[1])
		assertSnapshotIdentical(t, "containers", beforeC, "ps", "-a", "--format", "{{.Names}}")
		assertSnapshotIdentical(t, "networks", beforeN, "network", "ls", "--format", "{{.Name}}")
		assertSnapshotIdentical(t, "volumes", beforeV, "volume", "ls", "--format", "{{.Name}}")
	}()

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
	// provider_id is the stable instance name, not a container ID.
	if created.ProviderID != nrInstance {
		t.Errorf("provider_id=%q, want the instance name %q", created.ProviderID, nrInstance)
	}
	runnerName := spec.RunnerContainerName(nrInstance)
	t.Logf("[create] provider_id=%s; runner container=%s; sidecar=%s", created.ProviderID, runnerName, nrDindName())

	// The three job-scoped volumes embed a create nonce, so look them up by resource
	// label.
	nrWorkspace := volumeNameByResource(t, controllerID, "workspace")
	nrSocket := volumeNameByResource(t, controllerID, "socket")
	nrDindState := volumeNameByResource(t, controllerID, "dind-state")
	t.Logf("[create] generation-nonce volume names: workspace=%s socket=%s dind-state=%s", nrWorkspace, nrSocket, nrDindState)

	// The runner carries the DinD socket GID as a supplementary group, and dockerd was
	// started with the matching --group.
	groupAdd := dockerOut(t, "inspect", runnerName, "-f", "{{json .HostConfig.GroupAdd}}")
	t.Logf("[group] runner HostConfig.GroupAdd=%s (want to include %s)", groupAdd, spec.DindSocketGID)
	if !strings.Contains(groupAdd, spec.DindSocketGID) {
		t.Errorf("[group] runner GroupAdd=%s, want it to include the DinD socket GID %s", groupAdd, spec.DindSocketGID)
	}
	dindCmd := dockerOut(t, "inspect", nrDindName(), "-f", "{{json .Config.Cmd}}")
	if !strings.Contains(dindCmd, "--group="+spec.DindSocketGID) {
		t.Errorf("[group] dind cmd=%s, want --group=%s", dindCmd, spec.DindSocketGID)
	}

	if !waitForDindReady(t, nrDindName()) {
		t.Fatalf("dind daemon never became ready")
	}

	// As the non-root runner user: `docker info` and `docker run hello-world` work.
	whoami, _ := execRunner(runnerName, "id")
	t.Logf("[group] runner-user identity: %s", strings.TrimSpace(whoami))
	if info, err := execRunner(runnerName, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Errorf("[group] `docker info` as the runner user FAILED (the permission gap): %v\n%s", err, info)
	} else {
		t.Logf("[group] `docker info` as the runner user OK; dind ServerVersion=%s", strings.TrimSpace(info))
	}
	if hw, err := execRunner(runnerName, "docker", "run", "--rm", "hello-world"); err != nil {
		t.Errorf("[group] `docker run --rm hello-world` as the runner user FAILED: %v\n%s", err, hw)
	} else {
		t.Logf("[group] `docker run --rm hello-world` as the runner user OK: %s", firstLine(hw))
	}

	// The workspace is shared with the daemon, both ways between the runner and a
	// nested container.
	// Pre-pull busybox into the dind daemon so the nested `docker run` output is
	// just the bind-mounted file content, not interleaved pull progress.
	if out, err := execRunner(runnerName, "docker", "pull", "busybox"); err != nil {
		t.Fatalf("[workspace] pre-pull busybox into the dind daemon failed: %v\n%s", err, out)
	}
	work := spec.RunnerWorkDir
	if out, err := execRunner(runnerName, "sh", "-c", "echo runner-wrote-this > "+work+"/ws.txt"); err != nil {
		t.Fatalf("[workspace] runner could not write its workspace: %v\n%s", err, out)
	}
	// A nested container bind-mounting the workspace path must see the runner's file.
	seen, err := execRunner(runnerName, "docker", "run", "--rm", "-v", work+":/w", "busybox", "cat", "/w/ws.txt")
	if err != nil {
		t.Errorf("[workspace] nested container could not read the workspace bind: %v\n%s", err, seen)
	} else if strings.TrimSpace(seen) != "runner-wrote-this" {
		t.Errorf("[workspace] nested container saw %q through the workspace bind, want %q — the daemon resolved an EMPTY path, not the runner's files", strings.TrimSpace(seen), "runner-wrote-this")
	} else {
		t.Logf("[workspace] nested container read the runner-written file through -v %s:/w", work)
	}
	// And a write from the nested container is visible back in the runner workspace.
	if out, err := execRunner(runnerName, "docker", "run", "--rm", "-v", work+":/w", "busybox", "sh", "-c", "echo nested-wrote-this > /w/ws-nested.txt"); err != nil {
		t.Errorf("[workspace] nested container could not write the workspace bind: %v\n%s", err, out)
	}
	if back, err := execRunner(runnerName, "cat", work+"/ws-nested.txt"); err != nil {
		t.Errorf("[workspace] runner could not read the nested-written file back: %v\n%s", err, back)
	} else if strings.TrimSpace(back) != "nested-wrote-this" {
		t.Errorf("[workspace] runner read %q, want the nested container's write %q", strings.TrimSpace(back), "nested-wrote-this")
	} else {
		t.Logf("[workspace] runner read back the nested-container-written file — workspace is shared both ways")
	}

	// The isolation properties still hold.
	nested := "nr-nested-busybox"
	if out, err := execRunner(runnerName, "docker", "run", "-d", "--name", nested, "busybox", "sleep", "300"); err != nil {
		t.Errorf("could not start a nested container via the runner: %v\n%s", err, out)
	}
	if hostPS := dockerOut(t, "ps", "-a", "--format", "{{.Names}}"); strings.Contains(hostPS, nested) {
		t.Errorf("[isolation] ISOLATION BREACH: nested container %q is visible on the HOST daemon", nested)
	} else {
		t.Logf("[isolation] nested workload invisible to the host daemon (isolated in the sidecar)")
	}
	runnerMounts := dockerOut(t, "inspect", runnerName, "-f", "{{json .Mounts}}")
	if strings.Contains(runnerMounts, "/var/run/docker.sock") {
		t.Errorf("[isolation] runner has a host docker.sock bind: %s", runnerMounts)
	}
	dindMounts := dockerOut(t, "inspect", nrDindName(), "-f", "{{json .Mounts}}")
	if strings.Contains(dindMounts, "/var/run/docker.sock") {
		t.Errorf("[isolation] sidecar has a host docker.sock bind: %s", dindMounts)
	}
	env := dockerOut(t, "inspect", runnerName, "-f", "{{range .Config.Env}}{{println .}}{{end}}")
	if strings.Contains(env, instanceToken) || strings.Contains(env, srv.URL) {
		t.Errorf("[isolation] runner env leaks the instance token or metadata URL")
	}
	// Credentials are on the tmpfs, not the writable layer.
	if _, err := execRunner(runnerName, "test", "-e", spec.CredentialDir+"/.delivered"); err != nil {
		t.Errorf("[isolation] credential delivery marker missing under %s", spec.CredentialDir)
	}
	t.Logf("[isolation] no host socket on runner/sidecar; no token/metadata in env; creds present on the tmpfs")

	// Shared network namespace (ADR-001): as the non-root runner user, a port the
	// inner daemon publishes is reachable on 127.0.0.1 and localhost. This is the
	// testcontainers and compose case that failed with ECONNREFUSED when the sidecar
	// had its own namespace.
	assertDindSharesRunnerNetns(t, runnerName, nrDindName(), nrNet())
	assertInnerPublishedPortReachable(t, runnerName, "runner", "nr-published-httpd")

	// DeleteInstance removes everything; a repeat exits 30.
	_, delCode := runProvider(t, bin, configFile, controllerID, "DeleteInstance", nrInstance, nil)
	if delCode != 0 {
		t.Errorf("[delete] DeleteInstance exit=%d, want 0", delCode)
	}
	assertGone(t, "runner", "inspect", runnerName)
	assertGone(t, "sidecar", "inspect", nrDindName())
	assertGone(t, "network", "network", "inspect", nrNet())
	assertGone(t, "workspace volume", "volume", "inspect", nrWorkspace)
	assertGone(t, "socket volume", "volume", "inspect", nrSocket)
	assertGone(t, "dind-state volume", "volume", "inspect", nrDindState)
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("[delete] %d managed resources remain after delete, want 0", n)
	} else {
		t.Logf("[delete] DeleteInstance removed runner+sidecar+network+all volumes")
	}
	if _, del2 := runProvider(t, bin, configFile, controllerID, "DeleteInstance", nrInstance, nil); del2 != 30 {
		t.Errorf("[delete] repeat DeleteInstance exit=%d, want 30 (already gone)", del2)
	} else {
		t.Logf("[delete] repeat DeleteInstance correctly returned exit 30")
	}

	// Remove only the runner out of band. Deleting by provider_id must still remove
	// the privileged sidecar and volumes, so nothing leaks.
	b2 := nrBootstrap(srv.URL, caBundle)
	stdout2, code2 := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &b2)
	if code2 != 0 {
		t.Fatalf("[recreate] re-create exit=%d, want 0; stdout=%s", code2, stdout2)
	}
	var created2 params.ProviderInstance
	if err := json.Unmarshal([]byte(stdout2), &created2); err != nil {
		t.Fatalf("[recreate] parse re-create stdout: %v", err)
	}
	if !waitForDindReady(t, nrDindName()) {
		t.Fatalf("[recreate] dind daemon never became ready on re-create")
	}
	// The re-create is a new generation with a new nonce, so look up the differently
	// named volumes by label before asserting they are removed.
	nrWorkspace2 := volumeNameByResource(t, controllerID, "workspace")
	nrSocket2 := volumeNameByResource(t, controllerID, "socket")
	nrDindState2 := volumeNameByResource(t, controllerID, "dind-state")
	// Remove only the runner container out of band, leaving the privileged sidecar,
	// network and volumes behind.
	if out, err := dockerTry("rm", "-f", runnerName); err != nil {
		t.Fatalf("[recreate] out-of-band runner removal failed: %v\n%s", err, out)
	}
	if _, err := dockerTry("inspect", nrDindName()); err != nil {
		t.Fatalf("[recreate] precondition: sidecar should still exist after removing only the runner: %v", err)
	}
	// DeleteInstance by the ORIGINAL provider_id (the instance name).
	_, delCode2 := runProvider(t, bin, configFile, controllerID, "DeleteInstance", created2.ProviderID, nil)
	if delCode2 != 0 {
		t.Errorf("[recreate] DeleteInstance(provider_id=%q) after out-of-band runner removal exit=%d, want 0", created2.ProviderID, delCode2)
	}
	assertGone(t, "leaked sidecar", "inspect", nrDindName())
	assertGone(t, "network", "network", "inspect", nrNet())
	assertGone(t, "workspace volume", "volume", "inspect", nrWorkspace2)
	assertGone(t, "socket volume", "volume", "inspect", nrSocket2)
	assertGone(t, "dind-state volume", "volume", "inspect", nrDindState2)
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("[recreate] %d managed resources leaked after delete-by-provider_id with the runner gone, want 0", n)
	} else {
		t.Logf("[recreate] delete-by-provider_id reaped the privileged sidecar + network + volumes even with the runner already gone — no leak")
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
