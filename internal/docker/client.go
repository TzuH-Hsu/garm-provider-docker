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

	// ImageInspectWithRaw inspects a local image by reference. It returns
	// an errdefs.IsNotFound-satisfying error when the image is not present
	// locally, which is how CreateInstance decides whether it must pull
	// (ADR-002: pull-if-missing). The raw []byte return of the underlying
	// SDK method is unused by this provider but kept in the signature so
	// the real *client.Client satisfies this interface unchanged.
	ImageInspectWithRaw(ctx context.Context, imageID string) (types.ImageInspect, []byte, error)

	// ContainerCreate creates (but does not start) a container.
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)

	// ContainerStart starts a previously created container.
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error

	// ContainerStop stops a running container, looked up by ID or name. It
	// returns an errdefs.IsNotFound-satisfying error when no such container
	// exists. Used by Stop and, best-effort, by the delete ordering
	// (ADR-004).
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error

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

	// ExecStream runs cmd inside a running container, streaming stdin to the
	// exec's standard input, and returns the command's exit code once it has
	// finished. It is how the provider delivers credentials into the
	// runner's memory-backed credential tmpfs (ADR-002): a `tar -x` fed the
	// in-memory credential archive over its stdin.
	//
	// stdin must be a finite, in-memory reader (e.g. *bytes.Buffer) whose
	// Read never blocks — every current caller passes the credential tar
	// built entirely in memory. Cancelling ctx force-closes the underlying
	// connection to unblock output draining, but that does not interrupt a
	// concurrent stdin.Read(); a reader that can block indefinitely (a live
	// network stream, say) would defeat the cancellation guarantee. See
	// streamExec's doc comment in moby.go for the full explanation.
	//
	// docker exec — not docker cp (CopyToContainer) — is used deliberately.
	// Docker's archive endpoints resolve the destination path in a separate
	// filesystem view that does not include the container's user tmpfs
	// mounts (moby v27.5.1 daemon/containerfs_linux.go), so a `docker cp`
	// into a running container's tmpfs cannot land there. A process started
	// by `docker exec` runs in the container's own mount namespace, where
	// the tmpfs is visible, so the extracted files reach the real tmpfs.
	// This is why CopyToContainer was removed from this interface entirely.
	ExecStream(ctx context.Context, containerID string, cmd []string, stdin io.Reader) (exitCode int, err error)
}
