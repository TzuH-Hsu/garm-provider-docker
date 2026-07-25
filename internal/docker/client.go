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
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Client is the slice of the moby SDK this provider depends on.
//
// M0 "none" mode (ADR-001) needed only create/start/inspect/remove/list a
// runner container. M1 adds the Network*/Volume* methods below: the job
// network, socket volume, and dind-state volume that ADR-001's DinD modes
// and ADR-004's claim-marker teardown need. The topology code that actually
// calls these (creating/tearing down a job's network and volumes as part of
// CreateInstance/DeleteInstance) is WP2/WP3, not this work package — this
// interface only needs to exist and be backed by a faithful fake so WP2/WP3
// can build against it. Every method's signature is copied verbatim from the
// moby SDK so *client.Client (embedded in mobyClient, moby.go) already
// satisfies this interface unchanged, with no wrapper code required.
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

	// ContainerWait blocks until the container reaches condition (e.g.
	// container.WaitConditionNotRunning) and delivers the exit status on the
	// returned response channel, or a failure on the error channel — the moby
	// SDK's own two-channel shape. It backs M2-W2's run-to-completion helpers
	// (the externals seeder and the diagnostic-log pruner): create → start →
	// wait-for-exit → remove. The seeder in particular MUST block here until the
	// copy finishes, so the runner never mounts a half-seeded externals tree.
	// The real *client.Client already provides this exact signature, so
	// mobyClient satisfies it unchanged.
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)

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

	// NetworkCreate creates a labeled bridge network — the per-job network
	// ADR-001 requires in every mode, and ADR-004's claim marker (the first
	// resource created for an allocation). It returns a Conflict error
	// (errdefs.IsConflict) when a network with this name already exists,
	// matching the real daemon: the API 1.44+ duplicate-name check on
	// network create is unconditional, not opt-in (see the (removed)
	// CheckDuplicate handling in the moby SDK's client.NetworkCreate),
	// confirmed against a live daemon while building this interface (see
	// fake.go's doc comment on network name uniqueness).
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)

	// NetworkRemove removes a network by ID or name. It returns an
	// errdefs.IsNotFound-satisfying error when no such network exists,
	// which teardown ordering (ADR-004) tolerates like every other remove.
	NetworkRemove(ctx context.Context, networkID string) error

	// NetworkList lists networks, filtered by options.Filters — used with a
	// "label" filter for the ADR-004 teardown/orphan-sweep predicate, same
	// as ContainerList.
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)

	// VolumeCreate creates a labeled volume — the workspace, socket, and
	// dind-state volumes of ADR-001. Unlike NetworkCreate/ContainerCreate,
	// the real daemon's volume create is idempotent on a duplicate name: it
	// silently returns the EXISTING volume (original Labels/Driver kept,
	// the new call's Labels discarded) rather than erroring or creating a
	// second volume — confirmed against a live daemon while building this
	// interface (see fake.go's doc comment on volume name idempotency).
	// Callers that rely on a fresh, empty volume per allocation must treat a
	// name collision as its own signal (e.g. an orphaned leftover from a
	// prior allocation of the same instance name) rather than assuming
	// VolumeCreate itself will catch it.
	VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error)

	// VolumeInspect returns one volume by name, including its Labels. It
	// returns an errdefs.IsNotFound-satisfying error when no such volume
	// exists. Used to RE-VALIDATE a cache volume's ownership+identity labels
	// against a snapshot immediately before the destructive diagnostic-log
	// file-prune runs inside it (the GC's own cache-volume pass is log-only and
	// never removes a volume itself — ADR-003's cache-GC-safety amendment), and
	// to confirm — after the runner container is created — that a referenced
	// cache volume is still the labeled one this provider ensured, not an
	// UNLABELED volume the daemon auto-created because an operator-run manual
	// `docker volume prune` (or some other external actor) removed the
	// original in the ensure→mount window (ADR-003 W2, H3).
	VolumeInspect(ctx context.Context, volumeID string) (volume.Volume, error)

	// VolumeRemove removes a volume by name. force=true also removes a
	// volume still referenced by a stopped container (teardown ordering
	// removes containers before volumes, so force is a defense-in-depth
	// knob more than a primary mechanism). It returns an
	// errdefs.IsNotFound-satisfying error when no such volume exists.
	VolumeRemove(ctx context.Context, volumeID string, force bool) error

	// VolumeList lists volumes, filtered by options.Filters — used with a
	// "label" filter, same as ContainerList/NetworkList.
	VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
}
