package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
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
//
// A container that joined another's network namespace ("container:<ref>")
// also models how the daemon adds network stats to the sample: it looks up
// the namespace owner's sandbox, and the whole sample depends on that lookup.
// With the owner removed the lookup is NotFound and the daemon answers with an
// empty sample (name and ID, no blkio counters) although the container still
// runs; with the owner stopped but not removed its sandbox ID is cleared and
// the request fails.
func (f *FakeClient) ContainerStatsOneShot(ctx context.Context, containerID string) (container.StatsResponseReader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Like the real client, a request on a context that is already done fails.
	if err := ctx.Err(); err != nil {
		return container.StatsResponseReader{}, err
	}
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
	body := c.statsBody
	if ref, ok := joinedNetnsOwner(c.networkMode); ok {
		switch owner := f.find(ref); {
		case owner == nil:
			body = fmt.Sprintf(`{"name":%q,"id":%q}`, "/"+c.name, c.id)
		case owner.state != "running":
			return container.StatsResponseReader{}, errdefs.InvalidParameter(errors.New("invalid id: id is empty"))
		}
	}
	return container.StatsResponseReader{
		Body:   io.NopCloser(strings.NewReader(body)),
		OSType: "linux",
	}, nil
}

// ExecOutput models an output-capturing docker exec: it records the call and
// reports ExecOutputText and ExecOutputExit, or what ExecOutputFunc returns
// when it is set. Like the real daemon it requires a running container.
func (f *FakeClient) ExecOutput(ctx context.Context, containerID string, cmd []string) (string, int, error) {
	f.mu.Lock()
	if f.ExecOutputErr != nil {
		f.mu.Unlock()
		return "", 0, f.ExecOutputErr
	}
	c := f.find(containerID)
	if c == nil {
		f.mu.Unlock()
		return "", 0, notFoundf("container %s not found", containerID)
	}
	if c.state != "running" {
		f.mu.Unlock()
		return "", 0, fmt.Errorf("cannot exec in container %s: not running", containerID)
	}
	f.OutputExecs = append(f.OutputExecs, ExecRecord{ContainerID: c.id, Cmd: append([]string(nil), cmd...)})
	fn, out, code := f.ExecOutputFunc, f.ExecOutputText, f.ExecOutputExit
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, cmd)
	}
	return out, code, nil
}
