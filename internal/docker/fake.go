package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// FakeClient is a hand-written, in-memory implementation of Client for
// unit tests. It never talks to a real Docker daemon, so packages that
// depend only on the Client interface (internal/provider, internal/
// topology in M1+) can be table-tested without Docker installed.
type FakeClient struct {
	mu     sync.Mutex
	nextID int

	// containers is keyed by container ID.
	containers map[string]*fakeContainer

	// networks is keyed by network ID (a network also has a name, matched
	// via findNetworkByName — mirroring how containers are keyed by ID but
	// also resolvable by name).
	networks map[string]*fakeNetwork

	// volumes is keyed by volume name: unlike containers/networks, Docker
	// volumes have no separate ID — the name IS the identifier.
	volumes map[string]*fakeVolume

	// PulledImages records every ref passed to ImagePull, in call order,
	// so tests can assert what (and how many times) was pulled.
	PulledImages []string

	// PresentImages is the set of image refs ImageInspectWithRaw treats as
	// already present locally. A ref not in this set is reported NotFound,
	// which is how CreateInstance's pull-if-missing branch is exercised.
	PresentImages map[string]bool

	// PullErr, when non-nil, is returned by ImagePull instead of pulling —
	// used to exercise the pull-failure creation-guard path.
	PullErr error

	// ExecErr, when non-nil, is returned by ExecStream instead of running —
	// used to exercise the exec-delivery-failure creation-guard path.
	ExecErr error

	// CreateErr, when non-nil, is returned by ContainerCreate. By default no
	// container is recorded (a clean create failure). When
	// CreateErrLeaksContainer is also true, the container IS recorded before
	// the error is returned, modeling the ambiguous daemon case where a
	// create errors but a container may nonetheless exist (ADR-004 F6).
	CreateErr               error
	CreateErrLeaksContainer bool

	// ExecExitCode is the exit code ExecStream reports (default 0). A
	// non-zero value models a credential-delivery command that ran but
	// failed, and suppresses the tmpfs write.
	ExecExitCode int

	// StartErr, when non-nil, is returned by ContainerStart instead of
	// starting — used to exercise the start-failure creation-guard path.
	StartErr error

	// InspectErr, when non-nil, is returned by ContainerInspect instead of
	// inspecting — used to exercise the resolver's non-NotFound error path.
	InspectErr error

	// RemoveErr, when non-nil, is returned by ContainerRemove instead of
	// removing — used to exercise delete/teardown error handling.
	RemoveErr error

	// NetworkCreateErr, when non-nil, is returned by NetworkCreate instead of
	// creating — used to exercise the claim-marker create-failure path. By
	// default no network is recorded (a clean failure). When
	// NetworkCreateErrLeaks is also true, the network IS recorded before the
	// error is returned, modeling the ambiguous daemon case where a create
	// errors but a network may nonetheless exist (mirroring
	// CreateErr/CreateErrLeaksContainer for containers).
	NetworkCreateErr      error
	NetworkCreateErrLeaks bool

	// NetworkRemoveErr, when non-nil, is returned by NetworkRemove instead of
	// removing — used to exercise teardown error joining for networks.
	NetworkRemoveErr error

	// VolumeCreateErr, when non-nil, is returned by VolumeCreate instead of
	// creating — used to exercise the workspace-volume create-failure path of
	// the creation guard (net-created-then-vol-fails).
	VolumeCreateErr error

	// VolumeRemoveErr, when non-nil, is returned by VolumeRemove instead of
	// removing — used to exercise teardown error joining for volumes.
	VolumeRemoveErr error

	// Execs records every ExecStream call, in order, so tests can assert the
	// credential archive was streamed to the right container with the right
	// command.
	Execs []ExecRecord

	// Created records the SHAPE of every ContainerCreate call, in order, and
	// PERSISTS even after the container is removed — unlike the live containers
	// map, which drops a removed container. It is how a test asserts the shape of
	// a short-lived run-to-completion helper (the M2-W2 externals seeder /
	// diag-log pruner, which force-remove themselves in a defer) after
	// CreateInstance/GC returns.
	Created []CreatedContainer

	// BatchExitCode is the exit status ContainerWait reports for a
	// run-to-completion helper (default 0). A non-zero value models a seed/prune
	// helper that ran but failed, so a test can exercise the seed-failure
	// creation-guard path.
	BatchExitCode int

	// BatchWaitErr, when non-nil, is delivered on ContainerWait's error channel
	// instead of a status — modeling a daemon-side wait failure.
	BatchWaitErr error

	// RemoveOrder records every successful resource removal, in order, as
	// "container:<id>", "volume:<name>", or "network:<id>", so a test can
	// assert the ADR-004 network-last teardown ordering (F4): containers, then
	// volumes, then the network.
	RemoveOrder []string

	// CreateHook, when non-nil, is invoked at the very start of every
	// ContainerCreate — before the name-uniqueness check and before f.mu is
	// taken — so a test can model a concurrent CreateInstance that occupied a
	// container name in the window between another caller's duplicate check and
	// its own create (NEW-2). It must not re-enter this same ContainerCreate
	// call reentrantly without guarding (it runs without f.mu held).
	CreateHook func()

	// VolumeListHook, when non-nil, is invoked at the very start of every
	// VolumeList — before f.mu is taken — so a test can model a concurrent
	// mutation that lands exactly when a teardown enumerates volumes: e.g. a
	// peer teardown finishing generation A and a fresh CreateInstance claiming
	// generation B, in the window between a teardown capturing generation A's
	// nonce and its per-kind volume removal (F4). Like CreateHook it runs
	// without f.mu held (so it may call back into the fake's own locked methods)
	// and must guard against re-entrancy/once-ness itself.
	VolumeListHook func()

	// VolumeInspectHook, when non-nil, is invoked at the very start of every
	// VolumeInspect — before f.mu is taken — with the volume NAME being inspected.
	// It is the seam for the M2-W2 H3 stale-name TOCTOU: a test can model a
	// concurrent remove/recreate-under-the-same-name landing exactly BETWEEN a
	// GC's label-scoped VolumeList snapshot and its RE-INSPECT-before-remove of a
	// candidate, then assert the re-inspect sees the replacement's (foreign) labels
	// and skips the removal — proving the GC never deletes a same-name volume that
	// is no longer the stale cache it snapshotted. Like the other hooks it runs
	// without f.mu held (so it may call back into the fake's own locked methods)
	// and must guard re-entrancy/once-ness itself.
	VolumeInspectHook func(name string)

	// VolumeInspectErrHook, when non-nil, is consulted at the start of every
	// VolumeInspect (after VolumeInspectHook) with the volume NAME; if it returns
	// a non-nil error, VolumeInspect returns that error WITHOUT touching the store.
	// It models a TRANSIENT inspect failure on a specific volume — a context
	// cancellation or a transient daemon error — targeted by name, so a test can
	// prove the create path's post-create cache revalidation fails CLOSED without
	// authorizing deletion of the still-present warm cache (NEW-H1). Distinct from
	// removing the volume (which would be a NotFound, i.e. genuinely gone) — this
	// leaves the volume in the store while making its inspect fail transiently.
	VolumeInspectErrHook func(name string) error

	// VolumeRemoveHook, when non-nil, is invoked at the very start of every
	// VolumeRemove — before f.mu is taken — with the volume NAME the caller is
	// about to remove. It is the destructive-boundary seam for the F4
	// generation-unique-names interleave (ADR-004 amendment 2026-07-21): a test
	// can swap in generation B's freshly-created (differently-named) volume
	// AFTER a teardown's label-scoped VolumeList snapshot but BEFORE the teardown
	// physically removes the volume it read from that snapshot, then assert B's
	// volume is untouched — proving the removal can never name-collide across
	// generations because the names differ by construction. VolumeListHook fires
	// on the LIST; this fires on the REMOVE, the precise instant the old
	// name-based TOCTOU would have struck. Like the other hooks it runs without
	// f.mu held (so it may call back into the fake's own locked methods) and must
	// guard re-entrancy/once-ness itself.
	VolumeRemoveHook func(name string)
}

// ExecRecord captures one ExecStream call for test assertions.
type ExecRecord struct {
	ContainerID string
	Cmd         []string
	Stdin       []byte
}

// CreatedContainer captures the shape of one ContainerCreate call, retained
// across a later removal so a test can assert a short-lived helper's config.
type CreatedContainer struct {
	Name        string
	Labels      map[string]string
	Entrypoint  []string
	Cmd         []string
	Mounts      []mount.Mount
	NetworkMode container.NetworkMode
}

type fakeContainer struct {
	id     string
	name   string
	image  string
	labels map[string]string
	env    []string

	// tmpfs models the container's credential tmpfs, keyed by the filename
	// extracted into it. Only ExecStream (docker exec) writes here —
	// modeling that exec-delivered writes ARE visible inside the container,
	// unlike docker cp (see ExecStream's regression note).
	tmpfs map[string]string

	// tmpfsMounts models the container's configured tmpfs MOUNTS — the
	// HostConfig.Tmpfs short-syntax map (path → option string, e.g.
	// "/run/garm" → "…,mode=0700,uid=1001,gid=1001"). It is the mount
	// configuration, distinct from tmpfs above which is the delivered file
	// contents. Recorded at ContainerCreate so tests (and TmpfsMounts) can
	// assert the credential tmpfs was requested with the runner uid/gid — the
	// exact thing the real daemon honors for the short syntax but rejects for
	// the Mounts long syntax (see rejectUnsupportedTmpfsMount).
	tmpfsMounts map[string]string

	// networkMode models HostConfig.NetworkMode — the user-defined per-job
	// network the runner joins as its sole attachment (ADR-001). Recorded at
	// ContainerCreate so tests (and NetworkRemove's active-endpoint check) can
	// tell which network a container is attached to, and so ContainerInspect
	// reports it under NetworkSettings.Networks, mirroring the real daemon.
	networkMode string

	// mounts models HostConfig.Mounts — the named/anonymous workspace volume
	// (ADR-001), plus WP3's shared socket volume and the DinD sidecar's
	// dind-state volume. Recorded at ContainerCreate so ContainerInspect
	// reports them under .Mounts, letting a test assert the workspace volume
	// is mounted at the runner workdir (the WP9-flagged JIT-workdir contract)
	// and the socket/dind-state volumes at their DinD paths.
	mounts []mount.Mount

	// cmd, privileged, and runtime model the container.Config.Cmd and the two
	// HostConfig fields the DinD sidecar sets (ADR-001): the `dockerd
	// --host=... --storage-driver=...` argv, Privileged (true for
	// privileged-sidecar, false for sysbox-runc), and Runtime ("" default, or
	// "sysbox-runc"). Recorded at ContainerCreate and surfaced by
	// ContainerInspect so a test can assert the sidecar was built with the
	// right storage driver, privilege, and runtime — the exact real-daemon
	// contract WP3's live verification also checks.
	cmd        []string
	privileged bool
	runtime    string

	// groupAdd models HostConfig.GroupAdd — the supplementary groups the
	// runner is given so its unprivileged user can reach the shared DinD socket
	// (F1). Recorded at ContainerCreate and surfaced by ContainerInspect so a
	// test can assert the DinD socket GID was added.
	groupAdd []string

	// rawRuntime is the Runtime EXACTLY as the create request set it — "" for
	// privileged-sidecar (no explicit runtime), "sysbox-runc" for sysbox-runc.
	// It is preserved separately from what ContainerInspect reports because the
	// real daemon NORMALIZES an omitted runtime to the daemon default in
	// inspect output (F13); tests that assert on the create request use
	// RawRuntime, tests that assert on inspect see the normalized value.
	rawRuntime string

	// state is the Docker state string: created, running, exited, or dead.
	// It drives both the Running bool and the status a caller maps from.
	state     string
	oomKilled bool

	// finishedAt models State.FinishedAt — the moment the daemon recorded the
	// container exiting (F8). ContainerStop sets it to now; SetFinishedAt lets
	// a test place it in the past to exercise the exited-runner sweep grace,
	// which is measured from this timestamp, not from allocation creation. Zero
	// until the container has stopped.
	finishedAt time.Time
}

// fakeNetwork models an in-memory Docker network.
type fakeNetwork struct {
	id     string
	name   string
	labels map[string]string
}

// fakeVolume models an in-memory Docker volume. Volumes have no ID distinct
// from their name (see FakeClient.volumes).
type fakeVolume struct {
	name   string
	labels map[string]string
}

// NewFakeClient constructs an empty FakeClient.
func NewFakeClient() *FakeClient {
	return &FakeClient{
		containers:    make(map[string]*fakeContainer),
		networks:      make(map[string]*fakeNetwork),
		volumes:       make(map[string]*fakeVolume),
		PresentImages: make(map[string]bool),
	}
}

var _ Client = (*FakeClient)(nil)

// find resolves a container by ID first, then by exact (case-sensitive)
// Docker name, mirroring how the real daemon's by-ID endpoints also accept a
// container name. Returns nil when neither matches. Callers hold f.mu.
func (f *FakeClient) find(idOrName string) *fakeContainer {
	if c, ok := f.containers[idOrName]; ok {
		return c
	}
	return f.findByName(idOrName)
}

// findByName resolves a container by its exact (case-sensitive) Docker name.
// Returns nil when none matches. Callers hold f.mu. Used both by find and by
// ContainerCreate's name-uniqueness check (NEW-5).
func (f *FakeClient) findByName(name string) *fakeContainer {
	for _, c := range f.containers {
		if c.name == name {
			return c
		}
	}
	return nil
}

// ImagePull records refStr and returns an already-closed empty reader —
// there is no real image content to stream in the fake. When PullErr is
// set it fails instead, and also marks the image present on success so a
// subsequent ImageInspectWithRaw reflects the pull.
func (f *FakeClient) ImagePull(_ context.Context, refStr string, _ image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.PulledImages = append(f.PulledImages, refStr)
	if f.PullErr != nil {
		return nil, f.PullErr
	}
	f.PresentImages[refStr] = true
	return io.NopCloser(strings.NewReader("")), nil
}

// ImageInspectWithRaw reports whether refStr is in PresentImages, returning
// an errdefs.IsNotFound-satisfying error otherwise (matching the real SDK's
// behavior for an absent image). The returned .ID models the real daemon's
// content-addressable image ID — a "sha256:<hex>" derived deterministically
// from the ref (fakeImageID), NOT the ref string itself — so a caller that
// keys on the image digest (M2-W2's externals cache) gets a stable, Docker-
// name-safe token that differs across refs and agrees across inspects of the
// same ref, exactly as the real daemon does.
func (f *FakeClient) ImageInspectWithRaw(_ context.Context, imageID string) (types.ImageInspect, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.PresentImages[imageID] {
		return types.ImageInspect{}, nil, notFoundf("image %s not found", imageID)
	}
	return types.ImageInspect{ID: fakeImageID(imageID)}, nil, nil
}

// fakeImageID models the real daemon's content-addressable image ID: a stable
// "sha256:<hex>" over the ref. The real daemon reports a config digest here
// (not the ref); modeling that shape keeps the fake faithful for callers that
// strip the algorithm prefix and use the hex as a name-safe cache key.
func fakeImageID(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ExecStream models docker exec with stdin streaming (the credential
// delivery channel, ADR-002 F1). It records the call, validates the modeled
// command shape (a `tar -x … -C /run/garm` extraction), and — on a well-
// formed archive — extracts the streamed tar into the container's tmpfs model,
// modeling that a process started by docker exec runs in the container's own
// mount namespace, so files it writes into the tmpfs ARE visible. A malformed
// archive reports a non-zero exit like `tar -x` itself would, instead of
// silently succeeding (NEW-5).
//
// Regression note: this is deliberately the ONLY way the fake writes into a
// container's tmpfs. There is no CopyToContainer: docker cp CANNOT write
// into a running container's user tmpfs, because Docker resolves archive
// paths in a separate filesystem view that excludes user tmpfs mounts
// (moby v27.5.1 daemon/containerfs_linux.go). Modeling cp as working here is
// exactly the bug (F1) that shipped green unit tests but would fail on a
// real daemon, so cp is not modeled at all.
func (f *FakeClient) ExecStream(_ context.Context, containerID string, cmd []string, stdin io.Reader) (int, error) {
	var data []byte
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return 0, err
		}
		data = b
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ExecErr != nil {
		return 0, f.ExecErr
	}
	c := f.find(containerID)
	if c == nil {
		return 0, notFoundf("container %s not found", containerID)
	}
	// exec, like the real daemon, requires a running container.
	if c.state != "running" {
		return 0, fmt.Errorf("cannot exec in container %s: not running", containerID)
	}

	f.Execs = append(f.Execs, ExecRecord{
		ContainerID: c.id,
		Cmd:         append([]string(nil), cmd...),
		Stdin:       data,
	})

	// A test that forces a specific exit code models a command that ran but
	// failed (e.g. tar exiting non-zero); honor it and write nothing.
	if f.ExecExitCode != 0 {
		return f.ExecExitCode, nil
	}

	// The only command this fake models is the credential-delivery
	// `tar -x … -C /run/garm`. A different shape is a caller/contract bug the
	// fake should surface loudly rather than pretend to run.
	if !isCredentialTarExtract(cmd) {
		return 0, fmt.Errorf("fake ExecStream only models `tar -x -C %s`, got %v", credentialTarTargetDir, cmd)
	}

	// A malformed archive makes real `tar -x` exit non-zero; model that
	// instead of silently succeeding (NEW-5).
	if err := extractTarIntoTmpfs(c, data); err != nil {
		return tarFailureExitCode, nil
	}
	return 0, nil
}

// credentialTarTargetDir mirrors spec.CredentialDir (ADR-002's credential
// tmpfs). It is duplicated here rather than imported to keep package docker
// free of a dependency on package spec. The provider's CreateInstance happy-
// path tests drive the real delivery command (spec.CredentialDir) through
// this fake end to end, so a drift between the two would fail those tests.
const credentialTarTargetDir = "/run/garm"

// tarFailureExitCode is the non-zero code the fake reports when the streamed
// archive is not a valid tar, modeling `tar -x` failing.
const tarFailureExitCode = 2

// fakeDefaultRuntime is the daemon-default OCI runtime the fake reports for a
// container created without an explicit Runtime (F13): the real daemon's
// inspect output shows "runc", not the empty create-request value. RawRuntime
// exposes the un-normalized create request for tests that assert on it.
const fakeDefaultRuntime = "runc"

// expectedCredentialTarExtractCmd is the exact argv the provider's
// credentialDeliverCmd (internal/provider/create.go) builds for credential
// delivery: `tar -x -p -C /run/garm`.
var expectedCredentialTarExtractCmd = []string{"tar", "-x", "-p", "-C", credentialTarTargetDir}

// isCredentialTarExtract reports whether cmd is EXACTLY the modeled
// credential-delivery command. This is an exact-argv match rather than a
// "contains -x and -C <dir> somewhere" scan: a looser check would also
// accept degenerate shapes it was never meant to (extra/reordered
// arguments, a missing `-p`, or a `-C /run/garm` that just happens to
// appear alongside an unrelated `-x` flag elsewhere in the argv), silently
// modeling something other than what the provider actually runs (NEW-5).
func isCredentialTarExtract(cmd []string) bool {
	return slices.Equal(cmd, expectedCredentialTarExtractCmd)
}

// extractTarIntoTmpfs decodes the streamed tar and records each entry in the
// container's tmpfs model. It returns an error when the archive is malformed
// so ExecStream can report a non-zero exit, matching `tar -x`'s own behavior.
func extractTarIntoTmpfs(c *fakeContainer, data []byte) error {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err // malformed archive
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil { //nolint:gosec // bounded test input
			return err
		}
		if c.tmpfs == nil {
			c.tmpfs = map[string]string{}
		}
		c.tmpfs[hdr.Name] = buf.String()
	}
}

// Tmpfs returns a copy of a container's modeled credential-tmpfs contents,
// keyed by filename, so tests can assert what a docker-exec delivery wrote. The
// container is resolved by ID OR name (mirroring ContainerInspect), so a test
// can look it up by the instance's provider_id (now the instance name, F6) via
// its lowercased Docker name.
func (f *FakeClient) Tmpfs(containerID string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := f.find(containerID)
	if c == nil {
		return nil
	}
	out := make(map[string]string, len(c.tmpfs))
	for k, v := range c.tmpfs {
		out[k] = v
	}
	return out
}

// TmpfsMounts returns a copy of a container's configured tmpfs MOUNTS (the
// HostConfig.Tmpfs short-syntax map: path → option string), so tests can
// assert the credential tmpfs was requested at /run/garm with the runner
// uid/gid and mode 0700 — the short-syntax config the real daemon honors.
func (f *FakeClient) TmpfsMounts(containerID string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := f.find(containerID)
	if c == nil {
		return nil
	}
	out := make(map[string]string, len(c.tmpfsMounts))
	for k, v := range c.tmpfsMounts {
		out[k] = v
	}
	return out
}

// rejectUnsupportedTmpfsMount mirrors the real Docker daemon's validation of a
// Mounts long-syntax tmpfs entry (mount.Mount{Type: tmpfs, TmpfsOptions}): the
// daemon does NOT implement the uid/gid tmpfs options for the long syntax and
// rejects such a create with `invalid mount config for type "tmpfs": invalid
// option: uid`. Modeling that rejection here is the regression guard for defect
// 1 — a future revert to the long syntax fails a unit test instead of only
// failing on a real daemon. The supported path (HostConfig.Tmpfs short syntax)
// is not a Mounts entry and passes this check untouched.
func rejectUnsupportedTmpfsMount(hostConfig *container.HostConfig) error {
	if hostConfig == nil {
		return nil
	}
	for _, m := range hostConfig.Mounts {
		if m.Type != mount.TypeTmpfs || m.TmpfsOptions == nil {
			continue
		}
		for _, opt := range m.TmpfsOptions.Options {
			if len(opt) == 0 {
				continue
			}
			switch opt[0] {
			case "uid", "gid":
				return fmt.Errorf("invalid mount config for type %q: invalid option: %s", "tmpfs", opt[0])
			}
		}
	}
	return nil
}

// ContainerCreate creates an in-memory container record in the "created,
// not started" state.
func (f *FakeClient) ContainerCreate(_ context.Context, cfg *container.Config, hostConfig *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, containerName string) (container.CreateResponse, error) {
	// Fire the concurrency hook (if any) before taking the lock, so a test can
	// inject a same-name "winner" container that this call then collides with
	// (NEW-2). It runs without f.mu held to avoid a re-entrant deadlock.
	if f.CreateHook != nil {
		f.CreateHook()
	}

	// Reject an unsupported tmpfs mount config exactly like the real daemon,
	// BEFORE anything is recorded or any injected error is consulted — a real
	// daemon rejects a malformed request outright. This is the regression guard
	// for defect 1: the Mounts long-syntax tmpfs carrying uid/gid Options is NOT
	// implemented by the daemon and must fail the create; the supported path is
	// the HostConfig.Tmpfs short-syntax map. Modeling the long syntax as
	// succeeding is exactly the bug that shipped green unit tests but failed on
	// a real daemon.
	if err := rejectUnsupportedTmpfsMount(hostConfig); err != nil {
		return container.CreateResponse{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Clean create failure: nothing is recorded.
	if f.CreateErr != nil && !f.CreateErrLeaksContainer {
		return container.CreateResponse{}, f.CreateErr
	}

	// Docker-name uniqueness: the real daemon rejects a create whose name is
	// already in use with a 409 Conflict (NEW-5). Model that here, unless an
	// explicit CreateErr is already dictating this call's outcome.
	if f.CreateErr == nil && containerName != "" && f.findByName(containerName) != nil {
		return container.CreateResponse{}, conflictf("Conflict. The container name %q is already in use", "/"+containerName)
	}

	f.nextID++
	id := "fake-" + strconv.Itoa(f.nextID)

	name := containerName
	if name == "" {
		name = id
	}

	c := &fakeContainer{
		id:     id,
		name:   name,
		labels: map[string]string{},
		state:  "created",
	}
	if cfg != nil {
		c.image = cfg.Image
		c.env = append([]string(nil), cfg.Env...)
		c.cmd = append([]string(nil), cfg.Cmd...)
		for k, v := range cfg.Labels {
			c.labels[k] = v
		}
	}
	// Record the requested tmpfs MOUNTS (HostConfig.Tmpfs short syntax) so a
	// test can assert the credential tmpfs was created with the runner uid/gid
	// and mode. Any long-syntax tmpfs was already rejected above.
	if hostConfig != nil && len(hostConfig.Tmpfs) > 0 {
		c.tmpfsMounts = make(map[string]string, len(hostConfig.Tmpfs))
		for k, v := range hostConfig.Tmpfs {
			c.tmpfsMounts[k] = v
		}
	}
	// Record the network attachment (HostConfig.NetworkMode) and volume mounts
	// (HostConfig.Mounts) so ContainerInspect reports them, mirroring the real
	// daemon, and so NetworkRemove can enforce the active-endpoint rule.
	if hostConfig != nil {
		c.networkMode = string(hostConfig.NetworkMode)
		c.mounts = append([]mount.Mount(nil), hostConfig.Mounts...)
		c.privileged = hostConfig.Privileged
		c.runtime = hostConfig.Runtime
		c.rawRuntime = hostConfig.Runtime
		c.groupAdd = append([]string(nil), hostConfig.GroupAdd...)
	}
	// Model the real daemon AUTO-CREATING any referenced NAMED volume that does
	// not exist (verified on Docker Engine 29.6.1: `docker create -v missing:/x`
	// creates `missing` as an UNLABELED local volume). This is the M2-W2 H3
	// failure mode: if a concurrent GC evicted a cache volume in the window
	// between the provider ensuring/seeding it and this create referencing it, the
	// daemon silently materializes an EMPTY, UNLABELED replacement here rather than
	// failing — so the runner would mount an empty (read-only externals) tree and a
	// GC-invisible orphan is left behind. Modeling it UNLABELED is what lets the
	// provider's post-create revalidation detect and fail closed in unit tests.
	for _, m := range c.mounts {
		if m.Type == mount.TypeVolume && m.Source != "" {
			if _, ok := f.volumes[m.Source]; !ok {
				f.volumes[m.Source] = &fakeVolume{name: m.Source, labels: map[string]string{}}
			}
		}
	}
	f.containers[id] = c

	// Record the create SHAPE, retained across a later removal, so a test can
	// assert a short-lived helper's config after it force-removes itself.
	rec := CreatedContainer{Name: name, Labels: cloneLabels(c.labels)}
	if cfg != nil {
		rec.Entrypoint = append([]string(nil), cfg.Entrypoint...)
		rec.Cmd = append([]string(nil), cfg.Cmd...)
	}
	if hostConfig != nil {
		rec.Mounts = append([]mount.Mount(nil), hostConfig.Mounts...)
		rec.NetworkMode = hostConfig.NetworkMode
	}
	f.Created = append(f.Created, rec)

	// Ambiguous create failure: the container was recorded, but the call
	// still reports an error (ADR-004 F6).
	if f.CreateErr != nil {
		return container.CreateResponse{ID: id}, f.CreateErr
	}

	return container.CreateResponse{ID: id}, nil
}

// ContainerStart marks a previously created container as running.
//
// It models the real daemon rejecting a start whose prerequisites were removed
// out from under it (F5): starting a container attached to a user-defined
// network that no longer exists fails ("network … not found"), and so does
// starting one whose required NAMED volume is gone. This is the class of bug
// where a concurrent sweep deleted an in-flight allocation's network/volume and
// the create's ContainerStart then "succeeded" anyway in a unit test while
// failing on the live daemon; modeling the failure makes it fail in units too.
func (f *FakeClient) ContainerStart(_ context.Context, containerID string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.StartErr != nil {
		return f.StartErr
	}
	c := f.find(containerID)
	if c == nil {
		return notFoundf("container %s not found", containerID)
	}
	if isUserDefinedNetwork(c.networkMode) && f.findNetworkByName(c.networkMode) == nil && f.networks[c.networkMode] == nil {
		return fmt.Errorf("failed to start container %s: network %s not found", containerID, c.networkMode)
	}
	for _, m := range c.mounts {
		if m.Type == mount.TypeVolume && m.Source != "" {
			if _, ok := f.volumes[m.Source]; !ok {
				return fmt.Errorf("failed to start container %s: volume %s not found", containerID, m.Source)
			}
		}
	}
	c.state = "running"
	return nil
}

// ContainerStop marks a container as exited.
func (f *FakeClient) ContainerStop(_ context.Context, containerID string, _ container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := f.find(containerID)
	if c == nil {
		return notFoundf("container %s not found", containerID)
	}
	c.state = "exited"
	c.finishedAt = time.Now()
	return nil
}

// SetState forces a container's Docker state (created, running, exited,
// dead) and OOMKilled flag, so status-mapping paths that a plain
// start/stop cannot reach (dead, OOM-killed) are testable end-to-end. A
// transition into a terminal (exited/dead) state stamps FinishedAt with now if
// it is not already set, so status-mapping tests get a plausible timestamp.
func (f *FakeClient) SetState(containerID, state string, oomKilled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c, ok := f.containers[containerID]; ok {
		c.state = state
		c.oomKilled = oomKilled
		if (state == "exited" || state == "dead") && c.finishedAt.IsZero() {
			c.finishedAt = time.Now()
		}
	}
}

// SetFinishedAt places a container's State.FinishedAt at a specific instant, so
// a test can drive the exited-runner sweep grace (F8), which is measured from
// this timestamp rather than from allocation creation.
func (f *FakeClient) SetFinishedAt(containerID string, finishedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c, ok := f.containers[containerID]; ok {
		c.finishedAt = finishedAt
	}
}

// ContainerInspect returns the recorded state of containerID.
func (f *FakeClient) ContainerInspect(_ context.Context, containerID string) (types.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.InspectErr != nil {
		return types.ContainerJSON{}, f.InspectErr
	}
	c := f.find(containerID)
	if c == nil {
		return types.ContainerJSON{}, notFoundf("container %s not found", containerID)
	}
	return c.toContainerJSON(), nil
}

// ContainerRemove deletes containerID (by ID or name) from the fake's
// in-memory store.
func (f *FakeClient) ContainerRemove(_ context.Context, containerID string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.RemoveErr != nil {
		return f.RemoveErr
	}
	c := f.find(containerID)
	if c == nil {
		return notFoundf("container %s not found", containerID)
	}
	delete(f.containers, c.id)
	f.RemoveOrder = append(f.RemoveOrder, "container:"+c.id)
	return nil
}

// ContainerList returns every container whose labels match options.Filters
// (a "label" filter, exactly as the real daemon evaluates it).
func (f *FakeClient) ContainerList(_ context.Context, options container.ListOptions) ([]types.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []types.Container
	for _, c := range f.containers {
		if !options.Filters.MatchKVList("label", c.labels) {
			continue
		}
		out = append(out, c.toContainerSummary())
	}
	return out, nil
}

// ContainerWait models the moby SDK's two-channel run-to-completion wait for
// M2-W2's helper containers (the externals seeder / diag-log pruner). It
// transitions a running (or created) container to "exited" — modeling the batch
// job running to completion — and delivers the configured BatchExitCode on the
// response channel; BatchWaitErr, or a missing container, is delivered on the
// error channel instead. Both channels are buffered so the send never blocks,
// matching the real SDK (whose caller selects on the two channels).
func (f *FakeClient) ContainerWait(_ context.Context, containerID string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	statusCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.BatchWaitErr != nil {
		errCh <- f.BatchWaitErr
		return statusCh, errCh
	}
	c := f.find(containerID)
	if c == nil {
		errCh <- notFoundf("container %s not found", containerID)
		return statusCh, errCh
	}
	// Model the helper running to completion: created/running → exited.
	if c.state == "running" || c.state == "created" {
		c.state = "exited"
		if c.finishedAt.IsZero() {
			c.finishedAt = time.Now()
		}
	}
	statusCh <- container.WaitResponse{StatusCode: int64(f.BatchExitCode)}
	return statusCh, errCh
}

// findNetwork resolves a network by ID first, then by exact Docker name,
// mirroring find/findByName for containers. Returns nil when neither
// matches. Callers hold f.mu.
func (f *FakeClient) findNetwork(idOrName string) *fakeNetwork {
	if n, ok := f.networks[idOrName]; ok {
		return n
	}
	return f.findNetworkByName(idOrName)
}

// findNetworkByName resolves a network by its exact name. Returns nil when
// none matches. Callers hold f.mu.
func (f *FakeClient) findNetworkByName(name string) *fakeNetwork {
	for _, n := range f.networks {
		if n.name == name {
			return n
		}
	}
	return nil
}

// NetworkCreate models the real daemon's network create, including its
// name-uniqueness enforcement: a live daemon (Docker Engine 29.6.1/API
// 1.55, checked while building this interface) rejects a second
// `docker network create` reusing an in-use name with a 409 Conflict
// ("network with name %q already exists"), unconditionally — this is not
// an opt-in check the caller can skip (see client.go's doc comment on
// NetworkCreate). Modeling creation as silently succeeding for a
// colliding name would hide exactly the kind of real-daemon-divergent bug
// the M0 tmpfs-uid/gid lesson calls out.
func (f *FakeClient) NetworkCreate(_ context.Context, name string, options network.CreateOptions) (network.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Clean injected failure: nothing is recorded.
	if f.NetworkCreateErr != nil && !f.NetworkCreateErrLeaks {
		return network.CreateResponse{}, f.NetworkCreateErr
	}

	if f.findNetworkByName(name) != nil {
		return network.CreateResponse{}, conflictf("network with name %q already exists", name)
	}

	f.nextID++
	id := "fake-net-" + strconv.Itoa(f.nextID)
	f.networks[id] = &fakeNetwork{id: id, name: name, labels: cloneLabels(options.Labels)}

	// Ambiguous injected failure: the network was recorded, but the call still
	// reports an error (mirrors ContainerCreate's CreateErrLeaksContainer).
	if f.NetworkCreateErr != nil {
		return network.CreateResponse{ID: id}, f.NetworkCreateErr
	}
	return network.CreateResponse{ID: id}, nil
}

// NetworkRemove deletes a network (by ID or name) from the fake's in-memory
// store, tolerating "not found" like ContainerRemove.
//
// It models the real daemon's active-endpoint rule: `docker network rm` fails
// with a 403 ("has active endpoints") while any container — even a stopped one
// — is still attached to the network. Modeling that here makes the ADR-004
// teardown ordering (remove containers BEFORE their network) a tested
// invariant, not merely a real-daemon-only one: a teardown that tried to
// remove the network first would fail this check in a unit test, the same way
// it fails on the live daemon.
func (f *FakeClient) NetworkRemove(_ context.Context, networkID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.NetworkRemoveErr != nil {
		return f.NetworkRemoveErr
	}
	n := f.findNetwork(networkID)
	if n == nil {
		return notFoundf("network %s not found", networkID)
	}
	for _, c := range f.containers {
		if c.networkMode == n.name || c.networkMode == n.id {
			return errdefs.Forbidden(fmt.Errorf("error while removing network: network %s id %s has active endpoints", n.name, n.id))
		}
	}
	delete(f.networks, n.id)
	f.RemoveOrder = append(f.RemoveOrder, "network:"+n.id)
	return nil
}

// NetworkList returns every network whose labels match options.Filters (a
// "label" filter), exactly as ContainerList does for containers.
func (f *FakeClient) NetworkList(_ context.Context, options network.ListOptions) ([]network.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []network.Summary
	for _, n := range f.networks {
		if !options.Filters.MatchKVList("label", n.labels) {
			continue
		}
		out = append(out, network.Summary{ID: n.id, Name: n.name, Labels: cloneLabels(n.labels)})
	}
	return out, nil
}

// VolumeCreate models the real daemon's volume create — which, UNLIKE
// NetworkCreate/ContainerCreate, is idempotent on a duplicate name rather
// than a conflict: a live daemon (Docker Engine 29.6.1/API 1.55, checked
// while building this interface) given `docker volume create --label
// foo=bar myvol` followed by `docker volume create --label foo=baz myvol`
// returns exit 0 both times and keeps the ORIGINAL volume's labels — the
// second call's Labels are silently discarded, no new volume is created,
// and no error is returned. This directly contradicts the naive assumption
// that Docker resource creation always rejects duplicate names (true for
// containers and networks, false for volumes), which is exactly the kind
// of real-daemon-divergent behavior the M0 tmpfs-uid/gid lesson warns
// against silently getting wrong. Callers that need a guaranteed-fresh
// volume per allocation (this provider's workspace/socket/dind-state
// volumes, ADR-001) must treat a name collision as their own signal — e.g.
// an orphaned leftover from a prior allocation reusing this instance name —
// rather than relying on VolumeCreate to catch it; that is WP2/WP3's
// concern, not this fake's.
func (f *FakeClient) VolumeCreate(_ context.Context, options volume.CreateOptions) (volume.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.VolumeCreateErr != nil {
		return volume.Volume{}, f.VolumeCreateErr
	}

	name := options.Name
	if name == "" {
		f.nextID++
		name = "fake-vol-" + strconv.Itoa(f.nextID)
	}

	if existing, ok := f.volumes[name]; ok {
		// Idempotent hit: the original volume's labels win, matching the
		// real daemon's observed behavior above.
		return volume.Volume{Name: existing.name, Labels: cloneLabels(existing.labels)}, nil
	}

	v := &fakeVolume{name: name, labels: cloneLabels(options.Labels)}
	f.volumes[name] = v
	return volume.Volume{Name: v.name, Labels: cloneLabels(v.labels)}, nil
}

// VolumeInspect returns one volume by name (including its labels), or an
// errdefs.IsNotFound-satisfying error when it is absent — mirroring the real
// SDK. It is how the GC re-validates a volume's ownership+identity labels
// immediately before removing it by name, and how the create path confirms a
// referenced cache volume is still the labeled one it ensured rather than an
// UNLABELED auto-created replacement (M2-W2 H3).
func (f *FakeClient) VolumeInspect(_ context.Context, volumeID string) (volume.Volume, error) {
	// Fire the re-inspect seam (if any) before taking the lock, so a test can
	// model a concurrent same-name replacement landing between a GC's snapshot
	// list and this re-inspect (H3). It runs without f.mu held so it may call
	// back into the fake's own locked methods.
	if f.VolumeInspectHook != nil {
		f.VolumeInspectHook(volumeID)
	}
	// A targeted transient inspect failure (NEW-H1), consulted BEFORE the store is
	// read so the volume stays present while its inspect fails.
	if f.VolumeInspectErrHook != nil {
		if err := f.VolumeInspectErrHook(volumeID); err != nil {
			return volume.Volume{}, err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	v, ok := f.volumes[volumeID]
	if !ok {
		return volume.Volume{}, notFoundf("volume %s not found", volumeID)
	}
	return volume.Volume{Name: v.name, Labels: cloneLabels(v.labels)}, nil
}

// VolumeRemove deletes a volume by name from the fake's in-memory store,
// tolerating "not found" like ContainerRemove/NetworkRemove. force is
// accepted for interface parity with the real SDK but has no effect here:
// the fake has no container-reference tracking for volumes to override.
func (f *FakeClient) VolumeRemove(_ context.Context, volumeID string, _ bool) error {
	// Fire the destructive-boundary hook (if any) before taking the lock, so a
	// test can mutate the store to model a concurrent generation B claiming the
	// instance name in the window between a teardown's volume LIST and this
	// physical REMOVE (F4). It runs without f.mu held so it may call back into
	// the fake's own locked methods.
	if f.VolumeRemoveHook != nil {
		f.VolumeRemoveHook(volumeID)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.VolumeRemoveErr != nil {
		return f.VolumeRemoveErr
	}
	if _, ok := f.volumes[volumeID]; !ok {
		return notFoundf("volume %s not found", volumeID)
	}
	// F13(a): the real daemon rejects removing a volume still referenced by ANY
	// container — running OR stopped — with a 409 "volume is in use", and force
	// does NOT override that for a named volume with an existing container
	// reference (force only suppresses the "no such volume" case). Model it so a
	// teardown that tried to remove a volume before its container fails in units
	// too, exactly as it would on the live daemon.
	for _, c := range f.containers {
		for _, m := range c.mounts {
			if m.Type == mount.TypeVolume && m.Source == volumeID {
				return conflictf("remove %s: volume is in use - [%s]", volumeID, c.id)
			}
		}
	}
	delete(f.volumes, volumeID)
	f.RemoveOrder = append(f.RemoveOrder, "volume:"+volumeID)
	return nil
}

// VolumeList returns every volume whose labels match options.Filters (a
// "label" filter), exactly as ContainerList/NetworkList do.
func (f *FakeClient) VolumeList(_ context.Context, options volume.ListOptions) (volume.ListResponse, error) {
	// Fire the concurrency hook (if any) before taking the lock, so a test can
	// mutate the store to model a concurrent teardown+create landing right when a
	// teardown enumerates volumes (F4). It runs without f.mu held to avoid a
	// re-entrant deadlock when the hook calls back into the fake.
	if f.VolumeListHook != nil {
		f.VolumeListHook()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var out []*volume.Volume
	for _, v := range f.volumes {
		if !options.Filters.MatchKVList("label", v.labels) {
			continue
		}
		out = append(out, &volume.Volume{Name: v.name, Labels: cloneLabels(v.labels)})
	}
	return volume.ListResponse{Volumes: out}, nil
}

func (c *fakeContainer) toContainerJSON() types.ContainerJSON {
	state := &types.ContainerState{
		Status:    c.state,
		Running:   c.state == "running",
		Dead:      c.state == "dead",
		OOMKilled: c.oomKilled,
	}
	// FinishedAt is surfaced only once the container has actually finished, as
	// an RFC3339Nano string exactly like the real daemon, so the orphan sweep's
	// exited-runner grace (F8) can age it. A never-finished container leaves it
	// empty (the real daemon reports the zero time "0001-01-01T00:00:00Z").
	if !c.finishedAt.IsZero() {
		state.FinishedAt = c.finishedAt.UTC().Format(time.RFC3339Nano)
	}
	base := &types.ContainerJSONBase{
		ID:    c.id,
		Name:  "/" + c.name,
		State: state,
		HostConfig: &container.HostConfig{
			Tmpfs:      cloneLabels(c.tmpfsMounts),
			Privileged: c.privileged,
			// F13: the daemon normalizes an omitted runtime to its default in
			// inspect output; RawRuntime exposes the un-normalized request.
			Runtime:  normalizeRuntime(c.runtime),
			GroupAdd: append([]string(nil), c.groupAdd...),
		},
	}
	if c.networkMode != "" {
		base.HostConfig.NetworkMode = container.NetworkMode(c.networkMode)
	}

	cj := types.ContainerJSON{
		ContainerJSONBase: base,
		Config: &container.Config{
			Image:  c.image,
			Env:    append([]string(nil), c.env...),
			Cmd:    strslice.StrSlice(append([]string(nil), c.cmd...)),
			Labels: cloneLabels(c.labels),
		},
		Mounts: c.mountPoints(),
	}
	// A user-defined per-job network attachment surfaces under
	// NetworkSettings.Networks, exactly as `docker inspect` reports it, with a
	// deterministic fake private IP so addressesFromInspect has something to
	// read. Built-in modes (bridge/host/none/default) are not reported here.
	if isUserDefinedNetwork(c.networkMode) {
		cj.NetworkSettings = &types.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				c.networkMode: {IPAddress: "10.240.0.2"},
			},
		}
	}
	return cj
}

// mountPoints converts the container's recorded HostConfig.Mounts into the
// types.MountPoint slice ContainerInspect reports under .Mounts, so a test can
// assert the workspace volume is mounted at the runner workdir.
func (c *fakeContainer) mountPoints() []types.MountPoint {
	if len(c.mounts) == 0 {
		return nil
	}
	out := make([]types.MountPoint, 0, len(c.mounts))
	for _, m := range c.mounts {
		out = append(out, types.MountPoint{
			Type:        m.Type,
			Name:        m.Source, // for a named volume, Source is the volume name
			Destination: m.Target,
			// RW mirrors the real daemon's inspect output: true when read-write,
			// false for a ReadOnly mount (M2-W2's externals volume is mounted RO).
			RW: !m.ReadOnly,
		})
	}
	return out
}

// normalizeRuntime maps an omitted create-request runtime ("") to the daemon
// default (F13), mirroring how the real daemon reports runtime in inspect
// output. A non-empty runtime (e.g. "sysbox-runc") is reported verbatim.
func normalizeRuntime(runtime string) string {
	if runtime == "" {
		return fakeDefaultRuntime
	}
	return runtime
}

// RawRuntime returns the Runtime EXACTLY as the container's create request set
// it — "" when no explicit runtime was requested — un-normalized, for tests
// that assert on what the builder emitted rather than on inspect output (F13).
func (f *FakeClient) RawRuntime(containerID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c := f.find(containerID); c != nil {
		return c.rawRuntime
	}
	return ""
}

// isUserDefinedNetwork reports whether mode names a user-defined Docker
// network (a per-job network) rather than a built-in mode. Only user-defined
// attachments are reported as endpoints and enforced by NetworkRemove's
// active-endpoint check.
func isUserDefinedNetwork(mode string) bool {
	switch mode {
	case "", "bridge", "host", "none", "default", "container":
		return false
	default:
		return true
	}
}

func (c *fakeContainer) toContainerSummary() types.Container {
	return types.Container{
		ID:     c.id,
		Names:  []string{"/" + c.name},
		Image:  c.image,
		Labels: cloneLabels(c.labels),
		Status: c.state,
		State:  c.state,
	}
}

func cloneLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// notFoundf builds an error that satisfies errdefs.IsNotFound, matching
// what the real moby SDK returns for a 404 from the daemon (see
// client/errors.go's IsErrNotFound = errdefs.IsNotFound). Later WPs' ID-
// or-name resolver (ADR-004) checks errdefs.IsNotFound uniformly, whether
// talking to this fake or the real MobyClient.
func notFoundf(format string, a ...any) error {
	return errdefs.NotFound(fmt.Errorf(format, a...))
}

// conflictf builds an error that satisfies errdefs.IsConflict, matching what
// the real moby SDK returns for a 409 name-conflict from the daemon. The
// ambiguous-create resolver (ADR-004 F6) and the fake's own name-uniqueness
// check (NEW-5) both rely on this shape.
func conflictf(format string, a ...any) error {
	return errdefs.Conflict(fmt.Errorf(format, a...))
}
