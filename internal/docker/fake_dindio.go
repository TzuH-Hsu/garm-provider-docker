package docker

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"
)

// SetStats sets the JSON body ContainerStatsOneShot returns for containerID
// (by ID or name), replacing any earlier one. Unknown containers are ignored.
func (f *FakeClient) SetStats(containerID, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(containerID); c != nil {
		c.statsBody = body
	}
}

// ContainerStatsOneShot models the one-shot stats endpoint: a missing
// container is NotFound and a container that is not running is a Conflict,
// like ExecOutput's daemon, since the real daemon has no cgroup to sample.
func (f *FakeClient) ContainerStatsOneShot(_ context.Context, containerID string) (container.StatsResponseReader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.StatsErr != nil {
		return container.StatsResponseReader{}, f.StatsErr
	}
	c := f.find(containerID)
	if c == nil {
		return container.StatsResponseReader{}, notFoundf("No such container: %s", containerID)
	}
	if c.state != "running" {
		return container.StatsResponseReader{}, conflictf("container %s is not running", c.id)
	}
	return container.StatsResponseReader{
		Body:   io.NopCloser(strings.NewReader(c.statsBody)),
		OSType: "linux",
	}, nil
}

// ExecOutput models an output-capturing docker exec: it records the call and
// reports ExecOutputText and ExecOutputExit. Like the real daemon it requires
// a running container.
func (f *FakeClient) ExecOutput(_ context.Context, containerID string, cmd []string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ExecOutputErr != nil {
		return "", 0, f.ExecOutputErr
	}
	c := f.find(containerID)
	if c == nil {
		return "", 0, notFoundf("container %s not found", containerID)
	}
	if c.state != "running" {
		return "", 0, fmt.Errorf("cannot exec in container %s: not running", containerID)
	}
	f.OutputExecs = append(f.OutputExecs, ExecRecord{ContainerID: c.id, Cmd: append([]string(nil), cmd...)})
	return f.ExecOutputText, f.ExecOutputExit, nil
}
