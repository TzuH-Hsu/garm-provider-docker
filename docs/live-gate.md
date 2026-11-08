# Live gate: a real job through a real GARM

This is the runbook for `scripts/live-gate.sh`. The gate points the built (or
downloaded) provider at a real GARM instance and a throwaway test repository
running a trivial echo workflow, and drives the full lifecycle: `CreateInstance`
→ JIT registration → job runs → `DeleteInstance` → runner confirmed gone from
GitHub.

Passing this gate means: the script passes, **and** `docker inspect` of the
runner container shows no host Docker socket, no GARM credentials, and no
GitHub App key, in either its environment or its mounts.

Results of past runs, and what has not been verified, are in
[live-gate-runs.md](live-gate-runs.md).

This document does not reproduce GARM's own installation or credential-setup
instructions. It links to the official docs for those and focuses on what is
specific to this provider and this gate.

## Quick start

You need a running GARM with `garm-cli` authenticated against it, a throwaway
test repository registered with GARM, and a Docker host. See
[Prerequisites](#prerequisites) for what each one has to provide.

```sh
export REPO=<garm-repo-id-or-owner/name>   # from `garm-cli repo list`
scripts/live-gate.sh                       # scale set, dind_mode=none, v0.2.5 release
scripts/live-gate.sh --dind                # same, plus the privileged-sidecar variant
scripts/live-gate.sh --pool                # webhook-driven pool instead of a scale set
scripts/live-gate.sh --pool --non-jit      # non-JIT registration path (own provider entry, pool with one idle runner)
```

Run one of these. It walks through every step and stops wherever you have to
act. On macOS, or to test uncommitted changes, add `--from-source`.
[Running the gate](#running-the-gate) explains each step.

<!-- TOC -->
- [Prerequisites](#prerequisites)
- [Running the gate](#running-the-gate)
- [Release mode vs. source mode](#release-mode-vs-source-mode)
- [Scale set (default) vs. pool](#scale-set-default-vs-pool)
- [The DinD variant](#the-dind-variant)
  - [Registry mirror check (opt-in)](#registry-mirror-check-opt-in)
- [The non-JIT variant](#the-non-jit-variant)
- [What this gate proves](#what-this-gate-proves)
- [What each verification item proves](#what-each-verification-item-proves)
- [What this gate does not cover](#what-this-gate-does-not-cover)
- [Troubleshooting](#troubleshooting)
- [Recording a result](#recording-a-result)
- [Recorded runs](live-gate-runs.md)
<!-- /TOC -->

## Prerequisites

1. **A running GARM server** you control, with `garm-cli` installed and
   authenticated against it. See the official GARM docs:
   - [Quickstart: Docker](https://github.com/cloudbase/garm/blob/main/doc/quickstart-docker.md)
     or [Quickstart: systemd](https://github.com/cloudbase/garm/blob/main/doc/quickstart-systemd.md)
     to install and start GARM itself.
   - [First Steps](https://github.com/cloudbase/garm/blob/main/doc/first-steps.md)
     for the `garm-cli init`/`garm-cli profile add` authentication flow this
     gate's preflight check (`garm-cli controller show`) depends on.
2. **GitHub credentials registered with GARM** — a GitHub App or a PAT with
   the permissions GARM needs to manage runners and webhooks. See
   [Credentials](https://github.com/cloudbase/garm/blob/main/doc/credentials.md)
   for the exact scopes/permissions and the `garm-cli github credentials add`
   command. This gate never touches this credential directly — GARM's own
   metadata service is what the provider talks to (`ADR-002`).
3. **A throwaway test repository** registered with GARM
   (see [First Steps §3](https://github.com/cloudbase/garm/blob/main/doc/first-steps.md#3-add-a-repository)).
   Never point this gate at a production repository: it creates a real,
   if trivial, workflow run and a real self-hosted-runner registration.
   - **Scale-set mode (default): no webhook.** Register the repo without
     `--install-webhook`, e.g. `garm-cli repo add --owner ... --name ...
     --credentials ... --random-webhook-secret` (`garm-cli` still requires
     one of `--webhook-secret`/`--random-webhook-secret` even when no
     webhook is installed — upstream `cmd/garm-cli/cmd/repository.go`
     `MarkFlagsOneRequired`). GARM's GitHub credentials still need
     permission to create scale sets and runners for the repo.
   - **Pool mode (`--pool`)** needs `--install-webhook` (or a manually
     installed webhook) **and** GitHub being able to reach GARM's webhook
     URL.
4. **A Docker host** with `docker` reachable and `docker buildx` available
   (bundled with Docker Desktop and with recent Docker Engine's `buildx`
   plugin) — the default **release** mode uses `docker buildx imagetools
   inspect` to resolve the runner image's digest, no local image build
   required. `linux/amd64` and `linux/arm64` hosts are both supported;
   `OS_ARCH` is auto-detected from `uname -m`; if set explicitly it must
   match the host (cross-architecture runners are not supported and the
   script exits).
   - **Release mode needs a Linux host.** This repo's release workflow
     publishes `linux/amd64`/`linux/arm64` **provider binaries only** — the
     provider binary always runs as a host process wherever GARM itself
     runs (see the root README's "The provider image is a delivery
     mechanism, not a runtime" section), so there is no macOS release
     asset for it. On macOS (including Docker Desktop), use
     `--from-source` instead — it builds a native binary for the host
     you're on with `go build`. The **containers** this provider creates
     are always `linux` regardless of host OS (Docker Desktop's own VM
     handles that on macOS); it is only the provider binary's own OS that
     release mode is picky about. (A locally built binary supplied with
     `LOCAL_PROVIDER_BIN` is not a release asset, so this restriction does
     not apply to it; it must still match the host.)
   - **The Docker host may be shared.** The gate can run on a host that also
     runs other GARM scale sets or pools, including production ones: `verify`
     and `postjob` only inspect the container, network and volumes whose
     `garm.docker/instance-name` is this gate's runner (see Step 4). Keep the
     gate scale set at `--max-runners 1` (the printed command does) so exactly
     one runner matches; with several, set `GATE_INSTANCE`. The gate still
     creates real containers and, in the `--dind` variant, a privileged
     sidecar, so a production host remains a deliberate choice.
   - **`--from-source` additionally needs a Go toolchain** (`go.mod` sets
     `go 1.25.0` and `toolchain go1.27.3`; the `go` command downloads that
     toolchain on first use if yours is older) to build the provider binary,
     and builds the runner image locally
     (`docker build -f runner-images/noble/Dockerfile runner-images`, or
     `resolute` with `--runner-image resolute`). On a host without Go, build
     the binary elsewhere and hand it over with `LOCAL_PROVIDER_BIN` and
     `LOCAL_PROVIDER_SHA256`; `--from-source` then skips `go build` and only
     builds the runner image (see [Source mode with a pre-built
     binary](#source-mode-with-a-pre-built-binary)).
5. This repository checked out locally, with `scripts/live-gate.sh`
   executable (`chmod +x scripts/live-gate.sh` if needed).
6. **Provider paths readable by GARM's service user.** GARM executes the
   provider binary and reads its config as its own service user, so both
   must live in a directory that user can read: not under a home
   directory (typically not traversable by a service user) and not under
   `/tmp` (usually cleared on reboot). The script's defaults
   (`PROVIDER_BIN_DIR` under the checkout, `PROVIDER_CONFIG_DIR` under
   `/tmp`) are kept for backward compatibility and the preflight warns,
   without failing, when either resolves under `/tmp` or `$HOME`. Example:

   ```sh
   sudo install -d -m 0755 -o "$USER" /opt/garm-gate
   export PROVIDER_BIN_DIR=/opt/garm-gate/bin PROVIDER_CONFIG_DIR=/opt/garm-gate/config
   export GARM_URL=http://127.0.0.1:<port>   # must match the API's bind address
   ```

## Running the gate

Start with the commands in [Quick start](#quick-start). Release mode first checks that the release is published (an HTTP HEAD
request for its `SHA256SUMS`) and fails early, before any download, if it
is not: "release vX.Y.Z not published yet - use --from-source to build
from this checkout".
`v0.2.0` and earlier are known to fail against GARM v0.2.x scale sets, so
passing `--version` below `v0.2.1` with a scale-set target prints a warning
(it does not block; older GARM or `--pool` may still work).

The default command downloads and verifies the `v0.2.5` release, then runs every
step below in order, pausing for a confirmed manual action wherever one is
needed (adding the `[[provider]]` section to GARM's config, creating the
pool, confirming the GitHub UI). Pass `-y`/`--yes` to auto-confirm every
pause, `--from-source` to build from this checkout instead, `--version
vX.Y.Z` to validate a different release, `--pool` for a pool, `--dind` for
the DinD variant, or run a single step directly
(`scripts/live-gate.sh verify`, etc.) — see `scripts/live-gate.sh --help` for
the full command list and every environment variable it accepts.

**Modes are remembered between steps.** `preflight` (and `all`) record the
effective `PROVIDER_SOURCE`, `PROVIDER_VERSION`, `GARM_TARGET`,
`GATE_VARIANT`, `GATE_NON_JIT`, `PROVIDER_INTERFACE_VERSION`,
`RUNNER_IMAGE_NAME` and, when set, `GATE_REGISTRY_MIRROR` /
`GATE_INSECURE_REGISTRY` and the expected `LOCAL_PROVIDER_SHA256` in
`$PROVIDER_CONFIG_DIR/gate-modes` (mode 0600, plain `KEY=value` lines that
are parsed, never sourced). A later single step run without a flag for a
mode uses the saved value and logs `using saved <mode>=<value>`, so
`scripts/live-gate.sh --dind preflight` followed by a plain `pool`, `watch`,
`verify` or `postjob` stays in the DinD variant. An explicit flag or
environment value that conflicts with the saved one is an error rather than
a silent switch: to change modes, re-run `preflight` with the new flags (it
overwrites the file) or remove `gate-modes`. The file must be a regular file
you own; a symlink or foreign owner is rejected, a loose mode is reset to
0600, and unknown keys or invalid values abort. `--help` never reads it.

The scale set name is remembered the same way
(`$PROVIDER_CONFIG_DIR/scaleset-name`). In scale-set mode, `preflight` (and
`all`) fix it up front, generating one if none is saved, and any command
given an explicit `SCALESET_NAME` validates and saves it, so a name supplied
to `preflight` is the one `pool`, `watch` and the printed workflow use. Pool
mode never creates the file.

No step ever reads, generates, or prints a secret: no GitHub PAT/App key, no
GARM API token, no runner bearer token. `garm-cli`'s own stored profile is
what authenticates against GARM; this script only shells out to it.

### Step 0 — preflight

Checks `docker` and `garm-cli` are present and reachable/authenticated,
then, depending on mode:

- **release** (default): downloads the `PROVIDER_VERSION` (default
  `v0.2.5`) binary asset for this host's architecture and its
  `SHA256SUMS`, verifies the checksum (hard failure on mismatch), resolves
  `ghcr.io/tzuh-hsu/garm-runner-noble:<version>`'s (`garm-runner-resolute` with
  `--runner-image resolute`) digest via `docker
  buildx imagetools inspect`, and generates a provider `config.toml` with
  `runner_image` pinned to that digest — digest-pin enforcement stays ON.
  With `LOCAL_PROVIDER_BIN` and `LOCAL_PROVIDER_SHA256` set, the download is
  replaced by staging that local binary after its sha256 matches (see
  [Release mode with a locally built binary](#release-mode-with-a-locally-built-binary));
  the rest is unchanged.
- **`--from-source`**: builds the provider binary (`go build -ldflags
  version stamp`) and the runner image locally (`docker build -t <tag>
  -f runner-images/<name>/Dockerfile runner-images`), and generates a
  `config.toml` with
  `allow_unpinned_runner_image = true` (required for the mutable local
  tag). With `LOCAL_PROVIDER_BIN` and `LOCAL_PROVIDER_SHA256` set, the `go
  build` is replaced by staging that pre-built binary after its sha256
  matches (see [Source mode with a pre-built
  binary](#source-mode-with-a-pre-built-binary)); the runner image is still
  built locally.

Either way, Step 0 finishes by running the obtained binary once
(`GARM_COMMAND=GetVersion`) against the generated config, so a bad
download/build or a config the current loader rejects is caught here, not
later inside GARM's own provider-subprocess log.

Preflight stages everything first: the binary, `config.toml` and
`gate-modes` are written to private `.stage.*` directories inside
`PROVIDER_BIN_DIR` and `PROVIDER_CONFIG_DIR`, the smoke test runs against the
staged files, and with `--from-source` the runner image is built under a
temporary tag. Only once the download or build, the digest resolution and the
smoke test have all succeeded does it publish: retag the runner image to
`RUNNER_IMAGE_TAG`, then rename the binary, the config and `gate-modes` into
place, in that order. A failed re-run leaves the installed files and the
runner image tag as they were; if a publish step itself fails, the steps
already done are rolled back. The one window it cannot close is a hard kill
(SIGKILL, power loss) between the renames, which can leave a mix of old and
new files; re-run `preflight` to bring them back in line.

### Step 1 — register the provider with GARM

Prints the exact `[[provider]]` section to add to GARM's own `config.toml`
(commonly `/etc/garm/config.toml`, see
[Providers](https://github.com/cloudbase/garm/blob/main/doc/providers.md)):
the provider executable's absolute path, the generated config file's
absolute path, and `interface_version = "v0.1.0"` by default
(`PROVIDER_INTERFACE_VERSION=v0.1.1` overrides it). Pauses for you to add it
and restart/reload GARM, then confirm with `garm-cli provider list`. With
`--non-jit` it prints a separate entry with `disable_jit_config = true`
instead; see [The non-JIT variant](#the-non-jit-variant).

**Why v0.1.0.** On GARM v0.2.1 and earlier, GARM's own v0.1.1
`ListInstances` wrapper inverts its error check (`if err == nil` in
`runner/providers/v0.1.1/external.go`), so every *successful* provider list
is reported as `provider binary ... returned error: %!s(<nil>)`, and GARM's
runner reconciliation for scale sets and pools keeps failing. This is an
upstream GARM bug, fixed by
[cloudbase/garm#878](https://github.com/cloudbase/garm/pull/878) ("Improve
metrics", commit `488cf98b`, merged 2026-09-11) but not in any GARM release
as of 2026-10-11 (latest: v0.2.1). The v0.1.0 wrapper does not have the bug.
Nothing is lost: GARM v0.2.1 never calls the v0.1.1-only self-description
methods (`GetSupportedInterfaceVersions`, `GetConfigJSONSchema`,
`GetExtraSpecsJSONSchema`, `ValidatePoolInfo`, ADR-005); you can still run
them by hand with `GARM_INTERFACE_VERSION=v0.1.1`.

**When to switch to v0.1.1.** Once you run a GARM release that includes
#878, set `PROVIDER_INTERFACE_VERSION=v0.1.1`. The provider supports both
versions.

Related GARM v0.2.1 pitfall, unrelated to the interface version: a pool or
scale set whose `extra_specs` is JSON `null` (rather than `{}` or unset)
makes GARM panic while it injects `runner_install_template`
([cloudbase/garm#787](https://github.com/cloudbase/garm/issues/787), fix
slated for GARM 0.2.2). This gate never sends `null`.

### Step 2 — create a scale set (or pool)

Prints the exact `garm-cli scaleset add` command (`garm-cli pool add` with
`--pool`) against your `REPO`, with the generated scale set `--name` (the
workflow's `runs-on`), `--enabled`, `--min-idle-runners 0 --max-runners 1`
(fixed by design — this gate only ever needs one runner to exist at a
time; `--min-idle-runners 1` with `--non-jit`), `--extra-specs` in the `--dind` variant, `--os-arch` set to this host's
auto-detected architecture (`amd64` or `arm64`; `OS_ARCH` may only restate it),
and placeholder `--image`/`--flavor` values (required by `garm-cli`, but
informational only for this provider: `CreateInstance` always uses the
config's — or a selected flavor's — `runner_image` and ignores the
bootstrap `image` field — `internal/provider/create.go`'s `ensureImage`).
Pauses for you to create it and confirm with `garm-cli scaleset list`
(`garm-cli pool list` in pool mode).

### Step 3 — trigger the workflow and watch

Prints a sample workflow YAML to copy into
`<your-test-repo>/.github/workflows/garm-live-gate.yml` — `runs-on` set to
the scale set's name (or the pool's tags with `--pool`), and a job that does
`echo hello && env | sort | head` (plus a short `sleep` so the ephemeral
runner stays up long enough for Step 4's checks — this runner is
single-job ephemeral by design, `ADR-002`; the `--dind` variant adds
`docker run --rm hello-world`, a `docker build` step, a shared-workspace
step and a published-port step). Then polls
`docker ps --filter label=garm.docker/managed=true --filter label=garm.docker/role=runner` and GARM's runner list
(`garm-cli scaleset runner list <id>`, the id looked up by name, or
`SCALESET_ID`) every few seconds until you've triggered the workflow and a
runner appears (Ctrl+C to move on early). The `docker ps` listing is
informational and host-wide (it shows every managed runner on the host,
including other scale sets'); `verify` and `postjob` scope to this gate's own
instance. Always select the runner by `garm.docker/role=runner`, never by
`garm.docker/managed=true` alone: the provider also starts short-lived
cache-seeding helper containers (`garm-seed-*`) that carry `managed=true`.

### Step 4(a-d) — verify

Run it once the runner is `idle` (GARM's runner list, or `Listening for Jobs`
in `docker logs`), not the moment its container appears: the entrypoint links
the credential files into the install directory a few seconds after the
container starts, and check (d) fails if it runs before that. The job's hold
(30 s, 60 s with `--dind`, 120 s with `--dind` and `GATE_REGISTRY_MIRROR`)
leaves enough time for it.

Identifies the gate's own runner and checks it. The runner is selected by its
`garm.docker/instance-name` label, the GARM instance name, and never by
recency: the instance names come from GARM's runner list for the gate's own
scale set (`garm-cli scaleset runner list <id>`, the id looked up by name or
given as `SCALESET_ID`) or, with `--pool`, for the gate's pool
(`garm-cli runner list <POOL_ID>`). With `--pool`, `POOL_ID` (from `garm-cli
pool list --repo <repo>`) or `GATE_INSTANCE` is required: other pools of the
same repo are never assumed to be the gate's. Set `GATE_INSTANCE` to the
runner's instance name to skip the GARM lookup. Exactly one runner container must match; zero matches, several
matches, or a GARM list that cannot be read is a FAIL, and `verify` never
falls back to "the newest runner". This is what lets the gate run on a Docker
host that also runs other GARM scale sets and pools (for example a production
host): their runners carry the same `managed=true`/`role=runner` labels but a
different instance name, so they are never inspected. `verify` (and `watch`,
on every poll) records the instances it sees in
`$PROVIDER_CONFIG_DIR/gate-instances` so `postjob` can check all of them;
`watch` clears the file when a new run starts.

It prints PASS/FAIL for each check: ownership labels on the container, the labeled per-job
network and named workspace volume, and that the runner is actually wired
to them (attached to that network and not to the default `bridge`; the
workspace volume mounted at `/actions-runner/_work`) (a); in the `--dind`
variant the sidecar must join the runner's network namespace (no network of
its own), a port published by the sidecar's daemon must be reachable from
the runner at `127.0.0.1` and `localhost`, and both containers must
mount the labeled socket volume at `/run`; no forbidden env var name,
bind mount or `Privileged` flag in `docker inspect`, plus the sidecar
checks above in the `--dind` variant (b) — values are never printed,
only variable names, and even the informational `Config.Env` dump redacts
anything `TOKEN`/`METADATA_URL`/`CALLBACK_URL`-shaped as a defense-in-depth
display safeguard; the credential tmpfs contents (c); and the install-dir
symlink plus a running-process check (d). It also logs, informationally
only (not PASS/FAIL), how many persistent cache volumes exist — these
persist across jobs by design (`ADR-003`). It also FAILs if any job-scoped
volume (one carrying a `garm.docker/instance-name` label) also carries
`garm.docker/cache=true`: teardown skips cache-labeled volumes, so that
combination would leak the volume. None of these ever print
credential file *contents* — only filenames, permissions, and variable
*names*.

### Step 4(e) — post-job

Run once the workflow has finished. Asks you to confirm in the GitHub UI
that the runner is gone (this script never asks for a GitHub token, so it
cannot check this itself), then polls for the runner container to
disappear (bounded by `POST_JOB_WAIT_SECONDS`, default 300s — GARM's own
reaper/polling cadence governs how quickly this happens) and checks for any leftover `garm.docker/managed=true` container,
**network**, or volume. All of this is scoped to the gate's own
`garm.docker/instance-name` values: every instance `watch` and `verify`
recorded, any runner GARM still lists for the gate, and any names in
`GATE_INSTANCE` (commas or spaces). Other scale sets' runners and resources on
a shared host are neither waited for nor reported. The `--dind` workflow runs
two jobs (`demo` and `container-job`), each on its own ephemeral runner with
its own network and volumes, so `postjob` requires two known instances in that
variant (one otherwise) and FAILs if fewer are known, because the missing
runner's teardown would go unchecked. To record both, let `watch` keep running
until the second job's runner has started (or set `GATE_INSTANCE` to both
names). With no instance known at all, `postjob` fails rather than checking
the whole host.
Only persistent cache volumes (`garm.docker/cache=true`
and no `garm.docker/instance-name`) are excluded by label and listed
informationally; a volume with an instance-name is job-scoped and counts as a
leftover even if it also carries `cache=true`. The throttle stamps
(`garm.docker/role=diag-prune-stamp` since `v0.2.5`, and `role=throttle-stamp`
for the orphan sweep and the cache GC: never-started containers kept on purpose)
are likewise excluded by label and listed; every other managed
container, and every `role=runner` one, must be gone. The per-job network and the named workspace volume
both carry `garm.docker/managed=true` (`ADR-001`/`ADR-004`), so
`DeleteInstance`'s teardown removes both explicitly; a leftover one is a
real failure this check catches. This is distinct from the persistent,
repo-scoped cache volumes (`ADR-003`), which carry `garm.docker/cache=true`
instead and are deliberately never swept by any teardown path.

### Cleanup

Prints, but never runs, the manual cleanup commands: disable and delete the
scale set (`garm-cli scaleset update <id> --enabled=false`, `garm-cli
scaleset delete <id>`; the pool equivalents with `--pool`),
confirm no managed Docker resources (containers, networks, volumes) remain,
remove the gate's local binary and config, and remove the `[[provider]]` section
from GARM's config. It also reminds you that persistent cache volumes are
never touched by cleanup and shows the manual `docker volume prune` command
to reclaim them.

## Release mode vs. source mode

`scripts/live-gate.sh` has two modes, selected by `--from-source` (or the
`PROVIDER_SOURCE` env var). Both can take a pre-built provider binary instead
of obtaining one themselves (see below):

- **`release`** (default) — validates a **published release**, exactly as
  an operator would install it:
  1. Downloads the `garm-provider-docker_<version>_linux_<arch>` binary
     asset from the GitHub Release (`PROVIDER_VERSION`, default `v0.2.5`)
     for this host's architecture, plus that release's `SHA256SUMS`.
  2. Verifies the downloaded binary against `SHA256SUMS` — **failing hard**
     (not warning) on any mismatch or missing entry — before it is ever
     executed.
  3. Resolves `ghcr.io/tzuh-hsu/garm-runner-noble:<version>`'s (or, with
     `--runner-image resolute`, `garm-runner-resolute:<version>`'s) current
     digest via `docker buildx imagetools inspect` (no pull required) and
     writes the generated config's `runner_image` as
     `ghcr.io/tzuh-hsu/garm-runner-noble@sha256:<digest>` — **digest-pin
     enforcement stays on**: `allow_unpinned_runner_image` is never set in
     this mode, matching a real operator's config
     (`examples/config.minimal.toml`).
  4. Smoke-tests the binary (`GARM_COMMAND=GetVersion`) against the
     generated config, so a bad download or a config the current loader
     rejects fails here, immediately, with a clear message.
- **`source`** (`--from-source`) — the local-dev flow:
  builds the provider binary from this checkout (`go build`, version
  stamped from `git describe`) and the runner image locally (`docker build
  -f runner-images/noble/Dockerfile runner-images`, tagged
  `garm-runner-noble:demo`), and sets
  `allow_unpinned_runner_image = true` in the generated config (required —
  a local mutable tag can never be digest-pinned). Use this when testing
  uncommitted provider or runner-image changes.

### Release mode with a locally built binary

A pre-tag gate often needs the opposite split from `source`: a provider binary
built from the commit under test, run against the **published** runner image,
on a host where building images is not acceptable. Set both of these (they are
only valid together; with `--from-source` they do something different, see
the next section):

```sh
export LOCAL_PROVIDER_BIN=/opt/garm-gate/build/garm-provider-docker   # absolute path, executable
export LOCAL_PROVIDER_SHA256=<64 lowercase hex characters>           # the sha256 you expect it to have
scripts/live-gate.sh --version vX.Y.Z preflight
```

Everything stays release mode except where the binary comes from:

- Preflight copies the file into its private staging directory and hashes the
  **copy**. A mismatch with `LOCAL_PROVIDER_SHA256` is a hard failure: an
  unverified binary is never staged or executed. Compute the expected value
  where you built the binary (`sha256sum garm-provider-docker`) so that it
  does not come from the file being checked.
- No release asset or `SHA256SUMS` is downloaded, and the release does not
  have to be published yet. `--version` (or `PROVIDER_VERSION`) only selects
  the tag of `ghcr.io/tzuh-hsu/garm-runner-<name>` whose digest is resolved
  and pinned, so that image must already be published. Digest-pin enforcement
  stays ON: `allow_unpinned_runner_image` is never set.
- The binary must be built for the host's OS and architecture (the smoke test
  runs it once against the generated config). Because it is not a release
  asset, the Linux-only restriction of release mode does not apply.
- The script says so wherever it matters: a warning when the binary is staged,
  a "LOCAL provider binary" line in the preflight summary, a comment in the
  generated `config.toml`, and `, local binary` in the `description` of the
  `[[provider]]` snippet. The expected sha256 is remembered in `gate-modes`
  (the path is not); a later `preflight` without these variables goes back to
  downloading the release.

A result from this mode proves the **commit and binary you built**, not the
published release asset; see [Recording a result](#recording-a-result).

### Source mode with a pre-built binary

`--from-source` normally runs `go build`, so the gate host needs a Go
toolchain. When it has none and installing one is not wanted, cross-compile the
binary elsewhere (for the host's OS and architecture, from the commit under
test) and give the gate the file and its sha256, the same pair as above:

```sh
# on the build machine
GOOS=linux GOARCH=amd64 go build -o garm-provider-docker .
sha256sum garm-provider-docker

# on the gate host (no Go needed)
export LOCAL_PROVIDER_BIN=/opt/garm-gate/build/garm-provider-docker
export LOCAL_PROVIDER_SHA256=<the sha256 printed on the build machine>
scripts/live-gate.sh --from-source preflight
```

- The binary goes through the same verification as in release mode: preflight
  copies it into its private staging directory, hashes the **copy**, and dies on
  a mismatch, so an unverified binary is never staged or run. Compute the
  expected value where you built the binary, not from the file being checked.
- `go build` is skipped and `go` is not required. Nothing else changes in
  source mode: the runner image is still built locally from
  `runner-images/<name>` (`docker build`), and the generated config still sets
  `allow_unpinned_runner_image = true`. The runner image therefore comes from the
  checkout the script runs in, so keep that checkout at the commit the binary was
  built from.
- The warning, preflight summary line, `config.toml` comment, `, local binary`
  in the `[[provider]]` description and the remembered `LOCAL_PROVIDER_SHA256` in
  `gate-modes` work as in release mode.
- Without `LOCAL_PROVIDER_BIN` and `LOCAL_PROVIDER_SHA256`, source mode behaves
  exactly as before. Setting only one of the two is an error.

As in release mode, a result from this run proves the commit and binary you
built; record both (see [Recording a result](#recording-a-result)).

Either mode gates the Ubuntu 24.04 runner image by default. `--runner-image
resolute` (or `RUNNER_IMAGE_NAME=resolute`) gates the Ubuntu 26.04 image
instead: `runner-images/resolute` in source mode, `garm-runner-resolute` in
release mode (published from v0.2.3 on). Like the other modes it is remembered
by preflight for the later steps.

Every combination ends with a generated provider `config.toml` (path printed
by the script) and a verified, runnable provider binary; everything after
Step 0 (registration, pool, workflow, verification) is identical regardless
of mode.

## Scale set (default) vs. pool

| | Scale set (default) | Pool (`--pool` / `GARM_TARGET=pool`) |
|---|---|---|
| Job delivery | GARM long-polls GitHub's message queue | GitHub webhook → GARM |
| Inbound connectivity needed | None (outbound HTTPS only) | GitHub must reach GARM's webhook URL |
| Workflow `runs-on` | the scale set **name** (e.g. `garm-docker-gate-3fa9c1`) | the pool **tags** (e.g. `[docker, garm-demo]`) |
| Create / list / runners / delete | `garm-cli scaleset add` / `list` / `runner list <id>` / `delete <id>` | `garm-cli pool add` / `list` / `runner list` / `delete <id>` |

Use the default when GARM runs behind a NAT or VM switch (this gate's
intended setup): a webhook-driven pool can never receive a job there. The
script generates a unique scale set name (`garm-docker-gate-<6 hex>`),
remembers it in the config directory so every step agrees, and prints the
workflow with that exact `runs-on`. Scale set names and labels **cannot be
changed after creation** (upstream `doc/scale-sets.md`); to change one,
delete and re-create the scale set. Override with `SCALESET_NAME`.

The `garm-cli scaleset add` flags the script prints were checked against
upstream GARM v0.2.1 `cmd/garm-cli/cmd/scalesets.go` (`--repo`,
`--provider-name`, `--name`, `--image`, `--flavor`, `--min-idle-runners`,
`--max-runners`, `--os-arch`, `--os-type`, `--enabled`, `--extra-specs`;
`--provider-name`, `--name`, `--image` and `--flavor` are required).
Scale-set JIT delivery is forced on by GARM, and `GARM_POOL_EXTRASPECS` is
empty for scale sets — this provider reads `extra_specs` from the stdin
bootstrap payload, so it works unchanged.

## The DinD variant

`--dind` (or `GATE_VARIANT=dind`) is an **optional** second variant; the
default stays `dind_mode = "none"`. It generates, in addition to the
default config:

```toml
allowed_dind_modes = ["none", "privileged-sidecar"]
dind_image = "docker:dind@sha256:<digest resolved at preflight>"

[flavors.dind]
  runner_memory = "6GiB"
  dind_memory = "4GiB"
```

and attaches `--extra-specs '{"flavor":"dind","dind_mode":"privileged-sidecar"}'`
to the scale set (or pool). Preflight validates exactly that JSON against
the generated config with the provider's own `ValidatePoolInfo` before
anything touches Docker. The test workflow runs `docker run --rm
hello-world` and a `docker build` of a trivial Dockerfile inside the job,
which proves a non-root job can use the sidecar's daemon, and a step that
writes a file into `$GITHUB_WORKSPACE` and reads it back through
`docker run --rm -v "$GITHUB_WORKSPACE":/w alpine:3.20 cat /w/gate-shared.txt`
(must print `ok`), which proves the sidecar daemon sees the runner's
workspace (the sidecar mounts the same workspace volume at
`/actions-runner/_work`). The same step then has a nested container write
`gate-nested.txt` into the workspace and reads it back from the runner, which
proves the sidecar's workspace mount is writable end to end. A last step
starts a nested `busybox` httpd with an ephemeral published port (`-p 80`)
and fetches it with `curl` at `127.0.0.1` and `localhost` (must print
`published-ok`), which proves the sidecar shares the runner's network
namespace (ADR-001, 2026-10-12 amendment), the way testcontainers and
`docker compose` ports are used. The workflow also has a second job,
`container-job`, which `needs` the first one (so the gate's single runner is
free, and `verify` keeps its window on the first job). It runs in
`container: debian:trixie-slim`, starts with `actions/checkout` and then reads
the checked-out workflow file back with a `run:` step. This is the acceptance
test for container jobs (ADR-001, 2026-10-29 amendment): the runner binds
`/actions-runner/externals` into the job container as `/__e`, the sidecar's
daemon resolves that source in its own filesystem, and `actions/checkout`, a
JavaScript action, only starts if the sidecar holds the runner's externals. It
needs the externals cache, which is on by default. Step 4 then also
verifies the sidecar topology (`role=dind` labels, `Privileged=true`, no
bind mounts on either container, `DOCKER_HOST`, labeled socket and
`dind-state` volumes, the sidecar's mount set (socket, `dind-state`, workspace
and, with the cache on, the runner's externals volume read-only), and the
flavor's memory limits).

**Caveat:** `privileged-sidecar` runs a `Privileged=true` daemon on the
host; a malicious job could pivot to the host (ADR-001 "Residual risk").
Use a disposable VM. The 6GiB + 4GiB flavor sizes are limits, not
reservations, but size the VM accordingly.

### Registry mirror check (opt-in)

With `--dind`, setting `GATE_REGISTRY_MIRROR` adds a check of the sidecar's
registry mirror (`registry_mirrors`, see [`dind-image-pulls.md`](dind-image-pulls.md)).
Unset, which is the default, none of it runs and the gate is unchanged.

```sh
# The mirror is a Docker Hub pull-through cache your Docker host and the job
# networks can reach (a distribution `registry` in proxy mode, per
# dind-image-pulls.md). The log container is optional but recommended.
# Use an address that is reachable from INSIDE the sidecar: the host's LAN IP
# or a DNS name, never one in 172.17.0.0/16 (see the note below).
export GATE_REGISTRY_MIRROR=http://mirror.lan.example:5000
export GATE_MIRROR_LOG_CONTAINER=registry-mirror   # the mirror's container name on this host
scripts/live-gate.sh --dind preflight
# ... pool, then once the job is running and the runner is idle:
scripts/live-gate.sh verify
```

What it changes and checks:

- **Preflight** writes `registry_mirrors = ["<mirror>"]` into the generated
  config (and `insecure_registries = ["<GATE_INSECURE_REGISTRY>"]` when that is
  set), and refuses to continue if the provider build does not list
  `registry_mirrors` in its config schema: a build without the key would load
  the config, ignore the key, and run the sidecar with no mirror. A release that
  predates the setting therefore fails here; use a release that includes it or
  `--from-source`. The mirror and insecure registry are remembered with the
  other modes (`gate-modes`), so later single steps need not repeat them.
- **The workflow** gains a step, right after the first one, that fails the job
  unless `docker info` in the job lists the mirror under Registry Mirrors
  (dockerd prints it with a trailing `/`), and its final hold lasts 120 s
  instead of 60 s to leave `verify` room for its extra pull.
- **`verify`** adds, in the DinD part of check (b):
  1. the sidecar's `dockerd` argv has `--registry-mirror=<mirror>`, and an
     `--insecure-registry` flag only when `GATE_INSECURE_REGISTRY` is set. By
     default the mirror is therefore exercised **without** an insecure entry,
     which checks the claim that an `http://` mirror needs none (confirmed on
     Engine 29.9.2). If that fails on your Engine, set
     `GATE_INSECURE_REGISTRY` to the mirror's `host:port` and report it;
  2. `docker info` through the runner's `DOCKER_HOST` (the sidecar's daemon)
     lists the mirror, and the insecure registry when one is set;
  3. a pull of `GATE_MIRROR_PULL_IMAGE` (default `nginx:alpine-slim`, a small
     Docker Hub official image the sample workflow never pulls) succeeds and is
     observed at the mirror. The image is removed first so the pull is real.
     With `GATE_MIRROR_LOG_CONTAINER` set, the mirror's access log
     (`docker logs --since <just before the pull>`) must show requests under
     `/v2/library/<image>/`; this is the authoritative check, and its absence
     fails the gate. Without it, the mirror's `/v2/_catalog` (read from inside
     the runner, the same network path dockerd uses) must list the repository
     after the pull. That needs a distribution-based mirror, and if the
     repository was already cached it only proves the mirror holds it; `verify`
     says so, and if the catalog cannot be read it asks for the log container
     instead.

Run `verify` while the job holds (the same window as the other `verify`
checks). The mirror must be reachable from the job network; with
`[network].internal = true` it cannot be (that network has no route out at
all). The check pulls one small image through the mirror and
does nothing else to it.

**Choosing the mirror address.** The sidecar shares the runner container's
network namespace, and its nested `dockerd` creates its own `docker0` bridge
there with the default range `172.17.0.0/16`. A mirror written as
`http://172.17.0.1:5000` (the usual address of the *host's* `docker0`) therefore
resolves to the nested bridge inside the sidecar, the dial gets `connection
refused`, and `dockerd` silently falls back to Docker Hub. Nothing fails: the
pull still succeeds, just not through the mirror. Use an address that is
reachable from inside the sidecar, such as the host's LAN IP or a DNS name, and
keep it out of `172.17.0.0/16`. The `docker info` step still passes with a
wrong address (it only lists the configured mirror), so the pull check is the
one that catches it: with `GATE_MIRROR_LOG_CONTAINER` set, `verify` fails
because the mirror's access log shows no request for the pulled image. If the
check fails, confirm the pull succeeded anyway and look at the mirror's access
log before suspecting the provider. Overriding the nested bridge range (for
example `--bip` or `default-address-pools` through a custom `dind_image`) is
outside this gate.

## The non-JIT variant

`--non-jit` (or `GATE_NON_JIT=1`) is an **optional** variant that gates the
non-JIT registration path. It needs `--pool` and cannot be combined with
`--dind`.

```sh
export REPO=<garm-repo-id-or-owner/name>
scripts/live-gate.sh --pool --non-jit
```

**Why a pool.** Without JIT the provider delivers a registration token to
`/run/garm/registration-token`; the runner image's entrypoint reads it, deletes
the file at once, and only then runs `config.sh`, so a job step never sees the
token (a registration token can register further runners in the same scope while
it is valid). Scale sets never reach this path: GARM forces JIT for them whatever
`disable_jit_config` says. Only a pool on a provider entry with
`disable_jit_config = true` takes it, so the default scale-set gate leaves the
token deletion untested.

**Why `--min-idle-runners 1`.** The gate host is meant to sit behind a NAT and
receives no GitHub webhooks, so a pool never creates a runner on demand. GARM's
own loop (`ensureMinIdleRunners`) does not need a webhook: with
`--min-idle-runners 1` it creates one idle runner as soon as the pool is
enabled, and that runner pulls the queued job from GitHub itself. The pool is
still capped at `--max-runners 1`, and carries the dedicated tag
`garm-gate-nonjit` (override with `POOL_TAGS`) so no other job lands on it.

**Why a separate provider entry.** `disable_jit_config` belongs to GARM's
`[[provider]]` entry, not to the pool. Step 1 therefore prints a **second** entry,
`docker_gate_nonjit` by default (override with `PROVIDER_NAME`), with
`disable_jit_config = true`. It points at the same provider executable and config
file as the normal gate entry, which stays as it is and keeps serving scale sets.
GARM reads its `[[provider]]` sections at startup, so adding the entry **requires
a GARM restart**. The entry is kept after the run; the next non-JIT run reuses it.
Make sure the pool uses this entry (Step 2 prints `--provider-name` for it): on
the normal entry the runner would take the JIT path and `verify` would fail.

What changes compared with the default gate:

- **Step 1** prints the `docker_gate_nonjit` block described above, with the note
  that it is an additional entry and needs a restart.
- **Step 2** prints `garm-cli pool add ... --min-idle-runners 1 --max-runners 1
  --tags garm-gate-nonjit` against the non-JIT provider entry. The runner appears
  shortly after the pool is enabled and waits for the job. It is ephemeral, so
  after the job GARM creates a replacement idle runner until the pool is scaled to
  zero (see below). Note the pool ID: `verify`, `watch` and the teardown need it
  (`POOL_ID`).
- **Step 3** prints the sample workflow as `garm-live-gate-nonjit.yml`, with
  `runs-on: ["garm-gate-nonjit"]` and an extra step that fails if the token is
  visible to the job (`test ! -e /run/garm/registration-token`).
- **Step 4** expects JIT to be disabled: the runner's `JIT_CONFIG_ENABLED` must be
  `false`, otherwise the pool is not on the non-JIT entry and the run FAILs. The
  token check of check (c) is mandatory here: after the registration wait
  (up to 60 seconds), `/run/garm/registration-token` still being present is a
  FAIL, whether or not the runner managed to register (in the default gate the
  check is skipped for a runner that has not registered). The other checks are
  the same as for JIT runners: the entrypoint moves the credentials onto the
  tmpfs after `config.sh` has succeeded, so the same files and symlinks are
  checked.
- **`watch` and `postjob`** record only the first runner they see as the gate's
  runner, because of the replacement idle runner. The replacement is not part of
  the checks. `watch` clears the recorded instances when it starts. `verify`
  inspects the recorded runner (or `GATE_INSTANCE`) and never swaps in a
  runner the pool lists now: if the recorded runner's container is already gone,
  it FAILs with "verification window missed", and you re-run `watch` and
  `verify` with a fresh job. Without a recorded runner, `verify` warns and takes
  the one GARM lists, which after a finished job is the replacement idle runner,
  so start `watch` before triggering the job and run `verify` while the job
  holds.

**Teardown.** Because GARM keeps replacing the idle runner, scale the pool down
before deleting it. The Cleanup step prints:

1. `garm-cli pool update <POOL_ID> --min-idle-runners 0 --enabled=false`
2. wait until `garm-cli runner list <POOL_ID>` lists no runners
3. `garm-cli pool delete <POOL_ID>`
4. check the test repository's runner list on GitHub (Settings > Actions >
   Runners, or `gh api repos/<owner>/<name>/actions/runners`) for leftovers and
   remove any that remain

and tells you to keep the `docker_gate_nonjit` provider entry, together with the
binary and config directory it points at.

**Known limitation: pending status and slow deletion.** GARM's runner status
reporting is JIT-only. For a non-JIT runner GARM can therefore keep reporting it
as pending although its container has registered and is idle, it can take long
to notice that the ephemeral runner finished and delete it, and a runner can
linger in GitHub's list. The gate treats this as a known limitation, not a
failure: `verify` inspects the container rather than GARM's status, and `postjob`
logs, instead of failing, that GARM has not deleted the runner container within
`POST_JOB_WAIT_SECONDS` (and skips the leftover-resource check for that run;
re-run `postjob` after GARM has deleted it or after the pool teardown). The
teardown above removes what is left.

## What this gate proves

| # | Verification item | Maps to |
|---|---|---|
| (a) | The runner container, the per-job network, and the named workspace volume all carry the expected `garm.docker/*` ownership labels | `ADR-004`'s label schema (`ADR-001`'s per-job topology) |
| (b) | `docker inspect` shows no `BEARER_TOKEN`/`METADATA_URL`/`CALLBACK_URL`-like env var, no bind mount (hence no host `docker.sock`) and no `Privileged` flag on the runner; in the `none` variant no DinD sidecar exists, in the `--dind` variant the `role=dind` sidecar has the expected labels, `Privileged=true`, no bind mounts, and the flavor's memory limits | The headline safety property above; `ADR-001` |
| (c) | The credential files (plus the `.delivered` marker) actually land in `/run/garm` inside the running container. Without JIT, once the runner is up, `/run/garm/registration-token` must be absent | `ADR-002`'s **exec-tar-into-tmpfs** verification item — proves `docker exec`-streamed delivery works against a real Docker daemon, not just the fake client used in unit tests |
| (d) | `/actions-runner/.runner` is a working symlink into `/run/garm`, and the runner process is actually running | `ADR-002`'s **actions/runner symlink-compat** verification item — proves the real `.NET` runner accepts a symlinked config file instead of falling back to a writable-layer copy |
| (e) | After the job completes: the runner container (and DinD sidecar, socket and `dind-state` volumes in the `--dind` variant), its per-job network, and its named workspace volume are all gone; the runner is gone from GitHub; and no other `garm.docker/managed=true` resource is left over | The end of the runner lifecycle, for the variant that was run |

## What each verification item proves

| Check | Mechanism | Why it can't be fully verified by unit tests alone |
|---|---|---|
| (c) exec-tar-into-tmpfs delivery | `docker exec <runner> test -e /run/garm/<file>` for each credential file plus `.delivered`; without JIT, after waiting up to 60 seconds for the runner to register, the same test must fail for `registration-token` | `internal/docker/fake.go`'s fake client cannot reproduce moby's real tmpfs/mount-namespace semantics that made the original `docker cp` design unbuildable (`ADR-002`'s amendment) — only a real daemon proves the `docker exec`-streamed tar actually lands in a real memory-backed tmpfs |
| (d) actions/runner symlink-compat | `docker exec <runner> ls -la /actions-runner/.runner` shows `-> /run/garm/runner`, plus `docker top` shows the runner process running | Whether the real, unmodified `.NET` `actions/runner` binary accepts a symlinked `.runner`/`.credentials`/`.credentials_rsaparams` instead of requiring a real file is an external-binary behavior this repo's own tests cannot exercise (`ADR-002`'s open questions) |

## What this gate does not cover

This gate runs with `dind_mode = "none"` by default — the smallest, most
portable slice, needing no Sysbox or privileged-container support on the
Docker host — and optionally with `privileged-sidecar` (`--dind`, above).
It does **not** exercise:

- `sysbox-runc` mode (needs Sysbox installed on the host), and
  `privileged-sidecar` unless `--dind` is passed. DinD is a deliberate
  operator opt-in (`allowed_dind_modes` defaults to `["none"]`,
  fail-closed) with residual-risk tradeoffs documented in `ADR-001`, so the
  default run does not enable it.
- The persistent toolcache/pnpm/externals caches (`ADR-003`) beyond
  confirming their volumes, if present, are correctly excluded from
  teardown (Step 4(e)) — this gate does not run a second job to confirm a
  cache is actually *reused* across jobs.
- `extra_specs` beyond the single `--dind` payload above (the default run
  carries none) — schema validation and the operator-ceiling/allowlist
  behavior are covered by this repo's own unit and integration tests
  (`internal/extraspecs`, `internal/verify`), not by this gate.

## Troubleshooting

| Symptom | Likely cause | What to check |
|---|---|---|
| Step 0 fails with "no published binary for this host OS" | Running `release` mode (the default) on a non-Linux host | Only `linux/amd64`/`linux/arm64` provider binaries are published. Re-run with `--from-source` (builds a native binary for this host), or run this gate from a Linux host/VM. |
| Step 0 fails with a checksum mismatch | A corrupted or tampered download, a stale local copy at `PROVIDER_BIN_DIR`, or a `PROVIDER_VERSION` whose `SHA256SUMS` doesn't list this asset | Re-run Step 0 (it re-downloads); check `PROVIDER_VERSION`/`OS_ARCH` are correct; check https://github.com/TzuH-Hsu/garm-provider-docker/releases for the release's actual asset names. |
| Step 0 fails to resolve the runner image digest | `docker buildx` missing, or this host cannot reach `ghcr.io` | Confirm `docker buildx version` works; check outbound network/proxy/firewall rules for `ghcr.io`. |
| No job is ever picked up in scale-set mode | The workflow's `runs-on` does not exactly match the scale set name, the scale set is disabled, or GARM cannot reach GitHub (outbound HTTPS) | `garm-cli scaleset show <id>` (enabled? name?); compare `runs-on` with the name in the script's output; check GARM's logs for message-queue errors. |
| No job is ever picked up in pool mode (`--pool`) | GitHub cannot deliver webhooks to GARM (e.g. GARM behind a NAT) | Use the default scale-set mode, or expose the webhook endpoint. |
| `--non-jit` Step 4(c) reports `JIT_CONFIG_ENABLED is 'true'` | The pool was created on the normal provider entry, or `docker_gate_nonjit` was added without `disable_jit_config = true` | `garm-cli pool show <POOL_ID>` must name the provider printed in Step 1; confirm the entry in GARM's `config.toml` and that GARM was restarted after adding it. |
| `--non-jit` Step 4(c) reports `/run/garm/registration-token still present` | The runner image predates the entrypoint change that deletes the token after reading it, or the entrypoint stopped before reading it | Use a runner image built from the current checkout (`--from-source`) or a release that includes the change; read `docker logs <runner>`. |
| `--non-jit` Step 2: no runner ever appears | The pool is disabled, the `min-idle-runners 1` setting was not applied, or GARM was not restarted after adding the provider entry | `garm-cli pool show <POOL_ID>` (enabled? min idle?), `garm-cli provider list`, and GARM's log for provider errors. |
| `--non-jit` runner stays pending in GARM, or `postjob` logs the "known non-JIT limitation" line | GARM's status reporting is JIT-only | Not a failure. Run the teardown from Cleanup and check GitHub's runner list for leftovers. |
| `--dind` Step 4(b) reports the sidecar missing or the runner cannot reach its daemon | `allowed_dind_modes`/`extra_specs` not applied, the sidecar failed to start, or `docker:dind` could not be pulled | `docker ps -a --filter label=garm.docker/role=dind`; `docker logs <sidecar>`; confirm the scale set was created with the printed `--extra-specs`. |
| `--dind` Step 4(b) reports the sidecar not in the runner's network namespace, or a published port not reachable at `127.0.0.1` | A provider older than the 2026-10-12 shared network namespace change (ADR-001), or a runner image without `curl` | Check the provider version; `docker inspect -f '{{.HostConfig.NetworkMode}}' <sidecar>` must print `container:<runner id>`. |
| Runner container never appears after `garm-cli scaleset add` / `pool add` | The provider failed silently from GARM's point of view, or the pool/provider isn't wired up correctly | Check GARM's own log output (`docker logs garm` if run via Docker, `journalctl -u garm` if run via systemd) for lines mentioning this provider's executable or name — GARM logs a provider subprocess's stderr on failure. Confirm `garm-cli provider list` shows the provider and `garm-cli pool show <POOL_ID>` shows it `enabled`. This provider logs structured key=value lines (`internal/logging`); look for `level=ERROR` entries. |
| Step 4(a)'s job-network or workspace-volume check fails | A provider build older than the per-job network and named volume, or a config/topology bug | Confirm you are validating the intended `PROVIDER_VERSION`/build; check `docker network ls`/`docker volume ls --filter label=garm.docker/managed=true` directly for what actually exists. |
| Step 4(c) times out or reports missing credential files | The provider's fetch-first `docker exec`-streamed tar delivery didn't complete before the entrypoint's `GARM_CRED_WAIT_SECONDS` (120s default) expired, or the exec itself failed | Check the provider's own stderr (surfaced via GARM's logs, see above) for a `failed to deliver credentials` error; check `docker logs <runner>` for the entrypoint's `waiting up to ...s for the delivery marker` message and whether it timed out. A slow/overloaded Docker daemon or a metadata-service fetch that ran close to the 60s aggregate deadline are the two likely causes (`ADR-002`). |
| Step 4(d) reports `/actions-runner/.runner` is NOT a symlink with `ls: cannot access`, but the runner then starts and the job runs | `verify` ran while the runner was still `installing`, before the entrypoint had linked the credential files | Re-run the whole gate and start `verify` once the runner is `idle`; the link is in place a few seconds after the container starts. |
| Step 4(d) reports `/actions-runner/.runner` is NOT a symlink, or the runner process never starts | `actions/runner`'s `run.sh` (or the JIT/non-JIT path in `myoung34/github-runner`) rejected the symlinked config file, or replaced it with a real file | This is exactly the open question `ADR-002` flags as a verification item. If confirmed, the documented fallback is a container-scoped writable-layer copy destroyed at teardown (never host-backed shared storage) — see `ADR-002`'s "symlink compatibility" note for the fallback design; this repo's entrypoint (`runner-images/common/entrypoint.sh`) would need updating to implement it before this gate could be considered passing. |
| Step 4(e) times out waiting for the container to disappear | GARM hasn't yet noticed (via its own GitHub-runner-list polling) that the ephemeral runner finished, or `DeleteInstance` itself is failing | Runner callbacks are not implemented in this provider (deferred, `ADR-002`) — there is no config field or code path for them — so GARM always learns a job finished by polling, and there is no fixed upper bound faster than GARM's own reap cycle. If it never resolves, check GARM's logs for a `DeleteInstance` error against this provider, and try `garm-cli runner list --repo <REPO>` to see GARM's own view of runner state. |
| Step 4(e) reports a leftover managed network or volume (persistent cache volumes — `garm.docker/cache=true` with no instance-name — are excluded) | The named job network/workspace volume failed to tear down — a real bug | Check `docker network ls`/`docker volume ls --filter label=garm.docker/managed=true` for what's left, and the provider's stderr (via GARM's logs) for a teardown error (`internal/topology/teardown.go`). |
| `garm-cli pool add` rejects `--image`/`--flavor` as invalid | Confusing these with a real cloud image/flavor | For this provider these two flags are placeholders only — any non-empty value satisfies `garm-cli`; see Step 2 above. |

## Recording a result

Once this gate has actually been run against a real GARM instance, add a
section at the top of [live-gate-runs.md](live-gate-runs.md), and a row to its
table, so the result can be cited later without re-running it. Record:

- **Versions**: the GARM version (`garm-cli controller show` prints the
  controller's GARM version), this provider's `PROVIDER_VERSION` (or the
  `git describe` version if `--from-source`), and the resolved
  `ghcr.io/tzuh-hsu/garm-runner-noble` or `garm-runner-resolute` digest Step 0
  printed.
- **Host**: OS and architecture (`uname -sm`) and `docker version`'s
  `Server.Version`.
- **The provider binary, when `LOCAL_PROVIDER_BIN` was used** (in either mode): the git
  commit it was built from (and whether the tree was clean) and its sha256,
  as given in `LOCAL_PROVIDER_SHA256` and echoed by the preflight summary. A
  result from a local binary says nothing about the published release asset
  until the release's own `SHA256SUMS` entry is shown to equal that sha256
  (or the release is gated again in plain release mode), so state which it is.
- **Per-item PASS/FAIL**: the (a)–(e) results from Step 4's output (paste
  the script's own PASS/FAIL lines rather than summarizing from memory).
- **Mode**: `release`, `release` with a local binary, `--from-source`, or
  `--from-source` with a pre-built binary; dispatch target (scale set or
  `--pool`); variant (`none`, `--dind` or `--non-jit`); and whether
  `-y`/`--yes` was used.
- **Date** the run happened, since GARM, this provider, and the runner
  image can all move independently between runs.
