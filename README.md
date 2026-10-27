# garm-provider-docker

An external provider for [GARM](https://github.com/cloudbase/garm) that runs
ephemeral GitHub Actions runners as Docker containers on a single Docker
host. GARM owns GitHub App authentication, scale-set long-polling, JIT
runner-config generation, and pool scheduling; this provider is responsible
only for instance lifecycle on that one host, invoked as a subprocess
(environment variables and a stdin JSON payload in, a stdout JSON payload
and exit code out).

Target deployment: a single Docker host, NAS-first (Synology DSM, Unraid,
Raspberry Pi, and generic Linux), on both `linux/amd64` and `linux/arm64`.
Full design rationale lives in [`docs/plan.md`](docs/plan.md) and the
[Architecture Decision Records](docs/adr/).

## Features

- **Per-job isolation topology.** Every allocation gets its own bridge
  network, runner container, and workspace volume — plus, in DinD modes, its
  own DinD sidecar, socket volume, and `dind-state` volume — created together
  and torn down together. See [ADR-001](docs/adr/ADR-001-dind-strategy.md)
  and [ADR-004](docs/adr/ADR-004-teardown-and-orphan-cleanup.md).
- **Three `dind_mode` options**, capped by an operator `allowed_dind_modes`
  ceiling that a pool's `extra_specs` can never escalate past. The ceiling
  itself **defaults to `["none"]`** — fail-closed: DinD is unavailable until
  an operator both widens the ceiling and sets a digest-pinned `dind_image`,
  so no pool can obtain a privileged sidecar by omission.
  - `none` (default) — no DinD sidecar at all; a `docker` call inside the
    job fails fast.
  - `privileged-sidecar` — the only DinD mechanism that works on every
    NAS target (no Sysbox dependency), but the sidecar runs
    `Privileged=true`: a malicious job's own code can pivot from that
    privileged daemon to a host compromise. This is a real, direct residual
    risk, stated honestly rather than downplayed — see ADR-001's
    "Residual risk" section. The sidecar shares the runner's network
    namespace, as in an ARC pod, so a port a job publishes through it
    (`docker run -p`, testcontainers, `docker compose`) is reachable on the
    runner's `127.0.0.1`/`localhost`, as on a GitHub-hosted runner; the
    runner and the inner containers share one port space.
  - `sysbox-runc` — reduced-privilege alternative (no `Privileged=true`) for
    hosts that have Sysbox installed; not realistically installable on
    Synology DSM or Unraid today. Its sidecar keeps its own network
    namespace, so published ports are reached at the sidecar's container
    name (`<instance>-dind`), not on the runner's `localhost`.
- **Persistent, repo-scoped caches.** Toolcache and pnpm-store volumes
  survive across jobs against the same repository, keyed by a normalized
  repo URL plus generation/pnpm-major salts. Cache volumes are **never
  deleted automatically**: the opportunistic GC pass is *log-only* — it
  detects stale and superseded caches and logs them for operator
  visibility, and reclaiming them is a deliberate operator command. This
  is by design (ADR-003): Docker has no atomic label-qualified volume
  delete, so an inspect-then-delete-by-name is inherently racy, and the
  provider will not delete a volume it cannot prove is its own. Reclaim
  them at a quiescent moment with the controller-scoped purge:

  ```sh
  docker volume prune -a \
    --filter label=garm.docker/cache=true \
    --filter label=garm.docker/controller-id=<your-controller-id>
  ```

  (`docker volume prune` skips in-use volumes, so a warm cache held by a
  live job is never reclaimed.) The one destructive cache action the
  provider does perform is pruning *files* inside the diagnostic-log
  volume to `diagnostic_log_retention_days`, gated on full-identity
  validation, at most once every 6 hours per controller. That interval is
  recorded in one never-started `garm-diagprune-stamp-*` container per
  controller, which is safe to remove. Org/enterprise-shared caches are an explicit opt-in
  (`allow_org_shared`), off by default. NAS operators with limited storage
  should plan for this manual step. See
  [ADR-003](docs/adr/ADR-003-cache-keying-and-lifecycle.md). The pnpm store
  and the job workspace are separate volumes, so on a host whose Docker data
  root is not on XFS (reflink) or btrfs, every `pnpm install` copies the whole
  `node_modules` tree; see
  [`docs/package-store-installs.md`](docs/package-store-installs.md).
  The same volume also keeps pnpm's package-metadata cache (in a
  `metadata-cache` sub-directory), which saves several seconds per install on
  pnpm 11; see the
  [metadata cache section](docs/package-store-installs.md#the-pnpm-metadata-cache).
- **IPv4-only `localhost` in the runner.** The runner container is created
  with IPv6 disabled in its network namespace, so `::1` is never usable and
  `localhost` behaves as `127.0.0.1`, as on a VM host or a GitHub-hosted
  runner. From Docker Engine 26 a container on a network without IPv6 would
  otherwise keep `::1 localhost`, and a server bound to `localhost` (`::1`)
  is then refused by clients such as Node that skip `::1`
  (`ECONNREFUSED 127.0.0.1`); with the sysctl Docker 26+ also drops the line.
  Engine 25 and earlier already disable IPv6 there and keep a harmless, dead
  `::1 localhost` line. If the daemon cannot apply the sysctl, the provider
  logs a warning and recreates the runner once without it: on a host whose
  kernel has no IPv6 `localhost` is IPv4-only anyway, while any other refusal
  (permission, read-only `/proc/sys`, policy) is a degraded case where IPv6
  may stay enabled, which the warning and the live checks call out. Jobs that
  bind or dial `::1` explicitly do not work. See
  [ADR-002](docs/adr/ADR-002-runner-image-and-jit-delivery.md).
- **Fail-closed `extra_specs`.** The GARM-admin trust tier (pool
  `extra_specs`) is bounded by a published, `additionalProperties: false`
  JSON Schema plus config-checked ceilings: `dind_mode` by
  `allowed_dind_modes`, `runner_memory`/`dind_memory` by the configured
  limit (rejected, never clamped, if it exceeds the ceiling), and
  `extra_env` by an operator allowlist (default **empty**) plus a
  hard-reserved name set rejected even if an operator allowlists it by
  mistake. No `extra_specs` field, ever, can set a raw image reference,
  `docker_host`, or the `privileged` flag. The one exception to "unknown
  keys are rejected" is GARM's own `runner_install_template`, which GARM
  v0.2.x injects into every create and which is accepted and ignored. See
  [`docs/config-reference.md`](docs/config-reference.md) and
  [ADR-005](docs/adr/ADR-005-config-and-extra-specs-schema.md).
- **v0.1.0 + v0.1.1 interface.** The one binary satisfies both GARM
  `ExternalProvider` interface versions: `GARM_INTERFACE_VERSION=v0.1.1`
  turns on self-description (`GetSupportedInterfaceVersions`,
  `GetConfigJSONSchema`, `GetExtraSpecsJSONSchema`, `ValidatePoolInfo`);
  omitting it (or setting `v0.1.0`) gets the original eight lifecycle
  methods only.
- **Pools and scale sets.** Both work with GARM v0.2.x under either
  interface version. Under `v0.1.1`, GARM sends scale-set
  delete/get/start/stop calls without a pool ID; the provider accepts that
  (see [research.md §1.D](docs/research.md)). On GARM **v0.2.1 and
  earlier**, register the provider with `interface_version = "v0.1.0"`
  (see [Register with GARM](#register-with-garm) for why).
- **Runner status reported by the provider.** After delivering credentials,
  the provider reports the runner `installing` to GARM's instance callback,
  then `idle` once the runner logs `Listening for Jobs` (bounded wait,
  default 90 s). GARM v0.2.1 **scale sets need this**: they only move a
  runner to `active` from `idle`, so a runner left at `pending` stalls the
  scale set when it picks up a job (relaxed upstream in
  [cloudbase/garm#817](https://github.com/cloudbase/garm/pull/817), not in
  a release as of 2026-10-11). Reporting narrows the stall to a brief,
  self-clearing retry window if a job is assigned before `idle` is posted;
  it returns whenever `idle` is not posted (timeout, reporting disabled, a
  failed callback or watcher, a GARM restart mid-wait). It is on by default; the call is made by the
  provider process, so the runner container still never sees the instance
  token. See [`[status_report]`](docs/config-reference.md#status_report-runner-status-reported-to-garm).

## Install

For each pushed `vX.Y.Z` tag,
[`.github/workflows/release.yml`](.github/workflows/release.yml) and
[`.github/workflows/images.yml`](.github/workflows/images.yml) publish, from
the same tag:

1. **Release binaries** — static `linux/amd64` and `linux/arm64` binaries
   (`CGO_ENABLED=0`, no libc dependency) with a `SHA256SUMS` checksum file,
   attached to a draft GitHub Release.
2. **Container images** — `ghcr.io/tzuh-hsu/garm-provider-docker` (a
   distribution vehicle for the binary — see below) and
   `ghcr.io/tzuh-hsu/garm-runner-noble` and
   `ghcr.io/tzuh-hsu/garm-runner-resolute` (the Ubuntu 24.04 and 26.04
   runner images), all multi-arch
   (`linux/amd64` + `linux/arm64`) with SBOM and provenance attestations,
   tagged `:<tag>`. The tag push does not move `:latest`: that happens only
   when the tag's GitHub Release becomes a full release — its draft
   published as a non-pre-release, or a published pre-release later changed
   to a full release.

`v0.2.5` is the latest published release: its release binaries and
`SHA256SUMS` checksum file are downloadable from the
[v0.2.5 release page](https://github.com/TzuH-Hsu/garm-provider-docker/releases/tag/v0.2.5),
and its container images are on GHCR as both `:v0.2.5` and `:latest`. It
passed the end-to-end gate against GARM v0.2.1 on the Ubuntu 26.04 runner
image, with `dind_mode=none` and with `privileged-sidecar` (including a check
that a port published by the sidecar's daemon is reachable on the runner's
`127.0.0.1`, and a Docker Hub pull-through registry mirror); the Ubuntu 24.04
image was gated with `v0.2.3` and not re-run since (see [`docs/demo-m0.md`](docs/demo-m0.md)). `v0.2.0` does not work
with GARM v0.2.x.
(`v0.1.0`'s tag and `:v0.1.0` images still exist, but its GitHub Release
draft has since been removed.) You can also build from source instead
(see `go.mod`'s `go` directive for the required Go version):

```sh
git clone https://github.com/TzuH-Hsu/garm-provider-docker
cd garm-provider-docker
go build -o garm-provider-docker .
```

### The provider image is a delivery mechanism, not a runtime

**The provider cannot be run as a container.** GARM does not start a
container for an external provider — it `exec`s the configured
`provider_executable` **filesystem path** directly, passing the request
through environment variables and stdin (`garm-provider-common` v0.1.9:
`exec.CommandContext(ctx, providerBin)`). An OCI image reference is not a
path to an executable, so it cannot be a `provider_executable`, and there
is no hook that would make GARM run `docker run …` on your behalf. Even
setting that aside, the image's distroless `nonroot` process (uid 65532)
could not open a `root:docker` host socket.

What the image *is* good for is getting the binary onto a host, or into
your own GARM image, without a Go toolchain:

```sh
# Extract the static binary to the path GARM will exec.
cid="$(docker create ghcr.io/tzuh-hsu/garm-provider-docker:<tag>)"
docker cp "${cid}:/garm-provider-docker" /opt/garm/providers.d/garm-provider-docker
docker rm "${cid}"
chmod +x /opt/garm/providers.d/garm-provider-docker
```

If you build your own GARM image, copy the binary in at build time instead:

```dockerfile
COPY --from=ghcr.io/tzuh-hsu/garm-provider-docker:<tag> \
     /garm-provider-docker /opt/garm/providers.d/garm-provider-docker
```

Either way, the binary GARM execs runs in GARM's own process context and
needs access to the host Docker socket from there — for a containerized
GARM, that means the socket is mounted into the **GARM** container, which
is a property of your GARM deployment, not of this provider. Never mount
it into a runner or DinD-sidecar container, which must stay isolated from
the host daemon in every mode (see Security model below).

A small `docker run` exec-wrapper that would let the image act as a
`provider_executable` is a plausible future addition, but none is shipped
today, and none should be used until it has been tested end to end against
a real GARM — the credential and stdin/stdout contract is exactly the part
a naive wrapper gets wrong.

## Register with GARM

Add a `[[provider]]` section to GARM's own `config.toml` (commonly
`/etc/garm/config.toml`):

```toml
[[provider]]
  name = "docker_local"
  provider_type = "external"
  description = "Single-host Docker provider (garm-provider-docker)"

  [provider.external]
    provider_executable = "/opt/garm/providers.d/garm-provider-docker"
    config_file = "/etc/garm/garm-provider-docker.toml"
    interface_version = "v0.1.0"  # recommended on GARM <= v0.2.1; see below
```

**Which `interface_version`?** On GARM **v0.2.1 and earlier**, use
`v0.1.0`. GARM v0.2.1's own v0.1.1 wrapper inverts the error check in
`ListInstances` (`if err == nil` in `runner/providers/v0.1.1/external.go`),
so every *successful* list is reported as
`provider binary ... returned error: %!s(<nil>)` and GARM's runner
reconciliation for pools and scale sets keeps failing. This is an upstream
GARM bug, fixed by [cloudbase/garm#878](https://github.com/cloudbase/garm/pull/878)
(commit `488cf98b`, merged 2026-09-11) but not in any release as of
2026-10-11. Nothing is lost by choosing `v0.1.0`: GARM v0.2.1 never calls
the v0.1.1-only self-description commands (`GetSupportedInterfaceVersions`,
`GetConfigJSONSchema`, `GetExtraSpecsJSONSchema`, `ValidatePoolInfo`) —
you can still run them by hand with `GARM_INTERFACE_VERSION=v0.1.1` (see
[`docs/config-reference.md`](docs/config-reference.md)). Switch to
`v0.1.1` once a GARM release includes #878; this provider supports both.

**Running GARM in a container?** Start it with an init process
(`docker run --init`, or `init: true` in Docker Compose). The provider reports
runner status from a short-lived background process that must be reaped by
PID 1 when it exits; GARM's own image runs `/bin/garm` as PID 1, which does
not reap it. Without an init the provider detects this and waits for the
runner inside `CreateInstance` instead, which makes creates slower (see
[`[status_report]`](docs/config-reference.md#status_report-runner-status-reported-to-garm)).

Restart/reload GARM, confirm with `garm-cli provider list`, then create a
pool against it:

```sh
garm-cli pool add --repo <REPO> --provider-name docker_local \
  --min-idle-runners 0 --max-runners <N> --image <informational> \
  --flavor <informational> --tags <your-labels>
```

`--image`/`--flavor` are required by `garm-cli` but are **informational
only** for this provider: image and resource selection are controlled by
the provider's own config and, per pool, `extra_specs.flavor` — never by
the pool's `image`/`flavor` fields (see `docs/config-reference.md`).

## Quick start (config)

The smallest working config (`none` mode, everything else defaulted) —
see [`examples/config.minimal.toml`](examples/config.minimal.toml):

```toml
runner_image = "ghcr.io/tzuh-hsu/garm-runner-noble@sha256:<your-resolved-digest>"
```

Every other key falls back to its documented default: `dind_mode = "none"`,
`docker_host = "unix:///var/run/docker.sock"`, `storage_driver =
"overlay2"`, persistent caches enabled at their ADR-003 defaults, and
`[network].internal = false`.

See [`examples/config.full.toml`](examples/config.full.toml) for every
configurable key, commented. Both example files are loaded through the real
config loader by `internal/config/examples_test.go` on every change, so
they can never silently drift out of sync with the code.

For the full reference — every key, the `extra_specs` allowlist, the
reserved-env set, and the operator ceilings — see
[`docs/config-reference.md`](docs/config-reference.md). Both contracts are
also published as machine-readable JSON Schemas the binary emits itself
(v0.1.1):

```sh
GARM_INTERFACE_VERSION=v0.1.1 GARM_COMMAND=GetConfigJSONSchema \
  GARM_CONTROLLER_ID=x GARM_PROVIDER_CONFIG_FILE=/path/config.toml \
  ./garm-provider-docker

GARM_INTERFACE_VERSION=v0.1.1 GARM_COMMAND=GetExtraSpecsJSONSchema \
  GARM_CONTROLLER_ID=x GARM_PROVIDER_CONFIG_FILE=/path/config.toml \
  ./garm-provider-docker
```

The schemas themselves live at
[`internal/config/schema.json`](internal/config/schema.json) (provider
config) and
[`internal/extraspecs/schema.json`](internal/extraspecs/schema.json)
(`extra_specs`).

## Runner images

Two runner images are published with every release. Both are a
`myoung34/github-runner` base plus the same custom, JIT-aware entrypoint and
the same added packages (headless-Chrome libraries, fonts, `pipx`,
`clang-format`/`clang-tidy`, `cmake`, `ninja`); the runner user can install
tools into `/usr/local/bin` and `/usr/local/lib`.

| Image | Ubuntu | Toolchain | Use for |
|---|---|---|---|
| `ghcr.io/tzuh-hsu/garm-runner-noble` | 24.04 | GCC 13, LLVM 18, Python 3.12 | workflows on `ubuntu-24.04` |
| `ghcr.io/tzuh-hsu/garm-runner-resolute` | 26.04 | GCC 15, LLVM 22, Python 3.14 | workflows on `ubuntu-latest` from 2026-10-19, or `ubuntu-26.04` |

`runner_image` sets the default for every pool; a pool that needs the other
release selects a flavor that overrides it:

```toml
runner_image = "ghcr.io/tzuh-hsu/garm-runner-noble@sha256:<digest>"

[flavors.resolute]
  runner_image = "ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:<digest>"
```

and the pool's `extra_specs` is `{"flavor": "resolute"}`. Pools on different
Ubuntu releases never share a toolcache or pnpm store volume: those caches are
keyed on the image's `garm.docker/os-release` label as well as the repository
(ADR-003). See [`runner-images/README.md`](runner-images/README.md) for the
full entrypoint contract and package list.

Always pin `runner_image` (and `dind_image`, and any `[flavors.*].runner_image`
override) by digest — `name@sha256:<64-hex>` — never a mutable tag.
`allow_unpinned_runner_image`/`allow_unpinned_dind_image` are dev-only escape
hatches; never set them in a real deployment. Re-resolve the current digest
with:

```sh
docker buildx imagetools inspect ghcr.io/tzuh-hsu/garm-runner-noble:latest
docker buildx imagetools inspect ghcr.io/tzuh-hsu/garm-runner-resolute:latest
# or, without a local Docker daemon:
skopeo inspect docker://ghcr.io/tzuh-hsu/garm-runner-noble:latest
```

## Image pulls in DinD jobs

In the DinD modes every job starts with an empty image store, because each
allocation has its own sidecar and its own `dind-state` volume (ADR-001), so
every image a job pulls comes from the upstream registry every time. A
pull-through cache shared by the whole deployment, wired in with the
`registry_mirrors` setting (and `insecure_registries`, where needed), cuts
download latency and reduces both the number of upstream pulls and the
exposure to Docker Hub rate limits. See
[`docs/dind-image-pulls.md`](docs/dind-image-pulls.md).

## Security model

- **The host's Docker socket is never mounted into a runner or DinD-sidecar
  container, in any `dind_mode`.** This is structural — no code path does
  it — not merely a convention enforced by review.
- **JIT credentials never reach the container as an environment variable or
  a `docker inspect`-visible mount.** The instance token, `.runner`,
  `.credentials`, and `.credentials_rsaparams` reach the runner only through
  a memory-backed `tmpfs` (`/run/garm`) the provider streams in via
  `docker exec` after the container is created — never on host disk, never
  on the container's writable layer.
- **`extra_specs` is fail-closed end to end.** Schema-validated
  (`additionalProperties: false`), bounded by operator ceilings for
  `dind_mode`/memory, and `extra_env` is an opt-in allowlist (default empty)
  plus a hard-reserved name set (`RUNNER_*`, `DOCKER_*`, `GARM_*`,
  `RUN_AS_ROOT`, `PATH`, `LD_*`, …) rejected even if an operator allowlists
  it by mistake.
- **Cleanup is label-scoped, always.** Every **job-scoped** resource — the
  per-job network, the runner container, the workspace volume, and in DinD
  modes the sidecar plus its socket and `dind-state` volumes — carries
  `garm.docker/managed=true` plus controller-id/pool-id/instance-name
  labels. Shared resources are deliberately *not* instance-scoped: cache
  volumes carry `garm.docker/cache=true` plus controller-id (and their
  cache-kind/repo identity) but no pool-id or instance-name, precisely so
  they survive teardown and are reusable across jobs, and cache-helper
  containers (and the diag-prune stamp) carry `role=cache-helper`
  (`role=diag-prune-stamp`) with no instance-name so the
  teardown/sweep predicate structurally excludes them and `ListInstances`
  never reports them (ADR-003/ADR-004). Every cleanup path, including the
  manual `RemoveAllInstances` rescue operation, filters on the labels
  appropriate to what it is collecting and never runs an unscoped
  `docker system prune` or equivalent.
- **`privileged-sidecar` is honest about its limits.** It protects other
  jobs' data and contains accidents, but a malicious job's code inside a
  privileged DinD daemon can reach the underlying host (see ADR-001's
  "Residual risk" section). Prefer `sysbox-runc` or `none` for genuinely
  untrusted job code; restrict `privileged-sidecar` to trusted repositories
  if you use it at all.
- **Operators must ensure the host Docker daemon itself never listens on
  TCP** (`dockerd -H tcp://…` or an equivalent `daemon.json` `hosts` entry).
  A TCP-exposed host socket bypasses every per-job containment guarantee in
  this project regardless of any other setting.

## NAS notes

- **Synology DSM**: the host filesystem is commonly btrfs (sometimes
  overlay-on-overlay), which makes DinD's in-container storage-driver
  autodetection unreliable. Always set `storage_driver` explicitly
  (`overlay2` is the default; `vfs` is the slower, more compatible
  fallback) rather than relying on autodetection. Sysbox has no supported
  install path on DSM, so `sysbox-runc` is not available there —
  `privileged-sidecar` is the only working DinD mechanism if you need DinD
  at all. btrfs can also make `pnpm install` cheap, because it supports
  reflinks (kernel 5.18 or newer); see
  [`docs/package-store-installs.md`](docs/package-store-installs.md).
- **Unraid**: the same Sysbox-availability gap as DSM — `sysbox-runc` is not
  realistically installable; `privileged-sidecar` or `none` are the
  practical choices.
- **Raspberry Pi / generic `linux/arm64` hosts**: fully supported —
  `linux/arm64` binaries and images are built by the same release workflow
  as `linux/amd64`, not a separate or lagging build. Size the Docker
  daemon's `default-address-pools` for your expected concurrent-job count:
  each allocation, in every mode, consumes one per-job network from that
  pool, and pool exhaustion is a real, host-level capacity ceiling
  independent of runner/DinD resource limits.
- Whichever host you use, never configure the Docker daemon to listen on
  TCP — see Security model above.

## Status

M0 through M4 are implemented and tested on this branch. `v0.2.5` is the
latest published release, with release binaries and container images
(`:v0.2.5` and `:latest`) on GHCR; `v0.1.0`'s tag and `:v0.1.0` images
still exist, though its GitHub Release draft has since been removed. No
upstream pull request has been published yet. See
[`docs/plan.md`](docs/plan.md) for the milestone breakdown and
[`docs/adr/`](docs/adr/) for the design decisions behind each one.

## License

Apache-2.0 — see [`LICENSE`](LICENSE).
