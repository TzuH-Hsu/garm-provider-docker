# How it works

GARM owns GitHub authentication, scale-set long-polling, JIT configuration and
scheduling. This provider handles instance lifecycle on one Docker host. GARM
invokes it as a subprocess, with environment variables and a JSON payload on
stdin, and reads JSON from stdout. See [the GARM provider contract](garm-contract.md).

## Job resources

Each allocation gets its own bridge network, runner container and workspace
volume, plus a DinD sidecar, socket volume and `dind-state` volume in the DinD
modes. They are created and removed together. Every job-scoped resource carries
`garm.docker/managed=true` and controller, pool and instance labels, and cleanup
always filters on labels. See
[ADR-004](adr/ADR-004-teardown-and-orphan-cleanup.md).

## DinD modes

`dind_mode` is capped by the operator's `allowed_dind_modes`, which `extra_specs`
cannot exceed. It defaults to `["none"]`, and DinD also requires a digest-pinned
`dind_image`.

- **`none`:** no sidecar. A `docker` call in the job fails fast.
- **`privileged-sidecar`:** works without Sysbox, but the sidecar is
  `Privileged=true`, so malicious job code can reach the host. It shares the
  runner's network namespace, so a port a job publishes is reachable on the
  runner's `127.0.0.1`, as on a GitHub-hosted runner. The runner and inner
  containers share one port space.
- **`sysbox-runc`:** a reduced-privilege alternative for hosts with Sysbox. The
  sidecar keeps its own network namespace, so published ports are reached at
  `<instance>-dind`. It has no live test coverage.

See [ADR-001](adr/ADR-001-dind-strategy.md).

### Container jobs

A job with `container:` runs in a container the runner creates through the
sidecar's daemon, which resolves `/__w` and `/__e` in the sidecar's filesystem.
The sidecar therefore mounts the runner's workspace volume, and the shared
externals volume read-only, at the runner's paths. The externals volume is part
of the cache: with `[cache].enabled = false`, JavaScript actions cannot run in a
container job, although plain `run:` steps still work.

### Image pulls

Each DinD job starts with an empty image store. A shared pull-through cache,
set with `registry_mirrors`, reduces downloads and Docker Hub rate limiting. See
[Image pulls in DinD jobs](dind-image-pulls.md).

## Caches

Toolcache and pnpm store volumes persist across jobs for the same repository,
keyed by normalized repository URL, generation, pnpm major version and the
image's `garm.docker/os-release` label. Org-shared caches are opt-in
(`allow_org_shared`).

Cache volumes are never deleted automatically, because Docker has no atomic
label-qualified volume delete. The garbage collection pass only logs what it
finds. Reclaim space at a quiet moment:

```sh
docker volume prune -a \
  --filter label=garm.docker/cache=true \
  --filter label=garm.docker/controller-id=<your-controller-id>
```

`docker volume prune` skips volumes in use. The provider does prune files inside
the diagnostic-log volume to `diagnostic_log_retention_days`, at most once every
6 hours per controller. That interval is recorded in a never-started
`garm-diagprune-stamp-*` container, and `ListInstances` throttles its orphan sweep
(3 minutes) and cache collection (5 minutes) with `garm-sweep-stamp-*` and
`garm-cachegc-stamp-*` containers. These are safe to remove. See
[ADR-003](adr/ADR-003-cache-keying-and-lifecycle.md) and
[package store installs](package-store-installs.md).

## Credentials

The instance token, `.runner`, `.credentials` and `.credentials_rsaparams` reach
the runner only through a memory-backed `tmpfs` at `/run/garm`, streamed in with
`docker exec` after the container is created. They are not in the environment,
on host disk or in the container's writable layer. See
[ADR-002](adr/ADR-002-runner-image-and-jit-delivery.md).

## IPv4-only localhost

The runner is created with IPv6 disabled in its network namespace, so `localhost`
behaves as `127.0.0.1`, as on a GitHub-hosted runner. Otherwise, Docker Engine 26
and later can keep `::1 localhost` on a network without IPv6, and a server bound
to `::1` is refused by clients such as Node that skip it. If the daemon cannot
apply the sysctl, the provider logs a warning and recreates the runner once
without it. Jobs that bind or dial `::1` explicitly do not work.

## extra_specs

Pool `extra_specs` are checked against a published JSON Schema with
`additionalProperties: false`. `dind_mode`, `runner_memory` and `dind_memory` are
limited by operator ceilings and rejected, not clamped, above them. `extra_env`
needs an operator allowlist (empty by default), and reserved names are rejected
even if allowlisted. No field can set a raw image, `docker_host` or `privileged`.
GARM's own `runner_install_template`, which GARM v0.2.x injects, is accepted and
ignored. See [ADR-005](adr/ADR-005-config-and-extra-specs-schema.md).

Print the schemas with:

```sh
GARM_INTERFACE_VERSION=v0.1.1 GARM_COMMAND=GetConfigJSONSchema \
  GARM_CONTROLLER_ID=x GARM_PROVIDER_CONFIG_FILE=/path/config.toml \
  ./garm-provider-docker

GARM_INTERFACE_VERSION=v0.1.1 GARM_COMMAND=GetExtraSpecsJSONSchema \
  GARM_CONTROLLER_ID=x GARM_PROVIDER_CONFIG_FILE=/path/config.toml \
  ./garm-provider-docker
```

## Interface versions

One binary serves both GARM interface versions. `GARM_INTERFACE_VERSION=v0.1.1`
adds `GetSupportedInterfaceVersions`, `GetConfigJSONSchema`,
`GetExtraSpecsJSONSchema` and `ValidatePoolInfo`. Without it, or with `v0.1.0`,
only the original eight lifecycle methods are available. Under v0.1.1, GARM sends
scale-set delete, get, start and stop calls without a pool ID, and the provider
accepts that.

Use `v0.1.0` on GARM v0.2.1 and earlier. GARM v0.2.1's v0.1.1 wrapper reports every
successful `ListInstances` as an error, which breaks reconciliation
([cloudbase/garm#878](https://github.com/cloudbase/garm/pull/878)). Nothing is lost,
because GARM v0.2.1 never calls the v0.1.1-only commands.

## Runner status

After delivering credentials, the provider reports the runner `installing` to
GARM's instance callback, then `idle` once the runner logs `Listening for Jobs`,
waiting 90 seconds by default. The provider process makes the call, so the runner
container never sees the instance token.

GARM v0.2.1 scale sets need this: they move a runner to `active` only from `idle`,
so a runner left at `pending` stalls the scale set when it picks up a job. Upstream
[cloudbase/garm#817](https://github.com/cloudbase/garm/pull/817) removes the
restriction. Reporting narrows the stall to a brief retry if a job is assigned
before `idle` is posted. The stall returns if `idle` is not posted: on a timeout,
with reporting disabled, after a failed callback or watcher, or if GARM restarts
during the wait. See
[`[status_report]`](config-reference.md#status_report-runner-status-reported-to-garm).
