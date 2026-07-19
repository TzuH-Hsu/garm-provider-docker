// Package docker defines this provider's own narrow view of the moby
// Docker SDK, so internal/provider and internal/topology (M1+) depend on
// an interface this repo controls rather than on *client.Client directly.
package docker

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Client is the slice of the moby SDK this provider depends on.
//
// It is deliberately minimal for M0 "none" mode (ADR-001): create/start/
// inspect/remove/list a runner container, nothing else. DinD modes (M1)
// need NetworkCreate/NetworkRemove and VolumeCreate/VolumeRemove; those
// are added to this interface then, not now. Adding methods to a Go
// interface is purely additive — it does not break MobyClient (moby.go),
// which embeds the real *client.Client and so already implements any
// method the SDK exposes — so there is no forward-compat cost to
// deferring them until M1 actually needs them.
type Client interface {
	// ImagePull pulls refStr, honoring options (e.g. registry auth).
	// Callers must read the returned ReadCloser to completion (and close
	// it) to let the pull finish; the moby SDK streams progress through
	// it rather than blocking until the pull is done.
	ImagePull(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)

	// ContainerCreate creates (but does not start) a container.
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)

	// ContainerStart starts a previously created container.
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error

	// ContainerInspect returns the full state of one container, looked up
	// by ID or name. It returns an errdefs.IsNotFound-satisfying error
	// when no such container exists.
	ContainerInspect(ctx context.Context, containerID string) (types.ContainerJSON, error)

	// ContainerRemove removes a container, looked up by ID or name. It
	// returns an errdefs.IsNotFound-satisfying error when no such
	// container exists.
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error

	// ContainerList lists containers, filtered by options.Filters — used
	// with a "label" filter for every managed-resource lookup (ADR-004).
	ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error)
}
