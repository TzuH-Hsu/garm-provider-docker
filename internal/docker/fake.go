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
	return &FakeClient{containers: make(map[string]*fakeContainer)}
}

var _ Client = (*FakeClient)(nil)

// ImagePull records refStr and returns an already-closed empty reader —
// there is no real image content to stream in the fake.
func (f *FakeClient) ImagePull(_ context.Context, refStr string, _ image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.PulledImages = append(f.PulledImages, refStr)
	return io.NopCloser(strings.NewReader("")), nil
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
