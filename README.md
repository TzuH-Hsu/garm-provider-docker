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
  ceiling that a pool's `extra_specs` can never escalate past:
  - `none` (default) — no DinD sidecar at all; a `docker` call inside the
    job fails fast.
  - `privileged-sidecar` — the only DinD mechanism that works on every
    NAS target (no Sysbox dependency), but the sidecar runs
    `Privileged=true`: a malicious job's own code can pivot from that
    privileged daemon to a host compromise. This is a real, direct residual
    risk, stated honestly rather than downplayed — see ADR-001's
    "Residual risk" section.
  - `sysbox-runc` — reduced-privilege alternative (no `Privileged=true`) for
    hosts that have Sysbox installed; not realistically installable on
    Synology DSM or Unraid today.
- **Persistent, repo-scoped caches.** Toolcache and pnpm-store volumes
  survive across jobs against the same repository, keyed by a normalized
  repo URL plus generation/pnpm-major salts, with an opportunistic
  age-based GC. Org/enterprise-shared caches are an explicit opt-in
  (`allow_org_shared`), off by default. See
  [ADR-003](docs/adr/ADR-003-cache-keying-and-lifecycle.md).
- **Fail-closed `extra_specs`.** The GARM-admin trust tier (pool
  `extra_specs`) is bounded by a published, `additionalProperties: false`
  JSON Schema plus config-checked ceilings: `dind_mode` by
  `allowed_dind_modes`, `runner_memory`/`dind_memory` by the configured
  limit (rejected, never clamped, if it exceeds the ceiling), and
  `extra_env` by an operator allowlist (default **empty**) plus a
  hard-reserved name set rejected even if an operator allowlists it by
  mistake. No `extra_specs` field, ever, can set a raw image reference,
  `docker_host`, or the `privileged` flag. See
  [`docs/config-reference.md`](docs/config-reference.md) and
  [ADR-005](docs/adr/ADR-005-config-and-extra-specs-schema.md).
- **v0.1.0 + v0.1.1 interface.** The one binary satisfies both GARM
  `ExternalProvider` interface versions: `GARM_INTERFACE_VERSION=v0.1.1`
  turns on self-description (`GetSupportedInterfaceVersions`,
  `GetConfigJSONSchema`, `GetExtraSpecsJSONSchema`, `ValidatePoolInfo`);
  omitting it (or setting `v0.1.0`) gets the original eight lifecycle
  methods only.

## Install

No version has been tagged yet, so no release binaries or container images
have been published. Once a `vX.Y.Z` tag is pushed,
[`.github/workflows/release.yml`](.github/workflows/release.yml) and
[`.github/workflows/images.yml`](.github/workflows/images.yml) publish, from
the same tag:

1. **Release binaries** — static `linux/amd64` and `linux/arm64` binaries
   (`CGO_ENABLED=0`, no libc dependency) with a `SHA256SUMS` checksum file,
   attached to a draft GitHub Release.
2. **Container images** — `ghcr.io/tzuh-hsu/garm-provider-docker` (the
   provider binary, packaged for convenience — see below) and
   `ghcr.io/tzuh-hsu/garm-runner-noble` (the runner image), both multi-arch
   (`linux/amd64` + `linux/arm64`) with SBOM and provenance attestations.

Until then, build from source (see `go.mod`'s `go` directive for the
required Go version):

```sh
git clone https://github.com/TzuH-Hsu/garm-provider-docker
cd garm-provider-docker
go build -o garm-provider-docker .
```

Running the provider itself as a container is a packaging convenience, not
a requirement: GARM invokes it as a plain CLI executable either way (see
`main.go`). If you do run it from `ghcr.io/tzuh-hsu/garm-provider-docker`,
mount the host Docker socket into **that** container — never into a runner
or DinD-sidecar container, which must stay isolated from the host daemon in
every mode (see Security model below).

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
    interface_version = "v0.1.1"  # or "v0.1.0" / omit for the 8-method-only surface
```

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

## Runner image

Runner containers use `ghcr.io/tzuh-hsu/garm-runner-noble`: a
`myoung34/github-runner` Ubuntu Noble base plus a custom, JIT-aware
entrypoint. See [`runner-images/noble/README.md`](runner-images/noble/README.md)
for the full entrypoint contract.

Always pin `runner_image` (and `dind_image`, and any `[flavors.*].runner_image`
override) by digest — `name@sha256:<64-hex>` — never a mutable tag.
`allow_unpinned_runner_image`/`allow_unpinned_dind_image` are dev-only escape
hatches; never set them in a real deployment. Re-resolve the current digest
with:

```sh
docker buildx imagetools inspect ghcr.io/tzuh-hsu/garm-runner-noble:latest
# or, without a local Docker daemon:
skopeo inspect docker://ghcr.io/tzuh-hsu/garm-runner-noble:latest
```

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
- **Cleanup is label-scoped, always.** Every managed resource carries
  `garm.docker/managed=true` plus controller-id/pool-id/instance-name
  labels; every cleanup path, including the manual `RemoveAllInstances`
  rescue operation, filters on those labels and never runs an unscoped
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
  at all.
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

M0 through M4 are implemented and tested on this branch, but no version has
been tagged and no binaries, container images, or upstream pull request
have been published yet. See [`docs/plan.md`](docs/plan.md) for the
milestone breakdown and [`docs/adr/`](docs/adr/) for the design decisions
behind each one.

## License

Apache-2.0 — see [`LICENSE`](LICENSE).
