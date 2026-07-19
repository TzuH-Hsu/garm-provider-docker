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

Because a Docker `tmpfs` mount only materializes once its container is
running, delivery is a four-step, create-then-populate sequence (see
ADR-002 for the full description):

1. The provider **creates** the runner container with a `tmpfs` mount at
   `/run/garm` (`mode=0700`, memory-backed) — empty at this point.
2. The provider **starts** the container. This entrypoint's first action
   is to poll `/run/garm` for the expected files, bounded by a timeout.
3. The provider **streams** the fetched credential files into the
   now-running container via `docker cp` of an in-memory tar stream — the
   credentials are never written to a host-side temp file or environment
   variable at any point.
4. This entrypoint's poll loop detects the files, installs them into
   `/actions-runner`, and execs the runner.

The credential files live only in the container's memory-backed tmpfs:
never in the image rootfs, never in `docker inspect`-visible environment
or mounts, never on host disk.

## Environment contract

Always set by the provider:

| Variable | Purpose |
|---|---|
| `JIT_CONFIG_ENABLED` | `true`/`false` - selects the JIT vs. non-JIT path below. |
| `RUNNER_WORKDIR` | Job work directory. Honored by creating/chowning it; in JIT mode the actual runtime workdir is baked into the JIT config itself, so this is best-effort here. |
| `GITHUB_URL` | Base GitHub host (e.g. `https://github.com`) - informational/connectivity-check in JIT mode, the host component of the constructed registration URL in non-JIT mode. |
| `DISABLE_RUNNER_UPDATE` | Always `true`. |
| `DOCKER_HOST` | DinD modes only - triggers the Docker readiness wait (step 2 below). Unset in `none` mode. |

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
| `GARM_CRED_WAIT_SECONDS` | `120` | Timeout for step 1 (credential-file poll). |
| `WAIT_FOR_DOCKER_SECONDS` | `120` | Timeout for step 2 (Docker daemon readiness poll), DinD modes only. |
| `RUN_AS_ROOT` | unset (falls back to dropping privileges) | Set to `true` to keep the runner process running as root instead of dropping to the `runner` user via `gosu`. |

Deliberately absent, under any circumstance: the instance/bearer token,
the metadata URL, the callback URL. There is no field or code path here
that could emit any of them.

## Entrypoint contract

1. **Credential wait** — poll `/run/garm` for `runner`, `credentials`,
   `credentials_rsaparams` (JIT) or `registration-token` (non-JIT),
   bounded by `GARM_CRED_WAIT_SECONDS`.
2. **Docker readiness** (DinD modes only) — if `DOCKER_HOST` is set, poll
   `docker info` until ready, bounded by `WAIT_FOR_DOCKER_SECONDS`,
   independently of and concurrently with step 1. Skipped entirely when
   `DOCKER_HOST` is unset.
3. **Install & exec** — JIT: copy the three files into `/actions-runner`
   as `.runner`/`.credentials`/`.credentials_rsaparams`, then `exec
   ./run.sh` directly (no `config.sh`, no `--jitconfig`). Non-JIT: run
   `./config.sh --unattended --ephemeral --url … --token … --name …`
   first, then `exec ./run.sh`. Both paths drop root via `gosu runner`
   unless `RUN_AS_ROOT=true`.

The script never echoes credential file contents.

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
