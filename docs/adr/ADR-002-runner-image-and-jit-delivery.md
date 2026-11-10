# ADR-002: Runner image and JIT configuration delivery

Status: Accepted

## Context

GARM generates just-in-time (JIT) runner configuration and expects the provider to get it onto the
instance it creates ([`garm-contract.md`](../garm-contract.md)). The provider receives an instance token
on stdin. It must turn that into a registered runner without letting GARM's credentials (the token, the
metadata and callback URLs, or the GitHub App key behind them) reach the container's environment or
filesystem. The acceptance test is that `docker inspect` of a running runner shows no host Docker socket,
no GARM credential and no GitHub App key, in either its environment or its mounts.

Two base images are realistic. `myoung34/github-runner` includes common build tools, and its entrypoint only
runs `config.sh --token`. The official `ghcr.io/actions/actions-runner` is a minimal image with no
entrypoint and no build tools, so every job would install its own. Neither has an entrypoint that waits for
credentials the provider delivers.

## Decision

### Images

The repository builds its own runner images, each `FROM` a digest-pinned `myoung34/github-runner` and sharing
one entrypoint, `runner-images/common/entrypoint.sh`:

| Directory | Image | Base | Default toolchain |
|---|---|---|---|
| `runner-images/noble/` | `ghcr.io/tzuh-hsu/garm-runner-noble` | Ubuntu 24.04 | GCC 13, LLVM 18 |
| `runner-images/resolute/` | `ghcr.io/tzuh-hsu/garm-runner-resolute` | Ubuntu 26.04 | GCC 15, LLVM 22 |

Both are published as multi-arch (`linux/amd64`, `linux/arm64`) manifests. The build context is
`runner-images/` and each Dockerfile is selected with `-f`, because Docker cannot `COPY` from outside the
context and a symlink to the shared script is not followed. Image selection is config-only: `runner_image`
is the default and a `[flavors.*]` entry points a pool at the other image. `extra_specs` never carries an
image reference, so image provenance stays with the operator. Each image carries a `garm.docker/os-release`
label that keys the toolcache and pnpm volumes ([ADR-003](ADR-003-cache-keying-and-lifecycle.md)).

### The provider fetches the credentials

The instance token never enters the container. The provider fetches the three credential files from
GARM's metadata service itself: `.runner`, `.credentials` and `.credentials_rsaparams`, one request each.
It requires https (plain http only to a loopback host), refuses URLs with userinfo, and trusts the
`ca-cert-bundle` from the payload instead of only the host's trust store, so private-CA deployments work.
Cleartext is forbidden because the token is a Bearer credential.

The sequence is fetch first, then create, start and deliver, because a tmpfs exists only once its container
runs and because fetching first removes the race between a slow fetch and the entrypoint's credential wait.

1. **Fetch** the files before creating the runner, within a 60 second budget and with bounded retries. A
   failure rolls back the network and volumes made so far. Build an in-memory tar whose last entry is the marker `.delivered`.
2. **Create** the runner with a memory-backed tmpfs at `/run/garm` (`mode=0700`, 16 MiB, owned by uid and
   gid 1001, the runner user). Pull the image first if it is missing.
3. **Start** it. The entrypoint's first action is to wait for `/run/garm/.delivered`, for up to 120 seconds
   (`GARM_CRED_WAIT_SECONDS`).
4. **Deliver** by streaming the tar into a `docker exec` of `tar -x -p -C /run/garm`, and require exit code
   0. `docker cp` cannot be used: Docker resolves its archive paths in a separate filesystem view that
   excludes a running container's user tmpfs mounts. A process started by `docker exec` runs in the
   container's own mount namespace, where the tmpfs is visible. Because `tar` creates entries in archive
   order, the marker appears only after every credential file is written, so the entrypoint never sees a
   partial set. The files are never written to host disk.
5. **Verify** that the runner (and, in DinD modes, the sidecar) is still running before reporting success.

The entrypoint then installs the files and starts the runner. With JIT it symlinks `.runner`,
`.credentials` and `.credentials_rsaparams` in the install directory to the tmpfs files, never copying them
to the writable layer, and runs `run.sh` directly. No `config.sh` and no `--jitconfig` are involved, because
GARM serves the files and not a single blob. In DinD modes the Docker readiness wait runs in parallel with
the credential wait, and the runner starts after both.

JIT configuration is valid for about an hour, which is ample, since delivery follows the fetch within
seconds.

**Resulting properties.** The credential files exist only in the container's memory-backed tmpfs: never in
the image, the writable layer, a `docker inspect`-visible variable or host disk. The tmpfs is anonymous and
per container, so no other container can mount or see it. The runner user can read the files, so job code
can read its own runner's credentials. They identify one single-job runner and nothing else.

### Environment

Always set: `JIT_CONFIG_ENABLED`, `RUNNER_WORKDIR`, `GITHUB_URL`, `DISABLE_RUNNER_UPDATE=true`, and in DinD
modes `DOCKER_HOST` and `DOCKER_SOCK_GID`. Cache variables are set when caches are mounted
([ADR-003](ADR-003-cache-keying-and-lifecycle.md)). With JIT the provider sets none of `RUNNER_ORG`,
`RUNNER_REPO`, `RUNNER_ENTERPRISE`, `RUNNER_GROUP`, `RUNNER_NAME`, `RUNNER_LABELS` or `RUNNER_EPHEMERAL`, because GARM
bakes the name, labels, group and ephemeral flag into the credential files and `run.sh` reads them there.
Variables that the runner would ignore would only mislead.

The container never receives the instance token, the metadata URL or the callback URL. Every design that
puts a JIT blob or a Bearer token into an environment variable fails the `docker inspect` test by
definition. The `ACTIONS_RUNNER_INPUT_JITCONFIG` convention used by ARC is rejected for that reason, and
GARM does not serve a single blob anyway. A mode in which the container holds the token and calls GARM
itself is not implemented: it would expose the token to `docker inspect` and to job code.

### Non-JIT path

When a pool sets `disable_jit_config`, the provider fetches a registration token from
`runner-registration-token` instead, and delivers it the same way. The entrypoint runs
`config.sh --ephemeral --disableupdate --token ...` as the runner user through `gosu` (the base image's
`config.sh` refuses to run as root) and starts `run.sh`. It lets `config.sh` write the credential files
normally and, only after success, moves them onto the tmpfs and replaces them with symlinks. Pre-linking
before `config.sh` would leave dangling symlinks, and `Runner.Listener configure` reads `.credentials` at
start-up and crashes (`FileNotFoundException`, exit 134), as observed on a live daemon. A trap removes
credential files from the writable layer if `config.sh` fails. This path is implemented but has not been
verified against a real GARM pool.

The registration token is the one delivered file that must not stay on the tmpfs. The delivery exec runs as
the runner user, so the file is readable by job steps, which also have passwordless sudo, and a registration
token can register further runners in the same scope while it is valid. Unlike the JIT files it does not
identify a single runner. The entrypoint therefore reads the token and deletes
`/run/garm/registration-token` before it does anything else, so the file is gone before `config.sh` runs and
long before any job starts. It fails closed: a missing, empty or unreadable token, or a file that cannot be
removed, stops the runner. The failure and signal traps remove the file too, in case either arrives before the
read. The token is held in an unexported shell variable and is passed to `config.sh` with `--token`, which
puts it on that process's command line, visible to other processes in the container, but only while
`config.sh` runs. No job runs yet at that point. `config.sh` can read the token from the environment
instead, but that would not change who can see it before a job starts, so the command line is kept.
`exec` replaces the entrypoint with the runner, so neither the variable nor the arguments survive into it.

### Status reporting

GARM creates every runner as `pending`. On GARM v0.2.1 scale sets, the job-started handler moves a runner to
`active` with a checked update that allows only `idle` or `offline` as the previous state, so a runner that
never reports leaves the listener refetching the same message and the scale set stalls. Pools force the
update and are not affected. Upstream relaxed the check in cloudbase/garm#817, which was in no release when checked on
2026-10-11.

The provider therefore reports status itself, from the provider process, using the token it already holds.
It posts `installing` to `{callback-url}/status` once the credentials are delivered and the runner is
running, then `idle` once the runner's own stdout shows `Listening for Jobs`, read through the Docker logs
API, within `[status_report].timeout_seconds` (default 90, `0` disables). `idle` is never sent early,
because GARM's scale-set bootstrap-timeout reaper collects only `pending` and `installing` runners, and a runner
reported `idle` that then failed to start would never be reaped. On timeout the runner stays `installing`.
If the daemon cannot return logs (log driver `none`), readiness falls back to `Runner.Listener` appearing in
`docker top` and staying up for 5 seconds. That is weaker evidence, so the provider warns. It never changes
the container's log driver.

Reporting is best-effort and never fails `CreateInstance`. `agent_id` is omitted, since GARM rejects a value
that differs from the one on a scale-set row. Only JIT runners are reported: GARM stores a JIT runner's
agent ID when it generates the configuration, while a non-JIT runner registers inside the container and an
ID-less `idle` would leave GARM unable to remove its GitHub registration. Non-JIT runners exist only on
pools, which do not need the reports. The callback URL follows the same transport rules as the metadata URL,
and no redirect is followed.

**The wait never runs inside `CreateInstance`.** GARM v0.2.1 puts no deadline on the create call, but
cloudbase/garm#817 bounds it by `runner_bootstrap_timeout` and `exec_timeout_seconds` and kills the binary on
expiry. `CreateInstance` therefore makes no request to GARM. It re-executes the provider binary as a
detached watcher (hidden first argument, own session, stdout and stderr on `/dev/null` so GARM's pipes are
not held open) and returns after handing the request over a private stdin pipe, within 5 seconds. The request
carries the token, callback URL, container ID, deadline and proxy variables, and never appears in argv or
the environment. The watcher's environment holds only `SSL_CERT_FILE`, `SSL_CERT_DIR` and `GODEBUG`, when
set. It exits within about 25 seconds of the deadline whatever happens, and reports `idle` only after
inspecting the container and finding it running, because a stopped container's logs still replay a
`Listening for Jobs` line printed before it exited.

An orphaned watcher is adopted by PID 1. If PID 1 is GARM itself in a container started without an init, no
one reaps it and every finished watcher stays a zombie. The provider therefore detaches only when
`/proc/1/comm` names a known init (systemd, docker-init, tini, catatonit and similar). Otherwise it runs the
watcher inside `CreateInstance`, bounded by `timeout_seconds`, and warns that GARM's container should run
with an init (`docker run --init`, Compose `init: true`).

### IPv6 is disabled in the runner

The runner is created with `net.ipv6.conf.all.disable_ipv6=1`. From Docker Engine 26, a container on an
IPv4-only network keeps `::1` on `lo` and gets `::1 localhost` in `/etc/hosts`. A server that binds
`localhost` through plain `getaddrinfo` then listens on `::1` only, while a Node client resolves with
`AI_ADDRCONFIG`, drops `::1` because the container has no non-loopback IPv6 address, and dials `127.0.0.1`.
The result is `ECONNREFUSED`, which does not happen on a VM or a GitHub-hosted runner. Measured on a
user-defined bridge network with Node 22 (`server.listen(port, 'localhost')`, then
`fetch('http://localhost:port')`):

| Engine | `lo` IPv6 without the sysctl | Without the sysctl | With the sysctl |
|---|---|---|---|
| 24.0.9, 25.0.5 | disabled | passes (binds `127.0.0.1`) | passes, `::1 localhost` line kept |
| 26.1.4, 29.9.0 | enabled | `ECONNREFUSED 127.0.0.1` (binds `::1`) | passes, no `::1` lines |

Only the `all` key is set. It is the one the daemon consults when writing `/etc/hosts`, and it also covers
`lo` and `eth0`, which already exist when it is applied, so `default` (new interfaces only) adds nothing. The
sysctl is per namespace, needs no privilege and changes nothing on the host. There is
no setting for it. A job that binds or dials `::1` explicitly will not work, and the runner could not use
IPv6 beyond loopback anyway.

On a host whose kernel has no IPv6 there is no `/proc/sys/net/ipv6`, so the create succeeds and the start
fails with an error naming the sysctl. `CreateInstance` classifies that error and retries once, after
removing the rejected runner and recreating it without the sysctl:

- With `no such file or directory`, the host has no IPv6, `::1` cannot exist and nothing is lost. One
  warning is logged.
- For any other reason (permission denied, read-only `/proc/sys`, a security policy), the sysctl could not be
  applied while IPv6 may still be on, so `localhost` can resolve to `::1` again. This is still retried,
  because failing every allocation on such a host is worse, and the warning says IPv6 may remain enabled.

Cancellation and every other error are not retried. The cache volumes are revalidated before the second
start.

### Image contents and privileges

**The runner user has passwordless sudo.** Both `myoung34/github-runner` bases ship
`%sudo ALL=(ALL) NOPASSWD: ALL` and add `runner` to the `sudo` group, and the provider sets no
`no-new-privileges`. After the entrypoint drops privileges with `gosu`, `sudo -n id -u` prints `0`. A job can
therefore become root inside its own container. This is kept for parity with GitHub-hosted runners. The
container is unprivileged in `none` mode and has no host socket, but a job in a DinD mode also reaches a
root-equivalent daemon ([ADR-001](ADR-001-dind-strategy.md)). An operator who wants sudo gone builds a
derived image without the rule. A provider option to set `no-new-privileges` could be added, and it would
also block every other setuid binary. Nothing in the images depends on sudo. Setting `RUN_AS_ROOT=true` in
the container's own environment runs the runner and the job as root, and `extra_specs` cannot set it.

Package policy: the images carry the commonly needed apt-level dependencies (the shared libraries headless
Chrome needs, a minimal font set, `pipx`, `file`, `cmake`, `ninja-build`, `clang-format` and `clang-tidy`).
Languages and toolchains come from `setup-*` actions at job time. A repository with unusual needs gets a
derived image selected through a flavor. The images do not mirror GitHub-hosted runner images. `/usr/local/bin`
and `/usr/local/lib` are owned by the runner user, because CI scripts install pinned tools there and use
`sudo` only when the directory is not writable. A runner serves one job and its writable layer is discarded,
so nothing leaks between jobs. LLVM comes from the Ubuntu archive of each release and no third-party apt
repository is added. The entrypoint raises the open-file soft limit from 1024 to 65536 (clamped to the hard
limit, best effort), as hosted runners do. The package list is in
[`runner-images/README.md`](../../runner-images/README.md).

### Job-started hook

A job can damage a directory that the provider persists across jobs of one repository, and a later job can
then pass without doing its work. The case seen is a 0-byte `pnpm` binary in the pnpm store, after which every
`pnpm` command prints nothing and exits 0.

The images therefore set `ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/garm/hooks/job-started.sh`. The runner runs that
script before the first step of every job, as the `runner` user after the privilege drop, and shows its output
in the job log. It is a dispatcher that runs each `/opt/garm/hooks/job-started.d/*.sh` in name order with a
per-check timeout. A check exits 0 for fine, 10 for damage it has already printed an `::error::` annotation for,
and anything else for "the check itself broke", which only warns. `GARM_JOB_HOOKS` decides what damage does:
`fail` (the default) exits 1, which fails "Set up runner" and skips every step; `warn` continues; `off` skips the
checks. The hook files are root-owned `0755`, so a job cannot replace them. An operator-set
`ACTIONS_RUNNER_HOOK_JOB_STARTED` in the container environment replaces ours, and a derived image adds checks by
adding files.

Runner behaviour the implementation depends on, measured with runner 2.338.0: the path must end in `.sh`
(otherwise every job fails with "is not a valid path to a script"), the runner ignores the shebang and runs
`bash --noprofile --norc -e -o pipefail <file>`, `::error::` and `::warning::` become annotations but an
`::error::` alone does not fail the job (only the exit code does), and the step costs tens of milliseconds.

Why in the runner image and not in the provider: the knowledge of what a damaged pnpm store looks like belongs
with the tool, and a check for Python or Go state would otherwise become provider code. The provider keeps its
job of owning containers and volumes. A provider-side check would also write to the provider log on the host,
which the repository's developers do not read, and it could not see damage before the next job had already
passed. The hook shows the reason in the job that is affected. It runs inside the job's own container, so it is
accident detection and not a security boundary: a hostile job can already write to its repository's caches
([ADR-003](ADR-003-cache-keying-and-lifecycle.md)).

Failing is the default because a false green is worse than a red job, and it changes what a job runs, so the
`GARM_JOB_HOOKS=warn` and `off` opt-outs exist. `GARM_JOB_HOOKS` is the one `GARM_` name that `extra_specs` may
set ([ADR-005](ADR-005-config-and-extra-specs-schema.md)), by exact name and with the operator allowlist; the
other `GARM_` names stay reserved because the entrypoint reads some of them as root.

### Updates

Bumping the base digest or `GH_RUNNER_VERSION` produces a new image digest. The operator then updates the
digest in provider config.

## Rationale

A tool-rich base, a custom entrypoint and a provider-side fetch together satisfy both the toolcache
requirement ([ADR-003](ADR-003-cache-keying-and-lifecycle.md)) and the credential-invisibility requirement.
A design in which the container fetches its own configuration, or receives it through the environment,
fails the `docker inspect` test by construction. `docker exec` and not `docker cp` is a correctness
requirement, not a style choice.

## Alternatives considered

- The official `actions-runner` image: no tooling, which forces per-job installs and defeats the toolcache.
- `summerwind/actions-runner`: deprecated upstream.
- A from-scratch Ubuntu image: re-implements what the `myoung34` base provides.
- Container-side fetch with a Bearer token in the environment: exposes the token to `docker inspect`.

## Consequences

- The repository maintains two Dockerfiles and an entrypoint, and rebuilds them when `myoung34/github-runner`
  or `actions/runner` releases.
- The forge is GitHub only. The non-JIT path is not specific to GitHub in shape, but Gitea is not supported.
- Each image carries about 300 MB of LLVM libraries for `clang-tidy`.

## Open questions

- Whether a TLS failure during the metadata fetch should be reported as its own fault, to make private-CA
  mistakes easier to find.
- Whether to add a provider option for `no-new-privileges`.
