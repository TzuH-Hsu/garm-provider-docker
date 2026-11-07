// Package docker defines the narrow view of the moby Docker SDK that this provider
// depends on, and an in-memory fake of it for tests.
package docker

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Client is the slice of the moby SDK this provider depends on. Every signature but
// ExecStream and ExecOutput is copied from the SDK, so the *client.Client embedded in
// mobyClient provides the rest with no wrapper. A call that
// takes a container, network or volume ID or name returns an errdefs.IsNotFound error
// when nothing matches, except where noted.
type Client interface {
	// ImagePull pulls refStr, honoring options such as registry auth. The caller must
	// read the returned stream to the end and close it for the pull to finish.
	ImagePull(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)

	// ImageInspectWithRaw inspects a local image by reference. It returns an
	// errdefs.IsNotFound error when the image is not present, which is how CreateInstance
	// decides whether to pull (ADR-002). The raw bytes are unused; they keep the SDK
	// signature.
	ImageInspectWithRaw(ctx context.Context, imageID string) (types.ImageInspect, []byte, error)

	// ContainerCreate creates (but does not start) a container.
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)

	// ContainerStart starts a previously created container.
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error

	// ContainerStop stops a running container, found by ID or name.
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error

	// ContainerInspect returns the full state of one container, found by ID or name.
	ContainerInspect(ctx context.Context, containerID string) (types.ContainerJSON, error)

	// ContainerRemove removes a container, found by ID or name.
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error

	// ContainerList lists containers matching options.Filters. The provider looks up every
	// managed resource with a "label" filter (ADR-004).
	ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error)

	// ContainerWait blocks until the container reaches condition. The exit status arrives
	// on the response channel and a failure on the error channel, as in the SDK. The
	// run-to-completion helper containers use it and must wait here, so the runner never
	// mounts a half-seeded volume.
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)

	// ExecStream runs cmd in a running container with stdin streamed to the exec, and
	// returns the exit code once it finishes. It delivers credentials into the runner's
	// memory-backed tmpfs as a `tar -x` fed the in-memory credential archive (ADR-002).
	//
	// stdin must be a finite in-memory reader whose Read never blocks. Cancelling ctx
	// closes the connection but does not interrupt a pending stdin.Read, so a blocking
	// reader would hang the call; see streamExecTo in moby.go.
	//
	// It is an exec rather than CopyToContainer because Docker's archive endpoints resolve
	// paths in a filesystem view that excludes user tmpfs mounts, so a copy cannot land in
	// the tmpfs. An exec runs in the container's mount namespace, where the tmpfs is
	// visible.
	ExecStream(ctx context.Context, containerID string, cmd []string, stdin io.Reader) (exitCode int, err error)

	// ExecOutput runs cmd in a running container as its default user, with no stdin. It
	// returns stdout and stderr interleaved, capped at execOutputLimit bytes, and the
	// exit code. A non-zero exit is reported in exitCode, not as an error; err is for
	// transport failures, output overflow and ctx cancellation.
	ExecOutput(ctx context.Context, containerID string, cmd []string) (output string, exitCode int, err error)

	// ContainerStatsOneShot returns a single stats sample without waiting to prime the
	// CPU counters. The caller must close the returned reader.
	ContainerStatsOneShot(ctx context.Context, containerID string) (container.StatsResponseReader, error)

	// ContainerLogs streams a container's logs. For a container created without a TTY, as
	// every container here is, the stream is stdcopy-multiplexed and must be
	// demultiplexed with pkg/stdcopy. With options.Follow it stays open until the
	// container stops or ctx is cancelled. The caller must close the reader.
	ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error)

	// ContainerTop lists the processes in a container (`docker top`). The daemon answers
	// 409 Conflict for a container that is not running. CreateInstance uses it to check
	// readiness when the logs cannot be read.
	ContainerTop(ctx context.Context, containerID string, arguments []string) (container.TopResponse, error)

	// NetworkCreate creates a labeled bridge network. It returns an errdefs.IsConflict
	// error when the name is in use: since API 1.44 the daemon always rejects a duplicate
	// network name. ADR-004 uses the first network created for an allocation as its
	// claim marker.
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)

	// NetworkRemove removes a network, found by ID or name.
	NetworkRemove(ctx context.Context, networkID string) error

	// NetworkList lists networks matching options.Filters, like ContainerList.
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)

	// VolumeCreate creates a labeled volume. Unlike NetworkCreate and ContainerCreate, a
	// duplicate name is not an error: the daemon returns the existing volume with its
	// original labels and discards the new ones. A caller that needs a fresh volume must
	// treat a name collision as its own signal.
	VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error)

	// VolumeInspect returns one volume by name, including its Labels. Callers use it to
	// re-validate a cache volume's labels before a destructive step, and to confirm that a
	// volume mounted into a container is still the labeled one: the daemon silently creates
	// a missing named volume, unlabeled, when a container references it (ADR-003).
	VolumeInspect(ctx context.Context, volumeID string) (volume.Volume, error)

	// VolumeRemove removes a volume by name. A volume referenced by any container, even a
	// stopped one, fails with a conflict, and force does not override that, so remove the
	// containers first. With force, a missing volume returns nil instead of NotFound.
	VolumeRemove(ctx context.Context, volumeID string, force bool) error

	// VolumeList lists volumes matching options.Filters, like ContainerList.
	VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
}
