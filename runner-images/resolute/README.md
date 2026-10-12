# garm-runner-resolute

The Ubuntu 26.04 (Resolute) GitHub Actions runner image for
`garm-provider-docker`'s runner containers, published as
`ghcr.io/tzuh-hsu/garm-runner-resolute`. It is built `FROM` a digest-pinned
`myoung34/github-runner:ubuntu-resolute` with the shared JIT-aware entrypoint.

The entrypoint, its environment contract, the package policy and the package
list it shares with the Ubuntu 24.04 image are documented once, in
[`../README.md`](../README.md). This page covers what is specific to Resolute.

## When to use it

GitHub moves `ubuntu-latest` to Ubuntu 26.04 on 2026-10-19. Use this image for
repositories whose workflows target `ubuntu-latest` or `ubuntu-26.04` and
depend on that release's toolchain: GCC 15 as the default compiler, LLVM 22 for
`clang-format`/`clang-tidy`, Python 3.14. Repositories that stay on
`ubuntu-24.04` keep using [`garm-runner-noble`](../noble/README.md). Select it
per pool through a flavor (see [`../README.md`](../README.md#choosing-an-image)).

## Toolchain

| Tool | Version | Source |
|---|---|---|
| `gcc`, `g++` | 15 | base image (`build-essential`) |
| `clang-format`, `clang-tidy` | 22 (`clang-format-22`, `clang-tidy-22`) | Ubuntu archive; the unversioned names via `update-alternatives` |
| `cmake` | 4.2 | Ubuntu archive |
| `ninja` | 1.13 | Ubuntu archive (`ninja-build`) |
| Python | 3.14, with `pipx` | base image, `pipx` from the Ubuntu archive |
| Node | 22 | base image (`pnpm` 9 added by this image) |

Resolute's unversioned `clang-format`/`clang-tidy` packages still depend on
LLVM 21, but the archive also ships LLVM 22. This image installs
`clang-format-22` and `clang-tidy-22` and registers them as the unversioned
`clang-format`/`clang-tidy` with `update-alternatives`, so a repository that
formats against 22 gets 22 under either name and never sees spurious diffs
from 21. LLVM 21 is not installed as a tool (Mesa, pulled in by `libgbm1`,
carries its own `libllvm21` runtime library only).

## Build

```sh
docker build -t garm-runner-resolute:dev -f runner-images/resolute/Dockerfile runner-images
```

The build context is `runner-images/`, not this directory, so the shared
`common/entrypoint.sh` is in reach.

## Digest-pin policy

As for the Noble image: the base is pinned by its **manifest-list (index)
digest**, which covers both `linux/amd64` and `linux/arm64`, so
`.github/workflows/images.yml`'s multi-platform build resolves the right
per-platform image itself. The pin was taken from the rolling
`ubuntu-resolute` tag (GitHub runner 2.338.0 at the time). Re-resolve it with:

```sh
docker buildx imagetools inspect myoung34/github-runner:ubuntu-resolute
# or, without a local Docker daemon:
skopeo inspect docker://myoung34/github-runner:ubuntu-resolute
```

Never substitute a single-platform digest: it would break the other
architecture's build. After bumping the pin, update the digest pinned in
provider config (ADR-005).
