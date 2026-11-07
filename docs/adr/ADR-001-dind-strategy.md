# ADR-001: Docker-in-Docker strategy

Status: Accepted

## Context

`garm-provider-docker` is an external provider for
[GARM](https://github.com/cloudbase/garm). GARM owns GitHub authentication, scale-set polling,
JIT runner configuration and pool scheduling. This provider creates and destroys runner containers on
one Docker host. The call contract is in [`garm-contract.md`](../garm-contract.md).

Many CI jobs need Docker inside the job (Docker-in-Docker, DinD). Sysbox is the usual way to run an
unprivileged nested daemon, but it cannot be installed on every host, so the design cannot depend on it.
The model this provider enforces has three trust tiers. The operator's provider config is the most
trusted. A pool's `extra_specs`, set by a GARM admin, is narrower. The workflow payload is untrusted and
never decides anything privileged. One rule holds in every configuration: a runner, or the job it runs,
must never reach the host's Docker socket.

The provider process itself talks to the host daemon, so it is as trusted as GARM and effectively has root
on the host. This ADR is about what the containers it creates can reach.

## Decision

Three DinD modes share one per-allocation topology: `none`, `privileged-sidecar` and `sysbox-runc`.
DinD is off by default. `dind_mode` defaults to `none`, and `allowed_dind_modes`, the operator's ceiling on
what any pool may request, defaults to `["none"]`. To use DinD an operator widens the ceiling and sets a
digest-pinned `dind_image`. `dind_mode` must itself be a member of `allowed_dind_modes`, so a config that
selects a DinD mode without widening the ceiling fails to load. A pool's `extra_specs.dind_mode` can select
a mode inside the ceiling and never beyond it. If a pool selects a DinD mode on a host with no
`dind_image`, `CreateInstance` rejects it before any Docker call.

`privileged-sidecar` is the mode available on every host, so it is the mechanism to use when DinD is
wanted and Sysbox is not available. It is never enabled by omission. The host socket is never mounted
into any container in any mode.

DinD runs as a sidecar container that shares a job-scoped Unix socket volume with the runner, as in ARC's
`gha-runner-scale-set`. Each allocation owns these resources, created by `CreateInstance` and removed by
`DeleteInstance` ([ADR-004](ADR-004-teardown-and-orphan-cleanup.md)):

- a labelled bridge network (all modes);
- the runner container (all modes);
- a workspace volume (all modes);
- in DinD modes, the sidecar, a socket volume and a `dind-state` volume at `/var/lib/docker`.

In DinD modes the sidecar also mounts the workspace at the runner's workspace path, and, when the
externals cache is enabled, the shared externals volume read-only at the runner's externals path (see
"Container jobs"). Persistent caches are described in [ADR-003](ADR-003-cache-keying-and-lifecycle.md).

### `privileged-sidecar`

The sidecar runs `dockerd --host=unix:///run/docker.sock --storage-driver=<driver> --group=2000` with
`DOCKER_TLS_CERTDIR=""`. TLS is off because the daemon listens only on the shared socket volume, never on
TCP. The runner mounts the same volume at `/run` and sets `DOCKER_HOST=unix:///run/docker.sock`. Group
2000 owns the socket, and the entrypoint adds the runner user to it, because the runner and `docker:dind`
base images number their `docker` groups differently. The runner's entrypoint polls `docker info` for up
to 120 seconds before it starts the runner.

The socket volume is mounted at `/run` and not at `/var/run`. On Debian, Ubuntu and Alpine, `/var/run` is a
symlink to `/run`, and a volume mounted there would hide the credential tmpfs at `/run/garm`
([ADR-002](ADR-002-runner-image-and-jit-delivery.md)). Mounting the volume at the parent lets Docker mount
`/run` first and the tmpfs on top. This is also what ARC does. It concerns the nested daemon's socket
only; `docker_host` in the provider config is the provider's own connection to the host daemon.

**Shared network namespace.** The sidecar joins the runner's network namespace
(`HostConfig.NetworkMode = container:<runner ID>`). A port that the inner daemon publishes (`docker run -p`,
testcontainers, Compose `ports:`) is then reachable on the runner's `127.0.0.1`, as on a GitHub-hosted
runner or in an ARC pod. With separate namespaces, such jobs fail with `ECONNREFUSED`. The runner owns the
namespace and the sidecar joins it, for four reasons:

1. The runner is what GitHub sees. The sidecar carries the `dind_memory` limit and is more likely to be
   OOM-killed. If it owned the namespace, its death would cut the runner off the network and lose the job.
2. A container that joins another's namespace takes the `/etc/hosts`, `/etc/resolv.conf` and hostname of the
   container that created it. The runner keeps the files the daemon writes for it, including the IPv4-only
   `localhost` entry from ADR-002.
3. `net.*` sysctls are per namespace. The runner's `disable_ipv6` sysctl and its single retry without it
   stay where they are, applied once by the namespace owner. The sidecar sets no sysctl.
4. The runner stays the single endpoint on the job network, so its attachment checks are unchanged.

Keeping separate namespaces and pointing jobs at the sidecar by name would need changes in every job and
tool, and would not fix code that hard-codes `127.0.0.1`. A TCP `DOCKER_HOST` would break the no-TCP rule.

The daemon refuses to start a container that joins a namespace whose owner is not running. `CreateInstance`
therefore pulls the dind image first, then creates and starts the runner (including its IPv6 retry), then
creates and starts the sidecar, delivers the credentials, and verifies that both containers run. The
runner's Docker wait runs in parallel with its credential wait, which covers a sidecar that starts a moment
later. A failure at any step rolls back the whole attempt.

Teardown order is unchanged: runner, sidecar, volumes, network. Removing the namespace owner first is
allowed while the joined sidecar exists, and the network removal does not wait for the sidecar because the
sidecar holds no endpoint on it. A runner removed out of band leaves a sidecar with no external
connectivity, and `DeleteInstance` still removes it by label. Neither container has a restart policy.

Inside the shared namespace, `dockerd` creates `docker0`, enables IP forwarding and installs its NAT rules,
and `docker-proxy` serves published ports on `0.0.0.0` there. Nested bridge networking, `docker run -p` with
fixed and ephemeral ports, and `127.0.0.1` and `localhost` from the runner (as root and as the runner user)
were checked against `dockerd` 29.9.0 with the runner's `disable_ipv6=1`. IPv6 inside nested networks is not
supported, and the job network carries no IPv6 anyway. The runner and the inner daemon now share one port
space, so `docker run -p 8080:...` fails with "address already in use" if a runner process listens on 8080.
This matches GitHub-hosted runners and ARC. The Actions runner listens on no port.

Isolation analysis: the trust boundary does not move, because the runner and its sidecar belong to the same
job. The privileged sidecar now holds every capability over the runner's network namespace, including
observing its traffic. Job code already controls that daemon through `DOCKER_HOST`, and the residual risk
below already accepts that it can reach the host, so nothing new is exposed. The runner container stays
unprivileged with Docker's default capabilities. A nested container run with `--network host` lands in the
runner's namespace, as on a GitHub-hosted runner. Job-to-job isolation and the no-host-socket rule are
unchanged.

### `sysbox-runc`

Same topology, with two `HostConfig` differences on the sidecar: `Runtime = "sysbox-runc"` with
`Privileged = false`, and a network namespace of its own on the job network. Sysbox runs the sidecar in a
user namespace in which its root has no `CAP_NET_ADMIN` over the runner's namespace, so joining that namespace
would stop `dockerd` from creating `docker0`. In this mode, ports published by the inner daemon are not
reachable on the runner's `localhost`. A job reaches them at the sidecar's container name (`<instance>-dind`)
on the job network. If the daemon has no `sysbox-runc` runtime, the error says so. This mode is not covered by
the live gate.

### `none`

No sidecar, socket volume or `dind-state` volume, and `DOCKER_HOST` stays unset. Any `docker` call in the job
fails fast. The network, workspace and caches are still provisioned.

### Storage driver

The provider always creates a dedicated `dind-state` volume and passes an explicit `--storage-driver`
(`overlay2` by default, `vfs` as the slower, more compatible fallback). Autodetection inside a nested
container is unreliable on some host filesystems. A pool may choose between the two through `extra_specs`.
The workflow cannot change it.

### Registry mirrors

`registry_mirrors` and `insecure_registries` in the provider config become `--registry-mirror` and
`--insecure-registry` flags appended to the three fixed ones. They are typed lists and not a `daemon.json`
passthrough, because a free-form file could set `hosts` (a TCP listener), `storage-driver` or `group`, which
the provider owns, and `dockerd` refuses to start when an option appears both as a flag and in the file. They
are provider config only, since a mirror belongs to the host's network, so a pool admin cannot redirect a
sidecar's pulls. The `dind-state` volume is still fresh for every job. A custom `dind_image` whose
`daemon.json` sets the same options conflicts with these flags. See [`dind-image-pulls.md`](../dind-image-pulls.md).

### Container jobs

A workflow job with `container:` runs inside a container the runner creates through the sidecar's daemon.
The runner asks the daemon to bind-mount `/actions-runner/_work` as `/__w` and `/actions-runner/externals`
read-only as `/__e`. The daemon resolves those sources in its own filesystem, which is the sidecar's.
Mounting the workspace in the sidecar covers `/__w`. Mounting the externals volume there, read-only like
the runner's own mount, covers `/__e`, which holds the Node runtime that JavaScript actions need. The volume
is shared across repositories, so a writable mount would let one job poison runtimes other repositories
execute ([ADR-003](ADR-003-cache-keying-and-lifecycle.md)). With `[cache].enabled = false` there is no
externals volume, `/__e` stays empty, and JavaScript actions cannot run in a container job. Plain `run:`
steps are unaffected.

The runner also binds its tool cache directory as `/__t`. The toolcache volume stays out of the sidecar, so the
daemon creates an empty directory for that source there. A container job starts with an empty tool cache, and
what it installs goes away with the sidecar. This costs cache hits, not correctness, and it keeps a repository's
toolcache out of the privileged sidecar.

### Job network

Every allocation gets its own bridge network, drawn from the daemon's `default-address-pools`. The pool is
finite, so it limits how many jobs can run at once, independently of CPU and memory. Size
`default-address-pools` for the concurrency you plan. When the pool is exhausted, `CreateInstance` fails with
the daemon's error text on stderr and exit code 1. The provider does not classify the error, and a failed
create cannot carry a `provider_fault`, because the library drops the result when an error is returned.

`[network].internal` defaults to `false`. With Docker's `internal` flag a network has no external route at
all, so a runner could not reach `api.github.com` and a sidecar could not pull images. Job-to-job isolation
comes from each allocation having its own network, not from this flag. The consequence is that egress is
unrestricted: a job can reach other hosts on the LAN, the host's own services through the network gateway,
and any metadata endpoint reachable from the host. An operator who needs less sets `internal = true`, which
allows no egress at all. A forward-proxy sidecar with an allowlist would give both properties, and it is not
built. The host daemon must also not listen on TCP (`dockerd -H tcp://...` or a `hosts` entry). A
TCP-exposed daemon bypasses every per-job guarantee, and with `internal = false` this requirement carries
that weight together with the rule that `extra_specs` can never set `DOCKER_HOST`
([ADR-005](ADR-005-config-and-extra-specs-schema.md)).

### Scope

One Docker host, with Linux runners on `linux/amd64` or `linux/arm64`. Windows and macOS runners, rootless
DinD and multi-host orchestration are out of scope. Rootless DinD needs `uidmap`, cgroup v2 with systemd and
an outer container that is usually privileged anyway.

## Rationale

`privileged-sidecar` is the only mechanism that works wherever Docker does, so it is the one to use when
Sysbox is not available. DinD is opt-in at two levels because a privileged sidecar is a host-compromise
path. Without the ceiling, a GARM admin could switch a pool to `privileged-sidecar` on a host the operator
meant to keep at `none`. Keeping the host socket out of every mount table makes the "no host socket" rule
structural: no code path mounts `/var/run/docker.sock` into a job-scoped container.

## Alternatives considered

- A privileged runner with Docker inside it and no sidecar: couples the two lifecycles and has weaker
  isolation than a sidecar with its own crash boundary.
- Host socket passthrough: violates the central rule.
- Shared static networks or volumes across jobs: removes per-job isolation.
- Rootless DinD: see "Scope".

## Consequences

- Teardown covers five resource kinds in a fixed order ([ADR-004](ADR-004-teardown-and-orphan-cleanup.md)).
- The runner image and the sidecar image must both be digest-pinned and kept up to date.
- Concurrency is bounded by Docker's address pools.
- No privileged container exists on a host until the operator opts in.

### Residual risk

Per-job isolation (own network, volumes and sidecar) protects other jobs' data from a compromised job, and
the host socket stays unreachable in every mode. But in `privileged-sidecar` mode the job's code controls a
**privileged** daemon through `DOCKER_HOST`. It can run a nested privileged container, reach devices the
sidecar can reach, mount the host's root filesystem through a crafted container, and take over the host. None
of that uses the host's Docker socket, and all of it is a direct host-takeover path. **A privileged
sidecar is root-equivalent on the host for any job that runs in it.** It contains accidents, not an
adversarial job.

- This provider is meant for repositories the operator controls. For untrusted job code (fork pull requests,
  multiple tenants, contributors the operator does not trust), use a provider that runs jobs in VMs.
- Within this provider, `sysbox-runc` removes `Privileged=true` from the sidecar and narrows the risk
  considerably. `none` removes the sidecar. Both still leave jobs in containers that share the host kernel.
- Where neither is possible, restrict `privileged-sidecar` to pools that run only trusted repositories and
  branches. This is an operator policy; the provider cannot enforce it, because it does not know the job.

## Open questions

- Whether `cgroupns=host` is needed on older kernels.
- Whether the provider should also probe the sidecar's readiness, for faster failure reports.
- Whether a single-container Sysbox variant is worth adding, and whether `sysbox-runc` can offer published ports
  on the runner's `localhost`. Neither can be checked without a Sysbox install.
- How the `dockerd` version inside the sidecar image should be pinned and tracked.
- Whether the provider should refuse to start, or warn, when the host daemon appears to listen on TCP.
- Whether to build the forward-proxy egress allowlist.
