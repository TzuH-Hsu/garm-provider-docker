# garm-runner-noble

The Ubuntu 24.04 (Noble) GitHub Actions runner image for `garm-provider-docker`, published as
`ghcr.io/tzuh-hsu/garm-runner-noble`. It is built `FROM` a digest-pinned `myoung34/github-runner:ubuntu-noble`
with the shared JIT-aware entrypoint.

The entrypoint, its environment contract, the package policy and the package list it shares with the Ubuntu 26.04
image are documented once, in [`../README.md`](../README.md). This page covers what is specific to Noble.

## When to use it

For workflows that target `ubuntu-24.04`, or that do not need the Ubuntu 26.04 toolchain. For that toolchain use
[`garm-runner-resolute`](../resolute/README.md).

## Toolchain

| Tool | Version | Source |
|---|---|---|
| `gcc`, `g++` | 13 | base image (`build-essential`) |
| `clang-format`, `clang-tidy` | 18 (`clang-format-18`, `clang-tidy-18`) | Ubuntu archive; the unversioned names via `update-alternatives` |
| `cmake` | 3.28 | Ubuntu archive |
| `ninja` | 1.11 | Ubuntu archive (`ninja-build`) |
| Python | 3.12, with `pipx` | base image, `pipx` from the Ubuntu archive |
| Node | 18 | base image (`pnpm` 9 added by this image) |

LLVM 18 is Noble's own default (the release its unversioned `clang-format` and `clang-tidy` packages depend on)
and the default on GitHub's ubuntu-24.04 image. Newer LLVM on Noble exists only in `noble-updates` (19, 20) and in
the third-party apt.llvm.org repository, and neither is added. A repository that formats or analyses against
LLVM 22 uses the Resolute image, where 22 comes from the Ubuntu archive itself.

## Build

```sh
docker build -t garm-runner-noble:dev -f runner-images/noble/Dockerfile runner-images
```

The build context is `runner-images/`, not this directory, so the shared `common/entrypoint.sh` is in reach.

## Base image digest

The base is pinned by digest and not by the mutable tag, so a rebuild happens only when this repository bumps the
pin (or the `GH_RUNNER_VERSION` the base image is built with). The digest in the `Dockerfile` is the
**manifest-list (index) digest** of `myoung34/github-runner:ubuntu-noble`, which covers both `linux/amd64` and
`linux/arm64`. That lets the multi-platform build in `.github/workflows/images.yml` resolve the right image per
platform. Never substitute a single-platform digest, which would break the other architecture's build.

Re-resolve the current digest with:

```sh
docker buildx imagetools inspect myoung34/github-runner:ubuntu-noble
# or, without a local Docker daemon:
skopeo inspect docker://myoung34/github-runner:ubuntu-noble
```

After a rebuild, update the digest pinned in provider config. Image selection is config-only, and a pool's
`extra_specs` cannot override it.
