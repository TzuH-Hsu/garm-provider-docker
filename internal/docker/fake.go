package docker

import (
	"context"
	"fmt"
	"io"
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

	// CopyErr, when non-nil, is returned by CopyToContainer instead of
	// recording the copy — used to exercise the copy-failure creation
	// guard path.
	CopyErr error

	// Copies records every CopyToContainer call, in order, so tests can
	// assert the credential archive was streamed to the right container
	// and destination.
	Copies []CopyRecord
}

// CopyRecord captures one CopyToContainer call for test assertions.
type CopyRecord struct {
	ContainerID string
	DstPath     string
	Content     []byte
}

type fakeContainer struct {
	id      string
	name    string
	image   string
	labels  map[string]string
	env     []string
	started bool
}

// NewFakeClient constructs an empty FakeClient.
func NewFakeClient() *FakeClient {
	return &FakeClient{
		containers:    make(map[string]*fakeContainer),
		PresentImages: make(map[string]bool),
	}
}

var _ Client = (*FakeClient)(nil)

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

// CopyToContainer records the streamed archive (or fails when CopyErr is
// set). The container must exist and be started, matching the real daemon,
// which rejects a copy into a non-running container's tmpfs.
func (f *FakeClient) CopyToContainer(_ context.Context, containerID, dstPath string, content io.Reader, _ container.CopyToContainerOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.CopyErr != nil {
		return f.CopyErr
	}
	c, ok := f.containers[containerID]
	if !ok {
		return notFoundf("container %s not found", containerID)
	}
	if !c.started {
		return fmt.Errorf("cannot copy into container %s: not running", containerID)
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	f.Copies = append(f.Copies, CopyRecord{ContainerID: containerID, DstPath: dstPath, Content: data})
	return nil
}

// ContainerCreate creates an in-memory container record in the "created,
// not started" state.
func (f *FakeClient) ContainerCreate(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, containerName string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

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
	}
	if cfg != nil {
		c.image = cfg.Image
		c.env = append([]string(nil), cfg.Env...)
		for k, v := range cfg.Labels {
			c.labels[k] = v
		}
	}
	f.containers[id] = c

	return container.CreateResponse{ID: id}, nil
}

// ContainerStart marks a previously created container as started.
func (f *FakeClient) ContainerStart(_ context.Context, containerID string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, ok := f.containers[containerID]
	if !ok {
		return notFoundf("container %s not found", containerID)
	}
	c.started = true
	return nil
}

// ContainerInspect returns the recorded state of containerID.
func (f *FakeClient) ContainerInspect(_ context.Context, containerID string) (types.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, ok := f.containers[containerID]
	if !ok {
		return types.ContainerJSON{}, notFoundf("container %s not found", containerID)
	}
	return c.toContainerJSON(), nil
}

// ContainerRemove deletes containerID from the fake's in-memory store.
func (f *FakeClient) ContainerRemove(_ context.Context, containerID string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.containers[containerID]; !ok {
		return notFoundf("container %s not found", containerID)
	}
	delete(f.containers, containerID)
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
	state := &types.ContainerState{Running: c.started}
	if !c.started {
		state.Status = "created"
	} else {
		state.Status = "running"
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
	status := "created"
	if c.started {
		status = "running"
	}
	return types.Container{
		ID:     c.id,
		Names:  []string{"/" + c.name},
		Image:  c.image,
		Labels: cloneLabels(c.labels),
		Status: status,
		State:  status,
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
