# garm-runner-noble

The Ubuntu 24.04 (Noble) GitHub Actions runner image for
`garm-provider-docker`'s runner containers, published as
`ghcr.io/tzuh-hsu/garm-runner-noble`. It is built `FROM` a digest-pinned
`myoung34/github-runner:ubuntu-noble` with the shared JIT-aware entrypoint.

The entrypoint, its environment contract, the package policy and the package
list it shares with the Ubuntu 26.04 image are documented once, in
[`../README.md`](../README.md). This page covers what is specific to Noble.

## When to use it

For repositories whose workflows target `ubuntu-24.04`, or that have not moved
to the Ubuntu 26.04 toolchain `ubuntu-latest` points at from 2026-10-19. For
the 26.04 toolchain use [`garm-runner-resolute`](../resolute/README.md).

## Toolchain

| Tool | Version | Source |
|---|---|---|
| `gcc`, `g++` | 13 | base image (`build-essential`) |
| `clang-format`, `clang-tidy` | 18 (`clang-format-18`, `clang-tidy-18`) | Ubuntu archive; the unversioned names via `update-alternatives` |
| `cmake` | 3.28 | Ubuntu archive |
| `ninja` | 1.11 | Ubuntu archive (`ninja-build`) |
| Python | 3.12, with `pipx` | base image, `pipx` from the Ubuntu archive |
| Node | 18 | base image (`pnpm` 9 added by this image) |

LLVM 18 is Noble's own default (the release its unversioned
`clang-format`/`clang-tidy` packages depend on) and the default on GitHub's
ubuntu-24.04 image. Newer LLVM on Noble exists only in `noble-updates` (19, 20)
and in the third-party apt.llvm.org repository; neither is added here. A
repository that formats or analyses against LLVM 22 uses the Resolute image,
where 22 comes from the Ubuntu archive itself.

## Build

```sh
docker build -t garm-runner-noble:dev -f runner-images/noble/Dockerfile runner-images
```

The build context is `runner-images/`, not this directory, so the shared
`common/entrypoint.sh` is in reach.

## Digest-pin policy

The base image is pinned by digest, not by mutable tag, so a rebuild only
happens when this repository explicitly bumps the pin (or the
`GH_RUNNER_VERSION` the base image itself is built with). The digest in the
`Dockerfile` is the **manifest-list (index) digest** for
`myoung34/github-runner:ubuntu-noble` — not a single-platform manifest —
confirmed to cover both `linux/amd64` and `linux/arm64` under that one
digest. This lets `.github/workflows/images.yml`'s multi-platform
`docker/build-push-action` (`platforms: linux/amd64,linux/arm64`) resolve
the correct per-platform image automatically via buildx, with no separate
build matrix needed in this Dockerfile. (M0 originally pinned the
single-platform `linux/amd64` manifest, matching that milestone's
`linux/amd64`-only scope; M4 re-pinned to the manifest-list digest for the
multi-arch release — see the `Dockerfile`'s own comment for the history.)

Re-resolve the current digest with:

```sh
docker buildx imagetools inspect myoung34/github-runner:ubuntu-noble
# or, without a local Docker daemon:
skopeo inspect docker://myoung34/github-runner:ubuntu-noble
```

Bumping the digest (or `GH_RUNNER_VERSION`) is an operator action: after
a rebuild, update the digest pinned in provider config (ADR-005) — image
selection is config-only, never something a pool's `extra_specs` can
override.
