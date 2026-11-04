# Installation

GARM runs a provider by executing a file on disk, so whichever method you use,
the goal is the same: put the binary at the path set in `provider_executable`.

Each `vX.Y.Z` tag publishes:

- static `linux/amd64` and `linux/arm64` binaries with a `SHA256SUMS` file, on
  the [releases page](https://github.com/TzuH-Hsu/garm-provider-docker/releases)
- `ghcr.io/tzuh-hsu/garm-provider-docker`, which carries the binary
- `ghcr.io/tzuh-hsu/garm-runner-noble` (Ubuntu 24.04) and
  `ghcr.io/tzuh-hsu/garm-runner-resolute` (Ubuntu 26.04)

Images are multi-arch, with SBOM and provenance attestations, and tagged
`:<tag>`. `:latest` moves only when the tag's GitHub Release becomes a full
release.

Only `linux/amd64` has been tested end to end. See the README's
[verification status](../README.md#verification-status).

## From the release page

See the README's [quick start](../README.md#quick-start).

## From the image

The provider image cannot run as a container. GARM executes a filesystem path
and has no way to run an image, and the image's non-root user could not open
the host's Docker socket. The image is only a way to get the binary without a
Go toolchain:

```sh
cid="$(docker create ghcr.io/tzuh-hsu/garm-provider-docker:<tag>)"
docker cp "${cid}:/garm-provider-docker" /opt/garm/providers.d/garm-provider-docker
docker rm "${cid}"
chmod +x /opt/garm/providers.d/garm-provider-docker
```

If you build your own GARM image:

```dockerfile
COPY --from=ghcr.io/tzuh-hsu/garm-provider-docker:<tag> \
     /garm-provider-docker /opt/garm/providers.d/garm-provider-docker
```

## From source

You need the Go version named in [`go.mod`](../go.mod).

```sh
git clone https://github.com/TzuH-Hsu/garm-provider-docker
cd garm-provider-docker
go build -o garm-provider-docker .
```

## GARM in a container

The binary runs in GARM's process context, so GARM's container needs the host
Docker socket mounted. Treat that as root access to the host. Never mount the
socket into a runner or DinD sidecar.

Start GARM with an init process (`docker run --init`, or `init: true` in Docker
Compose). The provider reports runner status from a short-lived background
process that PID 1 must reap, and GARM's own image runs `/bin/garm` as PID 1,
which does not reap it. Without an init, the provider detects this and waits for
the runner inside `CreateInstance`, which makes creates slower. See
[`[status_report]`](config-reference.md#status_report-runner-status-reported-to-garm).

## Pinning images

Pin `runner_image`, `dind_image` and any `[flavors.*].runner_image` by digest
(`name@sha256:<64-hex>`). `allow_unpinned_runner_image` and
`allow_unpinned_dind_image` are for development only.

```sh
docker buildx imagetools inspect ghcr.io/tzuh-hsu/garm-runner-noble:latest
docker buildx imagetools inspect ghcr.io/tzuh-hsu/garm-runner-resolute:latest
# or, without a local Docker daemon:
skopeo inspect docker://ghcr.io/tzuh-hsu/garm-runner-noble:latest
```

## Pools

Scale sets are the tested path. To create a pool instead:

```sh
garm-cli pool add --repo <repo> --provider-name docker_local \
  --min-idle-runners 0 --max-runners <n> --image unused \
  --flavor unused --tags <your-labels>
```

`--image` and `--flavor` are required by `garm-cli` but ignored by this provider.
Images and resources come from the provider config and, per pool,
`extra_specs.flavor`. See the [configuration reference](config-reference.md).
