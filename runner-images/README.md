# Runner images

`garm-provider-docker` publishes two GitHub Actions runner images. Both are
built `FROM` a digest-pinned `myoung34/github-runner` base, carry the same
package set, and replace the base image's own `config.sh`-based entrypoint with
the same custom, JIT-aware [`common/entrypoint.sh`](common/entrypoint.sh). See
[ADR-002](../docs/adr/ADR-002-runner-image-and-jit-delivery.md) for the design
rationale and [ADR-001](../docs/adr/ADR-001-dind-strategy.md) for the
Docker-in-Docker (DinD) readiness wait the entrypoint also implements.

| Image | Ubuntu | Default compilers | Directory |
|---|---|---|---|
| `ghcr.io/tzuh-hsu/garm-runner-noble` | 24.04 (Noble) | GCC 13, LLVM 18 (`clang-format`, `clang-tidy`) | [`noble/`](noble/README.md) |
| `ghcr.io/tzuh-hsu/garm-runner-resolute` | 26.04 (Resolute) | GCC 15, LLVM 22 (`clang-format`, `clang-tidy`) | [`resolute/`](resolute/README.md) |

## Choosing an image

GitHub moves `ubuntu-latest` from Ubuntu 24.04 to 26.04 on 2026-10-19. A
repository whose workflows say `runs-on: ubuntu-latest` and expect that
toolchain (GCC 15 as the default `gcc`, LLVM 22 for formatting and static
analysis, Python 3.14) wants the Resolute image; a repository that pins
`ubuntu-24.04`, or has not moved yet, keeps the Noble image. Both are published
on every release and both are supported.

The provider runs one image per pool. Set the config-wide default with
`runner_image`, and give pools that need the other release a
[flavor](../docs/config-reference.md#flavors) whose `runner_image` points at
it; a pool selects the flavor through `extra_specs` (`{"flavor": "resolute"}`).
Pin both by digest.

Runners on different Ubuntu releases never share a toolcache or pnpm store:
the provider keys those cache volumes on the image's `garm.docker/os-release`
label (`ubuntu-24.04` / `ubuntu-26.04`), see
[ADR-003](../docs/adr/ADR-003-cache-keying-and-lifecycle.md) (2026-10-12
amendment). A derived image inherits the label from its base; an image built
from elsewhere should set it too, or its caches are keyed on its exact image
digest and start cold whenever it is rebuilt.

## Why a custom entrypoint

`myoung34/github-runner` has no native JIT (just-in-time) runner
registration support — its stock entrypoint only knows `config.sh --token`.
GARM's metadata service, however, serves JIT credentials as three
individual files (`.runner`, `.credentials`, `.credentials_rsaparams`),
not a single `--jitconfig` blob, and the provider fetches those files
itself over HTTPS and delivers them into the container — the container
never talks to GARM directly and never receives the instance token. This
entrypoint's only job is to wait for that delivery, install the files
(or, in non-JIT mode, register with a token) correctly, and exec the
runner.

## How the provider delivers credentials

Delivery is **fetch-first**, then create/start/exec-deliver (see ADR-002
for the full description and rationale):

1. The provider **fetches** the credential files (over HTTPS; a cleartext
   `http` metadata-url is rejected unless the host is loopback) before
   anything is created, bounded by an aggregate deadline, and builds an
   in-memory tar with an atomic `.delivered` marker as its last entry.
2. The provider **creates** the runner container with a `tmpfs` mount at
   `/run/garm` (`mode=0700`, memory-backed, owned by the runner user's
   uid/gid `1001` so this entrypoint's unprivileged runner can read the
   files) — empty at this point.
3. The provider **starts** the container. This entrypoint's first action
   is to wait for `/run/garm/.delivered`, bounded by a timeout.
4. The provider **delivers** the tar by streaming it into a
   `docker exec`-run `tar -x -C /run/garm`. `docker cp` is deliberately
   **not** used: it cannot write into a running container's user tmpfs,
   because Docker resolves archive paths in a separate filesystem view
   that excludes user tmpfs mounts (moby v27.5.1
   `daemon/containerfs_linux.go`); a process started by `docker exec` runs
   in the container's own mount namespace where the tmpfs is visible.
5. This entrypoint sees the `.delivered` marker and installs the
   credentials by **symlinking** them from `/run/garm` into
   `/actions-runner` — never copying them onto the disk-backed writable
   layer — then execs the runner.

The credential files live only in the container's memory-backed tmpfs:
never in the image rootfs, never on the disk-backed writable layer, never
in `docker inspect`-visible environment or mounts, never on host disk.
They are reached from `/actions-runner` only through symlinks.

## Environment contract

Always set by the provider:

| Variable | Purpose |
|---|---|
| `JIT_CONFIG_ENABLED` | `true`/`false` - selects the JIT vs. non-JIT path below. |
| `RUNNER_WORKDIR` | Job work directory. Honored by creating/chowning it; in JIT mode the actual runtime workdir is baked into the JIT config itself, so this is best-effort here. |
| `GITHUB_URL` | Base GitHub host (e.g. `https://github.com`) - informational/connectivity-check in JIT mode, the host component of the constructed registration URL in non-JIT mode. |
| `DISABLE_RUNNER_UPDATE` | Always `true`. |
| `DOCKER_HOST` | DinD modes only - triggers the Docker readiness wait (step 2 below). Unset in `none` mode. |
| `DOCKER_SOCK_GID` | DinD modes only, alongside `DOCKER_HOST` - the supplementary GID the entrypoint adds the `runner` user to before dropping privileges, so an unprivileged `docker` call can reach the DinD sidecar's socket (fixed value `2000`; see `internal/spec/dind.go`'s `DindSocketGID`). Unset in `none` mode. |
| `RUNNER_TOOL_CACHE` | Set whenever the persistent-cache feature is enabled (`[cache].enabled`, default `true`), even for a cache-ineligible allocation - the entrypoint creates and chowns this directory before dropping privileges (`prepare_cache_dirs`) so `pnpm`/tool-setup actions can write into it as the unprivileged runner. |
| `npm_config_store_dir`, `pnpm_config_store_dir` | Both set to the same path, only when a persistent pnpm-store volume is actually mounted: pnpm 10 and earlier read the `npm_config_` name, pnpm 11 reads the `pnpm_config_` name, and each ignores the other. `npm_config_store_dir` also drives the ownership prep shared with `RUNNER_TOOL_CACHE` (`prepare_cache_dirs`). Unset when the cache is disabled or the allocation is cache-ineligible, leaving pnpm on its own default store. With npm 11, `npm_config_store_dir` makes every npm/npx call print a harmless `Unknown env config "store-dir"` warning (ADR-003, 2026-10-11 Amendment). |
| `npm_config_cache_dir`, `pnpm_config_cache_dir` | Both set to `<pnpm store path>/metadata-cache` (a sub-directory of the store mount, never its root), exactly when the two store variables above are set: pnpm 10 and earlier read the `npm_config_` name, pnpm 11 reads the `pnpm_config_` name. It persists pnpm's package-metadata cache with the store; pnpm creates the directory itself as the runner user, so `prepare_cache_dirs` needs no change. With npm 11, `npm_config_cache_dir` adds a harmless `Unknown env config "cache-dir"` warning (ADR-003, 2026-10-26 Amendment; `docs/package-store-installs.md`). |
| `GARM_DIAG_DIR` | Set when a persistent diagnostic-logs volume is mounted, so the entrypoint owns that dir (`mkdir -p` + `chown` to `runner`, `prepare_diag`) before dropping privileges. Unset when the cache is disabled or the pool is cache-ineligible. |
| `GARM_EXTERNALS_SEEDED_MARKER` | Set only when a shared, image-digest-keyed externals cache volume is mounted (ADR-003). Path to the seed-completion marker the entrypoint blocks on (`wait_for_externals_seeded`, step 5 below) before execing the runner, so a job can never start against a half-seeded externals tree. Unset when the externals cache is not in play. |

JIT mode only (`JIT_CONFIG_ENABLED=true`) — deliberately nothing else:
GARM bakes the runner's name, labels, group, and ephemeral flag into the
`.runner`/`.credentials` files themselves, so `RUNNER_ORG`/`RUNNER_REPO`/
`RUNNER_ENTERPRISE`/`RUNNER_GROUP`/`RUNNER_NAME`/`RUNNER_LABELS`/
`RUNNER_NO_DEFAULT_LABELS`/`RUNNER_EPHEMERAL` are never set and never
consulted.

Non-JIT fallback only (pools with `disable_jit_config`):

| Variable | Purpose |
|---|---|
| `RUNNER_ORG` / `RUNNER_REPO` / `RUNNER_ENTERPRISE` | Whichever applies, per the parsed `repo_url` entity scope; combined with `GITHUB_URL` to build `config.sh --url`. |
| `RUNNER_GROUP` | Passed to `config.sh --runnergroup` when set. |
| `RUNNER_NAME` | Passed to `config.sh --name`. |
| `RUNNER_LABELS` | Passed to `config.sh --labels` when set. |
| `RUNNER_NO_DEFAULT_LABELS` | `true` adds `config.sh --no-default-labels`. |
| `RUNNER_EPHEMERAL` | `true` adds `config.sh --ephemeral`. |

Entrypoint-local tuning (not part of the provider's env contract, but
overridable for local testing):

| Variable | Default | Purpose |
|---|---|---|
| `GARM_CRED_WAIT_SECONDS` | `120` | Timeout for step 1 (waiting for the `.delivered` marker). |
| `WAIT_FOR_DOCKER_SECONDS` | `120` | Timeout for step 2 (Docker daemon readiness poll), DinD modes only. |
| `GARM_EXTERNALS_WAIT_SECONDS` | `600` | Timeout for step 5 (waiting for `GARM_EXTERNALS_SEEDED_MARKER` to appear), externals-cache allocations only. Generous headroom over a cold-volume seed (bounded provider-side at 10 minutes); on the common warm path the marker is already present and this returns immediately. |
| `RUN_AS_ROOT` | unset (drops privileges via `gosu`) | Set to `true` to run the runner (and `config.sh`, with `RUNNER_ALLOW_RUNASROOT=1`) as root instead of dropping to the `runner` user. **Fails closed, not silently**: when unset and `gosu` is unavailable, the entrypoint refuses to start rather than running as root. This is a deliberate, explicit opt-out of non-root isolation - `extra_specs.extra_env` can never set it (`RUN_AS_ROOT` is hard-reserved, ADR-005), so it is reachable only by an operator who controls the container's own environment directly. |

Deliberately absent, under any circumstance: the instance/bearer token,
the metadata URL, the callback URL. There is no field or code path here
that could emit any of them.

## Entrypoint contract

1. **Delivery-marker wait, concurrent with Docker readiness** — wait for the
   single atomic marker `/run/garm/.delivered` (the provider writes it as
   the last entry of the credential tar, so it appears only once every file
   is fully delivered), bounded by `GARM_CRED_WAIT_SECONDS`. Backgrounded
   alongside step 2 below; if either fails, the other is killed rather than
   left to run out its own timeout.
2. **Docker readiness** (DinD modes only) — if `DOCKER_HOST` is set, poll
   `docker info` until ready, bounded by `WAIT_FOR_DOCKER_SECONDS`,
   independently of and concurrently with step 1. Skipped entirely when
   `DOCKER_HOST` is unset.
3. **Open-file limit, DinD socket group membership, cache/diag directory
   ownership** — still as root, before dropping privileges: raise the
   open-file soft limit to 65536 or the hard limit, whichever is lower (see
   "Open-file limit" below; best effort, never fatal); in DinD modes, add the `runner` user
   to the supplementary group owning the DinD socket (`DOCKER_SOCK_GID`) so
   an unprivileged `docker` call can reach it; then `mkdir`/`chown` any
   mounted persistent-cache directories (`RUNNER_TOOL_CACHE`,
   `npm_config_store_dir`) and the diagnostic-logs directory
   (`GARM_DIAG_DIR`) to the `runner` user — a fresh named volume mounts
   root-owned, so without this the unprivileged runner cannot write into it.
   Each step is a no-op when its corresponding env var is unset.
4. **Install credentials** —
   - **JIT**: **pre-link** `/actions-runner/.runner`,
     `.credentials`, `.credentials_rsaparams` at the tmpfs files under
     `/run/garm` (credentials stay on tmpfs, never copied onto the
     writable layer) — no `config.sh`, no `--jitconfig`. Pre-linking is safe
     here because all three JIT files are delivered up front, so the
     symlinks are never dangling.
   - **Non-JIT**: run `./config.sh --unattended --ephemeral --disableupdate
     --url … --token … --name …` (as the runner user via `gosu`, so the base
     image's own root guard is satisfied), letting it write
     `.runner`/`.credentials`/`.credentials_rsaparams` into the install dir
     normally; **then, only after `config.sh` SUCCEEDS**, move those files onto
     `/run/garm` and replace them with symlinks (a post-config move), so
     steady-state credentials are tmpfs-resident like the JIT path. This does
     **not** pre-link before `config.sh`: the real .NET runner's
     `Runner.Listener configure` reads `.credentials` in its startup
     `HostContext` constructor, and a dangling pre-created symlink — whose
     tmpfs target is not delivered in non-JIT mode (`.credentials` is created
     BY `config.sh` during registration) — crashes it (`FileNotFoundException`,
     exit 134) before registration runs (confirmed on a live daemon). A
     scrub-on-failure trap removes any credential-pattern files from the
     writable layer if `config.sh` fails.
5. **Externals-seed wait** (only when `GARM_EXTERNALS_SEEDED_MARKER` is set)
   — block until that marker exists in the read-only externals mount,
   bounded by `GARM_EXTERNALS_WAIT_SECONDS`. The provider seeds the shared,
   image-digest-keyed externals volume under an in-volume flock and writes
   the marker last, so its presence proves the tree is fully populated; this
   consumer-side wait is the backstop that makes a half-seeded start
   impossible regardless of provider-side timing (ADR-003). Skipped entirely
   when the marker env var is unset (no externals cache mounted).
6. **Exec** — drop root via `gosu runner` unless `RUN_AS_ROOT=true`, then
   `exec ./run.sh` in the foreground so it receives signals directly.
   **Fails closed**: if `gosu` is missing and `RUN_AS_ROOT` is not set, the
   entrypoint exits with an error rather than silently running as root.

The script never echoes credential file contents.

## What is in the images

**Jobs have passwordless sudo.** The `myoung34/github-runner` bases ship
`%sudo ALL=(ALL) NOPASSWD: ALL` in `/etc/sudoers` and put the `runner` user in
the `sudo` group, and the provider sets no `no-new-privileges`, so a job can run
`sudo` as root inside its own container, as on GitHub-hosted runners. This is
kept deliberately ([ADR-002](../docs/adr/ADR-002-runner-image-and-jit-delivery.md),
2026-10-12 Amendment; earlier documentation wrongly said jobs had no sudo).
Operators who want it gone can use a derived image without the rule. Still,
installing system packages on every job is slow and not what the images are
built for. The policy:

- **Languages and toolchains** (Node, Python, Go, Java, ...) come from the
  `setup-*` actions at job time, via the toolcache.
- **Commonly needed apt-level dependencies live in the images.** They do not
  try to mirror GitHub-hosted runner images; they carry the system packages
  that ordinary jobs routinely assume, and only those.
- **Single-binary tools a job installs itself go into `/usr/local`.** CI
  scripts written for GitHub-hosted runners install pinned tools into
  `/usr/local/bin` (and tool trees such as CMake into `/usr/local/lib`) and
  fall back to `sudo` only when the directory is not writable. Both
  directories are owned by the runner user in these images, so those scripts
  install without root, and keep working in a derived image without sudo.
  Only the two directories change owner, not the files the base ships in
  them, and nothing leaks between jobs: each runner container serves one job
  and is removed with its writable layer.
- **Exotic needs get a derived image**, selected through a provider flavor
  ([ADR-002](../docs/adr/ADR-002-runner-image-and-jit-delivery.md),
  ADR-005): `FROM` one of these images, `apt-get install` what the repository
  needs, publish it, and point a flavor at it. Do not grow these images for one
  repository's niche dependency.

Already in the `myoung34/github-runner` bases, and so not added here: `git`,
`git-lfs`, `gh`, `curl`, `wget`, `jq`, `yq`, `zip`, `unzip`, `xz-utils`,
`zstd`, `bzip2`, `rsync`, `openssh-client`, `gnupg`, `build-essential` (gcc,
g++, make, libc6-dev), `libssl-dev`, `pkg-config`, `python3` with `venv` and
`pip`, `perl`, `patch`, `tzdata`, `locales`, the AWS CLI, plus Node and the
Docker CLI with Compose. Both images add the following (the `apt-get` layer in
each `Dockerfile`), each present for both `linux/amd64` and `linux/arm64`:

| Packages | Why |
|---|---|
| `libnss3`, `libnspr4`, `libatk1.0-0t64`, `libatk-bridge2.0-0t64`, `libatspi2.0-0t64`, `libcups2t64`, `libdbus-1-3`, `libgbm1`, `libxkbcommon0`, `libasound2t64`, `libcairo2`, `libpango-1.0-0`, `libx11-6`, `libxcb1`, `libxext6`, `libxfixes3`, `libxcomposite1`, `libxdamage1`, `libxrandr2` | The shared libraries a headless Chrome / Chromium for Testing links against. Puppeteer, Playwright and Lighthouse CI download a browser at job time but not its system libraries. This is the set `ldd` reports as missing for Chrome for Testing on the Noble base (it matches Puppeteer's documented Debian/Ubuntu list); the same set leaves nothing missing on Resolute. `libgbm1` brings Mesa and its own LLVM runtime with it. |
| `fontconfig`, `fonts-liberation`, `fonts-noto-color-emoji` | A minimal font set so headless pages render text and emoji instead of empty boxes (screenshots, visual tests, PDF output). Not a full CJK or symbol font set; that is a derived-image job. |
| `file` | Not in the bases; build and packaging scripts call it routinely. |
| `pipx` | PEP 668 marks the system Python as externally managed on both releases, so `pip install --user` fails. CI scripts install Python CLI tools with `pipx install <tool>==<version>` into `~/.local/bin` and add that directory to `$GITHUB_PATH`. |
| `clang-format-<N>`, `clang-tidy-<N>` | C/C++ formatting and static analysis, LLVM 18 on Noble and LLVM 22 on Resolute (see each image's README for why). The unversioned `clang-format`/`clang-tidy` names point at them through `update-alternatives`, as on GitHub-hosted images, so both `clang-format` and `clang-format-<N>` resolve. This is the largest addition: about 300 MB per image for the LLVM libraries `clang-tidy` depends on. |
| `cmake`, `ninja-build` | GitHub-hosted images carry both, and repositories that do not pin their own versions expect them on `PATH`. Repositories that pin versions install them into `/usr/local`, which comes first on `PATH`. About 50 MB. |

Deliberately **not** added: `xvfb` and a headed-browser stack (a derived image,
if a repository needs headed tests), `sqlite3`, database clients, network
debugging tools, and the newer LLVM releases that are not in the Ubuntu
archive for that release (no third-party apt repositories).

## Open-file limit

Docker starts containers with an open-file soft limit of 1024 (hard limit
typically 524288), while a GitHub-hosted runner gives jobs about 65536.
Bundlers, test runners and file watchers hit 1024 quickly. The entrypoint
therefore raises the **soft** limit to 65536 (clamped to the container's hard
limit) in its root phase, before the `gosu` drop, so `run.sh` and every job step
inherit it. This is best effort: if the limit cannot be raised it logs a line
and the runner starts anyway. A runner whose soft limit is already at or above
the target is left alone, so a Docker daemon or pool that sets a higher limit
keeps it. It lives in the entrypoint rather than in a provider-side
`HostConfig.Ulimits` entry so that it also applies to any image built `FROM`
one of these images and does not depend on the provider version.

## pnpm

The `myoung34/github-runner` bases ship Node (18 on Noble, 22 on Resolute) but
**not** `pnpm`, `npm`, or `corepack`, so the provider's persistent pnpm store
(ADR-003, `npm_config_store_dir=/opt/pnpm-store`) would be inert against the
stock images. Both images install a **pinned** pnpm (`ARG PNPM_VERSION`, default `9.15.9`) lean: the
pnpm npm-registry tarball (~20 MB of JS) is extracted to `/opt/pnpm` and run by
the base image's own Node through a tiny `/usr/local/bin/pnpm` wrapper — no
second bundled Node runtime, no apt `npm`/build-toolchain bloat. `pnpm --version`
works out of the box, and with `npm_config_store_dir` set, `pnpm config get
store-dir` resolves to `/opt/pnpm-store`.

The pnpm **major** version must match the provider's `[cache].pnpm_major`
(default `9`) — the pnpm store's on-disk layout is tied to the major, so bumping
`pnpm_major` in provider config is the signal to bump `PNPM_VERSION` in both
Dockerfiles too.

## Build

The build context is this directory, so both Dockerfiles can copy the shared
entrypoint from `common/`:

```sh
docker build -t garm-runner-noble:dev    -f runner-images/noble/Dockerfile    runner-images
docker build -t garm-runner-resolute:dev -f runner-images/resolute/Dockerfile runner-images
```

`docker build runner-images/noble` on its own no longer works: that context
does not contain `common/entrypoint.sh`.
