# garm-runner-noble

The GitHub Actions runner image used by `garm-provider-docker`'s runner
containers, built `FROM` a digest-pinned `myoung34/github-runner:ubuntu-noble`
(Ubuntu 24.04) with a custom `entrypoint.sh` that replaces the base image's
own `config.sh`-based entrypoint entirely. See
[ADR-002](../../docs/adr/ADR-002-runner-image-and-jit-delivery.md) for the
full design rationale and [ADR-001](../../docs/adr/ADR-001-dind-strategy.md)
for the Docker-in-Docker (DinD) readiness wait this entrypoint also
implements.

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
| `GARM_DIAG_DIR` | M2-W2 only - set when a persistent diagnostic-logs volume is mounted, so the entrypoint owns that dir (`mkdir -p` + `chown` to `runner`) before dropping privileges. Unset when the cache is disabled or the pool is cache-ineligible. |

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
| `RUN_AS_ROOT` | unset (drops privileges via `gosu`) | Set to `true` to run the runner (and `config.sh`, with `RUNNER_ALLOW_RUNASROOT=1`) as root instead of dropping to the `runner` user. When unset and `gosu` is unavailable, the entrypoint fails closed rather than running as root. |

Deliberately absent, under any circumstance: the instance/bearer token,
the metadata URL, the callback URL. There is no field or code path here
that could emit any of them.

## Entrypoint contract

1. **Delivery-marker wait** — wait for the single atomic marker
   `/run/garm/.delivered` (the provider writes it as the last entry of the
   credential tar, so it appears only once every file is fully delivered),
   bounded by `GARM_CRED_WAIT_SECONDS`.
2. **Docker readiness** (DinD modes only) — if `DOCKER_HOST` is set, poll
   `docker info` until ready, bounded by `WAIT_FOR_DOCKER_SECONDS`,
   independently of and concurrently with step 1. Skipped entirely when
   `DOCKER_HOST` is unset.
3. **Install & exec** —
   - **JIT**: **pre-link** `/actions-runner/.runner`,
     `.credentials`, `.credentials_rsaparams` at the tmpfs files under
     `/run/garm` (credentials stay on tmpfs, never copied onto the
     writable layer), then `exec ./run.sh` directly (no `config.sh`, no
     `--jitconfig`). Pre-linking is safe here because all three JIT files are
     delivered up front, so the symlinks are never dangling.
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
     writable layer if `config.sh` fails, then `exec ./run.sh`.
   - Both paths drop root via `gosu runner` unless `RUN_AS_ROOT=true`, and
     **fail closed**: if `gosu` is missing and `RUN_AS_ROOT` is not set,
     the entrypoint exits with an error rather than silently running as
     root.

The script never echoes credential file contents.

## pnpm (M2-W2)

The base `myoung34/github-runner` image ships Node but **not** `pnpm`, `npm`, or
`corepack`, so the provider's persistent pnpm store (ADR-003,
`npm_config_store_dir=/opt/pnpm-store`) was inert against the stock image. This
image installs a **pinned** pnpm (`ARG PNPM_VERSION`, default `9.15.9`) lean: the
pnpm npm-registry tarball (~20 MB of JS) is extracted to `/opt/pnpm` and run by
the base image's own Node through a tiny `/usr/local/bin/pnpm` wrapper — no
second bundled Node runtime, no apt `npm`/build-toolchain bloat. `pnpm --version`
works out of the box, and with `npm_config_store_dir` set, `pnpm config get
store-dir` resolves to `/opt/pnpm-store`.

The pnpm **major** version must match the provider's `[cache].pnpm_major`
(default `9`) — the pnpm store's on-disk layout is tied to the major, so bumping
`pnpm_major` in provider config is the signal to bump `PNPM_VERSION` here too.

## Build

```sh
docker build -t garm-runner-noble:dev runner-images/noble
```

## Digest-pin policy

The base image is pinned by digest, not by mutable tag, so a rebuild only
happens when this repository explicitly bumps the pin (or the
`GH_RUNNER_VERSION` the base image itself is built with). The digest
above is the **linux/amd64 platform manifest** resolved from the
`ubuntu-noble` tag's manifest list — correct for M0's linux/amd64-only
scope (`docs/plan.md` M0). At M4 (multi-arch release), re-resolve to
either a manifest-list digest or move to a per-platform build matrix so
`linux/arm64` is covered too. Re-resolve the current digest with:

```sh
docker buildx imagetools inspect myoung34/github-runner:ubuntu-noble
# or, without a local Docker daemon:
skopeo inspect docker://myoung34/github-runner:ubuntu-noble
```

Bumping the digest (or `GH_RUNNER_VERSION`) is an operator action: after
a rebuild, update the digest pinned in provider config (ADR-005) — image
selection is config-only, never something a pool's `extra_specs` can
override.
