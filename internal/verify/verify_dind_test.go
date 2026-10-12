//go:build dockerverify

// This file adds the M1-WP3 real-daemon verification harness (privileged-sidecar
// DinD), gated behind the same `dockerverify` build tag as the WP2 harness so it
// never runs in the normal `go test ./...` gate: it requires a live Docker
// daemon, builds/pulls images, and needs outbound internet (the DinD daemon
// pulls a nested image). Run it explicitly with:
//
//	go test -tags dockerverify -v -run TestVerifyM1WP3DindAllocation ./internal/verify/
//
// It drives the REAL provider binary through a full privileged-sidecar DinD
// allocation and asserts, with actual command output, the WP3 evidence points
// W3-a..f: the sidecar's daemon comes up; the runner reaches THAT daemon (not
// the host); a nested workload started via the runner is visible only inside the
// sidecar and invisible to the host (the core isolation red line); no host
// docker.sock is mounted; teardown removes everything (repeat delete → exit 30);
// and the storage driver dockerd actually used is reported.
//
// Everything it creates is scoped to a unique controller-id and torn down at the
// end; it snapshots host containers/networks/volumes before and after and
// asserts they are identical, so it never touches foreign resources.
package verify

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cloudbase/garm-provider-common/params"
)

// dindImageDigest is a working, digest-pinned docker:dind reference (the
// multi-arch index the real-job gate pins, Docker Engine 29.9.0 inside).
// The shipped default dind_image is an M4 release concern (ADR-001 open
// question); this is only the reference the live harness runs against.
const dindImageDigest = "docker:dind@sha256:0b6d18a4a222e71c88be6034926b4f6cde2fc33c2218643d03ccfddc65995470"

const wp3Instance = "wp3-verify-01"

// WP3 derived resource names must match internal/spec's name builders. The
// network/sidecar names are stable per instance; the volume names are
// generation-nonce-embedded (F4) and discovered by label at runtime.
func wp3Net() string      { return wp3Instance + "-net" }
func wp3DindName() string { return wp3Instance + "-dind" }

func wp3Bootstrap(metadataURL string, caBundle []byte) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name:             wp3Instance,
		RepoURL:          "https://github.com/example-org/example-repo",
		MetadataURL:      metadataURL,
		InstanceToken:    instanceToken, // shared WP2 const
		CACertBundle:     caBundle,
		OSType:           params.Linux,
		OSArch:           hostOSArch(),
		PoolID:           poolID, // shared WP2 const
		JitConfigEnabled: true,
	}
}

// buildCliRunnerImage builds a tiny runner image that has a docker CLI (so
// `docker info`/`docker run` can be exec'd inside it) and a sleep entrypoint
// (so the provider's credential-delivery exec + running-verification succeed
// without a real runner). docker:cli is alpine-based, so busybox tar handles
// the provider's `tar -x -p -C /run/garm` delivery.
func buildCliRunnerImage(t *testing.T, tag string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Dockerfile"),
		"FROM docker:cli\nENTRYPOINT [\"sleep\", \"infinity\"]\n")
	if out, err := dockerTry("build", "-t", tag, dir); err != nil {
		t.Fatalf("build cli runner image: %v\n%s", err, out)
	}
}

// snapshotIDs returns the sorted set of ids from a `docker ... -q`-style query,
// for the identical-before/after foreign-resource guarantee.
func snapshotIDs(t *testing.T, args ...string) []string {
	t.Helper()
	ids := lines(dockerOut(t, args...))
	sort.Strings(ids)
	return ids
}

func assertSnapshotIdentical(t *testing.T, label string, before []string, args ...string) {
	t.Helper()
	after := snapshotIDs(t, args...)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Errorf("[snapshot] %s changed after the run:\n  before: %v\n  after:  %v", label, before, after)
	} else {
		t.Logf("[snapshot] %s identical before/after (%d) — no foreign resource touched", label, len(after))
	}
}

// waitForDindReady polls `docker exec <dind> docker info` until the sidecar's
// daemon answers (it is running the instant CreateInstance returns, but the
// daemon takes a moment to come up; the real runner entrypoint's own
// `until docker info` wait handles this in production).
func waitForDindReady(t *testing.T, dindName string) bool {
	t.Helper()
	for i := 0; i < 60; i++ {
		if _, err := dockerTry("exec", dindName, "docker", "info"); err == nil {
			t.Logf("[a] dind daemon ready after ~%ds", i)
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

func TestVerifyM1WP3DindAllocation(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)

	// --- foreign snapshot (must be identical after) --------------------------
	canaries := ensureForeignCanaries(t)
	assertForeignPresent(t, canaries[0])
	assertForeignPresent(t, canaries[1])
	// Snapshot by NAME, not id: Docker Desktop rotates the DEFAULT bridge
	// network's internal id when its network stack reinitializes (e.g. after the
	// last user network is removed), independent of this provider — the set of
	// resource NAMES is the stable, meaningful "no foreign resource touched"
	// invariant, and the provider is label-scoped so it never touches the bridge.
	beforeC := snapshotIDs(t, "ps", "-a", "--format", "{{.Names}}")
	beforeN := snapshotIDs(t, "network", "ls", "--format", "{{.Name}}")
	beforeV := snapshotIDs(t, "volume", "ls", "--format", "{{.Name}}")
	t.Logf("[snapshot] before: %d containers, %d networks, %d volumes", len(beforeC), len(beforeN), len(beforeV))

	// --- build the docker-cli runner image + the real provider binary --------
	runnerTag := "garm-wp3-verify-cli:latest"
	buildCliRunnerImage(t, runnerTag)

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
		// M2 persistent cache off: this test verifies DinD topology/teardown in
		// isolation, and cache volumes deliberately survive DeleteInstance
		// (covered by TestVerifyM2W1CacheHit), so leaving it on would break the
		// post-delete zero-managed-resources assertion.
		`[cache]`,
		`enabled = false`,
		"",
	}, "\n"))

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	// Cleanup: label-scoped teardown for THIS controller-id (removes the sidecar
	// too, which kills its nested workload), then the foreign + snapshot checks.
	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, canaries[0])
		assertForeignPresent(t, canaries[1])
		assertSnapshotIdentical(t, "containers", beforeC, "ps", "-a", "--format", "{{.Names}}")
		assertSnapshotIdentical(t, "networks", beforeN, "network", "ls", "--format", "{{.Name}}")
		assertSnapshotIdentical(t, "volumes", beforeV, "volume", "ls", "--format", "{{.Name}}")
	}()

	// =========================================================================
	// CreateInstance — the full DinD topology
	// =========================================================================
	b := wp3Bootstrap(srv.URL, caBundle)
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
	runnerID := created.ProviderID
	t.Logf("[create] runner provider_id=%s; sidecar=%s", runnerID, wp3DindName())

	// F4: the three job-scoped volumes carry generation-nonce names, discovered
	// by resource label rather than reconstructed.
	wp3Workspace := volumeNameByResource(t, controllerID, "workspace")
	wp3Socket := volumeNameByResource(t, controllerID, "socket")
	wp3DindState := volumeNameByResource(t, controllerID, "dind-state")
	t.Logf("[create] generation-nonce volume names: workspace=%s socket=%s dind-state=%s", wp3Workspace, wp3Socket, wp3DindState)

	// Topology sanity: sidecar + both DinD volumes exist and are labeled.
	dindPriv := dockerOut(t, "inspect", wp3DindName(), "-f", "{{.HostConfig.Privileged}}")
	dindCmd := dockerOut(t, "inspect", wp3DindName(), "-f", "{{json .Config.Cmd}}")
	dindRole := dockerOut(t, "inspect", wp3DindName(), "-f", "{{index .Config.Labels \"garm.docker/role\"}}")
	t.Logf("[topology] sidecar Privileged=%s role=%s cmd=%s", dindPriv, dindRole, dindCmd)
	if dindPriv != "true" {
		t.Errorf("sidecar Privileged=%s, want true (privileged-sidecar)", dindPriv)
	}
	if !strings.Contains(dindCmd, "--storage-driver=overlay2") {
		t.Errorf("sidecar dockerd cmd missing --storage-driver=overlay2: %s", dindCmd)
	}
	for _, v := range []string{wp3Workspace, wp3Socket, wp3DindState} {
		if _, err := dockerTry("volume", "inspect", v); err != nil {
			t.Errorf("[topology] job volume %q missing: %v", v, err)
		}
	}
	// No host docker.sock on the sidecar either.
	if m := dockerOut(t, "inspect", wp3DindName(), "-f", "{{json .Mounts}}"); strings.Contains(m, "/var/run/docker.sock") {
		t.Errorf("[topology] sidecar has a host docker.sock bind: %s", m)
	}
	// Shared network namespace (ADR-001 amendment): the sidecar joins the
	// runner's namespace, and only the runner is attached to the job network.
	assertDindSharesRunnerNetns(t, runnerID, wp3DindName(), wp3Net())

	if !waitForDindReady(t, wp3DindName()) {
		t.Fatalf("(W3-a) dind daemon never became ready")
	}

	// =========================================================================
	// (W3-a) sidecar daemon up; capture the storage driver it actually used
	// =========================================================================
	dindInfo := dockerOut(t, "exec", wp3DindName(), "docker", "info",
		"--format", "Driver={{.Driver}} ServerVersion={{.ServerVersion}} Name={{.Name}} Root={{.DockerRootDir}}")
	t.Logf("[W3-a] dind `docker info`: %s", dindInfo)
	storageDriver := infoField(dindInfo, "Driver=")
	if storageDriver == "" {
		t.Errorf("(W3-a) could not read the dind storage driver from: %s", dindInfo)
	}
	dindName := infoField(dindInfo, "Name=")

	// =========================================================================
	// (W3-b) runner reaches the DIND daemon, not the host
	// =========================================================================
	runnerDockerHost := strings.TrimSpace(dockerOut(t, "exec", runnerID, "sh", "-c", "echo $DOCKER_HOST"))
	t.Logf("[W3-b] runner DOCKER_HOST=%s", runnerDockerHost)
	if runnerDockerHost != "unix:///run/docker.sock" {
		t.Errorf("(W3-b) runner DOCKER_HOST=%q, want unix:///run/docker.sock", runnerDockerHost)
	}
	runnerInfo := dockerOut(t, "exec", runnerID, "docker", "info", "--format", "Name={{.Name}} ServerVersion={{.ServerVersion}}")
	runnerSeesName := infoField(runnerInfo, "Name=")
	hostName := dockerOut(t, "info", "--format", "{{.Name}}")
	t.Logf("[W3-b] runner `docker info` -> %s ; dind Name=%s ; host Name=%s", runnerInfo, dindName, hostName)
	if runnerSeesName != dindName {
		t.Errorf("(W3-b) runner sees daemon Name=%q, want the dind Name=%q", runnerSeesName, dindName)
	}
	if runnerSeesName == hostName {
		t.Errorf("(W3-b) runner is talking to the HOST daemon (Name=%q), not the sidecar", hostName)
	}

	// =========================================================================
	// (W3-c) nesting isolation — the core red line
	// =========================================================================
	nestedName := "wp3-nested-busybox"
	if out, err := dockerTry("exec", runnerID, "docker", "run", "-d", "--name", nestedName, "busybox", "sleep", "300"); err != nil {
		t.Errorf("(W3-c) could not start a nested container via the runner: %v\n%s", err, out)
	}
	dindPS := dockerOut(t, "exec", wp3DindName(), "docker", "ps", "--format", "{{.Names}}")
	t.Logf("[W3-c] nested workload seen INSIDE the sidecar: %q", dindPS)
	if !strings.Contains(dindPS, nestedName) {
		t.Errorf("(W3-c) nested container %q not visible inside the sidecar (dind ps: %q)", nestedName, dindPS)
	}
	hostPS := dockerOut(t, "ps", "-a", "--format", "{{.Names}}")
	if strings.Contains(hostPS, nestedName) {
		t.Errorf("(W3-c) ISOLATION BREACH: nested container %q is visible on the HOST daemon", nestedName)
	} else {
		t.Logf("[W3-c] nested workload is INVISIBLE to the host daemon — fully isolated inside the sidecar")
	}

	// =========================================================================
	// (W3-d) no host socket; the runner cannot reach the host daemon
	// =========================================================================
	runnerMounts := dockerOut(t, "inspect", runnerID, "-f", "{{json .Mounts}}")
	if strings.Contains(runnerMounts, "/var/run/docker.sock") {
		t.Errorf("(W3-d) runner has a host docker.sock bind: %s", runnerMounts)
	} else {
		t.Logf("[W3-d] runner mounts carry NO host docker.sock (only the named socket volume): %s", runnerMounts)
	}

	// =========================================================================
	// (W3-g) a port the inner daemon publishes is reachable on the runner's
	// 127.0.0.1 and localhost (shared network namespace, ADR-001 amendment)
	// =========================================================================
	assertInnerPublishedPortReachable(t, runnerID, "", "wp3-published-httpd")

	// =========================================================================
	// (W3-e) teardown removes runner->dind->volumes->network (network LAST, F4);
	// repeat → exit 30
	// =========================================================================
	_, delCode := runProvider(t, bin, configFile, controllerID, "DeleteInstance", wp3Instance, nil)
	if delCode != 0 {
		t.Errorf("(W3-e) DeleteInstance exit=%d, want 0", delCode)
	}
	assertGone(t, "runner", "inspect", runnerID)
	assertGone(t, "sidecar", "inspect", wp3DindName())
	assertGone(t, "network", "network", "inspect", wp3Net())
	assertGone(t, "workspace volume", "volume", "inspect", wp3Workspace)
	assertGone(t, "socket volume", "volume", "inspect", wp3Socket)
	assertGone(t, "dind-state volume", "volume", "inspect", wp3DindState)
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("(W3-e) %d managed resources remain after delete, want 0", n)
	} else {
		t.Logf("[W3-e] DeleteInstance removed runner + sidecar + network + all 3 volumes (incl. dind-state); the nested workload died with the sidecar")
	}

	_, del2 := runProvider(t, bin, configFile, controllerID, "DeleteInstance", wp3Instance, nil)
	if del2 != 30 {
		t.Errorf("(W3-e) repeat DeleteInstance exit=%d, want 30 (already gone)", del2)
	} else {
		t.Logf("[W3-e] repeat DeleteInstance correctly returned exit 30")
	}

	// =========================================================================
	// (W3-f) storage driver report
	// =========================================================================
	t.Logf("[W3-f] dockerd inside the privileged sidecar used storage driver %q (requested overlay2); host daemon driver is %q",
		storageDriver, dockerOut(t, "info", "--format", "{{.Driver}}"))
	if storageDriver != "overlay2" {
		t.Logf("[W3-f] NOTE: overlay2 was requested but dockerd fell back to %q on this nested/emulated daemon", storageDriver)
	}
}

// infoField extracts the value following key (e.g. "Driver=") from a
// space-joined `docker info --format` line.
func infoField(line, key string) string {
	for _, f := range strings.Fields(line) {
		if strings.HasPrefix(f, key) {
			return strings.TrimPrefix(f, key)
		}
	}
	return ""
}

// assertDindSharesRunnerNetns checks the privileged-sidecar network topology
// (ADR-001, shared network namespace amendment) on the live daemon: the
// sidecar's NetworkMode is container:<runner's full ID> and it has no network
// attachment of its own, the runner is attached to exactly the job network,
// and both containers really are in one network namespace (same
// /proc/self/ns/net inode).
func assertDindSharesRunnerNetns(t *testing.T, runner, dind, jobNet string) {
	t.Helper()
	runnerFullID := dockerOut(t, "inspect", runner, "-f", "{{.Id}}")
	mode := dockerOut(t, "inspect", dind, "-f", "{{.HostConfig.NetworkMode}}")
	if mode != "container:"+runnerFullID {
		t.Errorf("[netns] sidecar NetworkMode=%q, want container:%s (the runner's namespace)", mode, runnerFullID)
	} else {
		t.Logf("[netns] sidecar NetworkMode=%s (joins the runner's namespace)", mode)
	}
	const netsFmt = "{{range $name, $_ := .NetworkSettings.Networks}}{{println $name}}{{end}}"
	if nets := lines(dockerOut(t, "inspect", dind, "-f", netsFmt)); len(nets) != 0 {
		t.Errorf("[netns] sidecar has network attachments of its own: %v, want none", nets)
	}
	if nets := lines(dockerOut(t, "inspect", runner, "-f", netsFmt)); len(nets) != 1 || nets[0] != jobNet {
		t.Errorf("[netns] runner networks=%v, want exactly [%s]", nets, jobNet)
	} else {
		t.Logf("[netns] runner is attached to exactly the job network %s", jobNet)
	}
	runnerNS := dockerOut(t, "exec", runner, "readlink", "/proc/self/ns/net")
	dindNS := dockerOut(t, "exec", dind, "readlink", "/proc/self/ns/net")
	if runnerNS == "" || runnerNS != dindNS {
		t.Errorf("[netns] runner namespace %q != sidecar namespace %q, want one shared namespace", runnerNS, dindNS)
	} else {
		t.Logf("[netns] runner and sidecar share network namespace %s", runnerNS)
	}
}

// publishedPortWait bounds how long the published-port probe retries while the
// nested httpd comes up.
const publishedPortWait = 30 * time.Second

// assertInnerPublishedPortReachable starts a nested busybox httpd through the
// runner's DOCKER_HOST with an ephemeral published port (`-p 80`, the shape
// testcontainers uses), reads the host port back with `docker port`, and
// fetches it from inside the runner via 127.0.0.1 and via localhost. With the
// sidecar in its own namespace this fails with "connection refused" (the port
// is bound in the sidecar's namespace); with the shared namespace it succeeds,
// as on a GitHub-hosted runner. user, when non-empty, runs every command
// inside the runner as that user (`docker exec -u`), as a job step would.
func assertInnerPublishedPortReachable(t *testing.T, runner, user, name string) {
	t.Helper()
	const body = "garm-published-port-ok"
	script := "mkdir -p /w && echo " + body + " > /w/index.html && exec httpd -f -p 80 -h /w"
	if out, err := dockerTry(execIn(runner, user, "docker", "run", "-d", "--name", name, "-p", "80", "busybox:1.36", "sh", "-c", script)...); err != nil {
		t.Errorf("(W3-g) could not start a nested httpd with a published port via the runner: %v\n%s", err, out)
		return
	}
	mapping := dockerOut(t, execIn(runner, user, "docker", "port", name, "80/tcp")...)
	port := publishedHostPort(mapping)
	if port == "" {
		t.Errorf("(W3-g) could not read the published host port from `docker port`: %q", mapping)
		return
	}
	t.Logf("[W3-g] nested httpd published 80/tcp as %q", mapping)
	for _, host := range []string{"127.0.0.1", "localhost"} {
		url := "http://" + host + ":" + port + "/"
		got, err := fetchFromContainer(runner, user, url, publishedPortWait)
		if err != nil || got != body {
			t.Errorf("(W3-g) runner GET %s = %q, %v; want %q (a port the inner daemon publishes must be reachable on the runner's %s)", url, got, err, body, host)
			continue
		}
		t.Logf("[W3-g] runner GET %s -> %q", url, got)
	}
}

// publishedHostPort returns the host port of the first `docker port` mapping
// line ("0.0.0.0:32768" or "[::]:32768"), or "" when there is none.
func publishedHostPort(mapping string) string {
	ls := lines(mapping)
	if len(ls) == 0 {
		return ""
	}
	i := strings.LastIndex(ls[0], ":")
	if i < 0 || i == len(ls[0])-1 {
		return ""
	}
	return ls[0][i+1:]
}

// execIn returns the `docker exec` arguments that run args inside container,
// as user when user is non-empty.
func execIn(container, user string, args ...string) []string {
	out := []string{"exec"}
	if user != "" {
		out = append(out, "-u", user)
	}
	return append(append(out, container), args...)
}

// fetchFromContainer GETs url with wget inside container (as user when
// non-empty), retrying until it answers or wait elapses, and returns the
// trimmed body.
func fetchFromContainer(container, user, url string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		out, err := dockerTry(execIn(container, user, "wget", "-qO-", "-T", "3", url)...)
		if err == nil || time.Now().After(deadline) {
			return strings.TrimSpace(out), err
		}
		time.Sleep(500 * time.Millisecond)
	}
}
