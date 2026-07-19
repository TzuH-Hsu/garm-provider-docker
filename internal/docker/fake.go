package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
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

	// Execs records every ExecStream call, in order, so tests can assert the
	// credential archive was streamed to the right container with the right
	// command.
	Execs []ExecRecord

	// CreateHook, when non-nil, is invoked at the very start of every
	// ContainerCreate — before the name-uniqueness check and before f.mu is
	// taken — so a test can model a concurrent CreateInstance that occupied a
	// container name in the window between another caller's duplicate check and
	// its own create (NEW-2). It must not re-enter this same ContainerCreate
	// call reentrantly without guarding (it runs without f.mu held).
	CreateHook func()
}

// ExecRecord captures one ExecStream call for test assertions.
type ExecRecord struct {
	ContainerID string
	Cmd         []string
	Stdin       []byte
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

	// state is the Docker state string: created, running, exited, or dead.
	// It drives both the Running bool and the status a caller maps from.
	state     string
	oomKilled bool
}

// NewFakeClient constructs an empty FakeClient.
func NewFakeClient() *FakeClient {
	return &FakeClient{
		containers:    make(map[string]*fakeContainer),
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
// behavior for an absent image).
func (f *FakeClient) ImageInspectWithRaw(_ context.Context, imageID string) (types.ImageInspect, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.PresentImages[imageID] {
		return types.ImageInspect{}, nil, notFoundf("image %s not found", imageID)
	}
	return types.ImageInspect{ID: imageID}, nil, nil
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
// keyed by filename, so tests can assert what a docker-exec delivery wrote.
func (f *FakeClient) Tmpfs(containerID string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, ok := f.containers[containerID]
	if !ok {
		return nil
	}
	out := make(map[string]string, len(c.tmpfs))
	for k, v := range c.tmpfs {
		out[k] = v
	}
	return out
}

// ContainerCreate creates an in-memory container record in the "created,
// not started" state.
func (f *FakeClient) ContainerCreate(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, containerName string) (container.CreateResponse, error) {
	// Fire the concurrency hook (if any) before taking the lock, so a test can
	// inject a same-name "winner" container that this call then collides with
	// (NEW-2). It runs without f.mu held to avoid a re-entrant deadlock.
	if f.CreateHook != nil {
		f.CreateHook()
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
		for k, v := range cfg.Labels {
			c.labels[k] = v
		}
	}
	f.containers[id] = c

	// Ambiguous create failure: the container was recorded, but the call
	// still reports an error (ADR-004 F6).
	if f.CreateErr != nil {
		return container.CreateResponse{ID: id}, f.CreateErr
	}

	return container.CreateResponse{ID: id}, nil
}

// ContainerStart marks a previously created container as running.
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
	return nil
}

// SetState forces a container's Docker state (created, running, exited,
// dead) and OOMKilled flag, so status-mapping paths that a plain
// start/stop cannot reach (dead, OOM-killed) are testable end-to-end.
func (f *FakeClient) SetState(containerID, state string, oomKilled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c, ok := f.containers[containerID]; ok {
		c.state = state
		c.oomKilled = oomKilled
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

func (c *fakeContainer) toContainerJSON() types.ContainerJSON {
	state := &types.ContainerState{
		Status:    c.state,
		Running:   c.state == "running",
		Dead:      c.state == "dead",
		OOMKilled: c.oomKilled,
	}
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			ID:    c.id,
			Name:  "/" + c.name,
			State: state,
		},
		Config: &container.Config{
			Image:  c.image,
			Env:    append([]string(nil), c.env...),
			Labels: cloneLabels(c.labels),
		},
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
