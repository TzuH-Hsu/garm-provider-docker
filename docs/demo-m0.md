# End-to-end runbook: real GARM job

This is the runbook for `scripts/demo-m0.sh`: point the built (or
downloaded) provider at a real GARM instance and a throwaway test
repository running a trivial echo workflow, and drive the full lifecycle —
`CreateInstance` → JIT registration → job runs → `DeleteInstance` → runner
confirmed gone from GitHub. By default jobs are dispatched through a GARM
**scale set** (GARM long-polls GitHub's message queue — no inbound webhook,
so the GARM host can sit behind a NAT); a webhook-driven **pool** is
available with `--pool`. An optional **DinD variant** (`--dind`) exercises
`privileged-sidecar` as well.

Passing this gate means: the script passes, **and** `docker inspect` of the
runner container shows no host Docker socket, no GARM credentials, and no
GitHub App key, in either its environment or its mounts (`docs/plan.md`
§4's headline acceptance criterion). This started as the M0 go/no-go gate
(`docs/plan.md` §3 M0: pool mode, `dind_mode=none`, `linux/amd64` only);
M1–M4 have since shipped the full per-job topology (job network, named
workspace volume, DinD modes, persistent caches, `extra_specs`, v0.1.1
self-description). `v0.2.0` is the recorded failing baseline: it does not
work against GARM v0.2.x (see "Results"); the published `v0.2.4` release
passes the current gate on the Ubuntu 26.04 runner image, for `none` and
`--dind` (28 and 53 PASS, script at 0cf2a76). `v0.2.3` passed the revision
before the shared network namespace and published-port checks, for both runner
images (28 and 50 PASS, script at 92d6c85); `v0.2.2` the one before
`--runner-image` existed (28 and 50 PASS, script at efe5014), and `v0.2.1` an
earlier one (26 and 48 PASS, script at fa4351c); `v0.2.1` fails the IPv6 checks
added for `v0.2.2`. The Ubuntu 24.04 image has not been gated with `v0.2.4`
yet. The runbook keeps `dind_mode=none` as its own
default (the smallest, most portable slice) and treats the DinD modes as
out of scope for this particular gate (see "What this demo does not cover"
below), but everything else it exercises is the CURRENT provider, not the
original M0 slice. The file keeps its original `demo-m0` name to avoid
breaking existing links to it (this repo's own `docs/upstream/` submission
kit references it by path); only the framing has changed.

This document does not reproduce GARM's own installation or credential-setup
instructions — it links to the official docs for those and focuses on what
is specific to this provider and this demo.

<!-- TOC -->
- [Prerequisites](#prerequisites)
- [Release mode vs. source mode](#release-mode-vs-source-mode)
- [Scale set (default) vs. pool](#scale-set-default-vs-pool)
- [The DinD variant](#the-dind-variant)
  - [Registry mirror check (opt-in)](#registry-mirror-check-opt-in)
- [What this demo proves](#what-this-demo-proves)
- [What this demo does not cover](#what-this-demo-does-not-cover)
- [Running the demo](#running-the-demo)
- [What each verification item proves](#what-each-verification-item-proves)
- [Troubleshooting](#troubleshooting)
- [Recording a result](#recording-a-result)
- [Results](#results)
- [Known upstream issue](#known-upstream-issue)
- [Still unverified](#still-unverified)
<!-- /TOC -->

## Prerequisites

1. **A running GARM server** you control, with `garm-cli` installed and
   authenticated against it. See the official GARM docs:
   - [Quickstart: Docker](https://github.com/cloudbase/garm/blob/main/doc/quickstart-docker.md)
     or [Quickstart: systemd](https://github.com/cloudbase/garm/blob/main/doc/quickstart-systemd.md)
     to install and start GARM itself.
   - [First Steps](https://github.com/cloudbase/garm/blob/main/doc/first-steps.md)
     for the `garm-cli init`/`garm-cli profile add` authentication flow this
     demo's preflight check (`garm-cli controller show`) depends on.
2. **GitHub credentials registered with GARM** — a GitHub App or a PAT with
   the permissions GARM needs to manage runners and webhooks. See
   [Credentials](https://github.com/cloudbase/garm/blob/main/doc/credentials.md)
   for the exact scopes/permissions and the `garm-cli github credentials add`
   command. This demo never touches this credential directly — GARM's own
   metadata service is what the provider talks to (`ADR-002`).
3. **A throwaway test repository** registered with GARM
   (see [First Steps §3](https://github.com/cloudbase/garm/blob/main/doc/first-steps.md#3-add-a-repository)).
   Never point this demo at a production repository: it creates a real,
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
   - **`--from-source` additionally needs a Go toolchain** (`go.mod` pins
     `go 1.25.0`) to build the provider binary, and builds the runner image
     locally (`docker build -f runner-images/noble/Dockerfile runner-images`,
     or `resolute` with `--runner-image resolute`).
5. This repository checked out locally, with `scripts/demo-m0.sh`
   executable (`chmod +x scripts/demo-m0.sh` if needed).
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

## Release mode vs. source mode

`scripts/demo-m0.sh` has two modes, selected by `--from-source` (or the
`PROVIDER_SOURCE` env var), plus a variant of release mode for a locally
built binary (see below):

- **`release`** (default) — validates a **published release**, exactly as
  an operator would install it:
  1. Downloads the `garm-provider-docker_<version>_linux_<arch>` binary
     asset from the GitHub Release (`PROVIDER_VERSION`, default `v0.2.4`)
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
- **`source`** (`--from-source`) — the original, M0-era local-dev flow:
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
only valid together, and not with `--from-source`):

```sh
export LOCAL_PROVIDER_BIN=/opt/garm-gate/build/garm-provider-docker   # absolute path, executable
export LOCAL_PROVIDER_SHA256=<64 lowercase hex characters>           # the sha256 you expect it to have
scripts/demo-m0.sh --version vX.Y.Z preflight
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

Either mode gates the Ubuntu 24.04 runner image by default. `--runner-image
resolute` (or `RUNNER_IMAGE_NAME=resolute`) gates the Ubuntu 26.04 image
instead: `runner-images/resolute` in source mode, `garm-runner-resolute` in
release mode (published from v0.2.3 on). Like the other modes it is remembered
by preflight for the later steps.

Every combination ends with a generated demo provider `config.toml` (path printed
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
bootstrap payload, so it works unchanged (`docs/research.md` §1.D).

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
`docker compose` ports are used. Step 4 then also
verifies the sidecar topology (`role=dind` labels, `Privileged=true`, no
bind mounts on either container, `DOCKER_HOST`, labeled socket and
`dind-state` volumes, and the flavor's memory limits).

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
scripts/demo-m0.sh --dind preflight
# ... pool, then once the job is running and the runner is idle:
scripts/demo-m0.sh verify
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

## What this demo proves

| # | Demo verification item | Maps to |
|---|---|---|
| (a) | The runner container, the per-job network, and the named workspace volume all carry the expected `garm.docker/*` ownership labels | `ADR-004`'s label schema (`ADR-001`'s per-job topology) |
| (b) | `docker inspect` shows no `BEARER_TOKEN`/`METADATA_URL`/`CALLBACK_URL`-like env var, no bind mount (hence no host `docker.sock`) and no `Privileged` flag on the runner; in the `none` variant no DinD sidecar exists, in the `--dind` variant the `role=dind` sidecar has the expected labels, `Privileged=true`, no bind mounts, and the flavor's memory limits | `docs/plan.md` §4's headline acceptance criterion; `ADR-001` |
| (c) | The credential files (plus the `.delivered` marker) actually land in `/run/garm` inside the running container | `ADR-002`'s **exec-tar-into-tmpfs** verification item — proves `docker exec`-streamed delivery works against a real Docker daemon, not just the fake client used in unit tests |
| (d) | `/actions-runner/.runner` is a working symlink into `/run/garm`, and the runner process is actually running | `ADR-002`'s **actions/runner symlink-compat** verification item — proves the real `.NET` runner accepts a symlinked config file instead of falling back to a writable-layer copy |
| (e) | After the job completes: the runner container (and DinD sidecar, socket and `dind-state` volumes in the `--dind` variant), its per-job network, and its named workspace volume are all gone; the runner is gone from GitHub; and no other `garm.docker/managed=true` resource is left over | `docs/plan.md` §4's "job end results in the runner being auto-removed... and the runner... [is] gone" criterion, for the variant that was run |

## What this demo does not cover

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

## Running the demo

```sh
export REPO=<garm-repo-id-or-owner/name>   # from `garm-cli repo list`
scripts/demo-m0.sh                         # scale set, dind_mode=none, v0.2.4 release
scripts/demo-m0.sh --dind                  # same, plus the privileged-sidecar variant
scripts/demo-m0.sh --pool                  # webhook-driven pool instead of a scale set
```

Release mode first checks that the release is published (an HTTP HEAD
request for its `SHA256SUMS`) and fails early, before any download, if it
is not: "release vX.Y.Z not published yet - use --from-source to build
from this checkout".
`v0.2.0` and earlier are known to fail against GARM v0.2.x scale sets, so
passing `--version` below `v0.2.1` with a scale-set target prints a warning
(it does not block; older GARM or `--pool` may still work).

This downloads and verifies the `v0.2.4` release (default), then runs every
step below in order, pausing for a confirmed manual action wherever one is
needed (adding the `[[provider]]` section to GARM's config, creating the
pool, confirming the GitHub UI). Pass `-y`/`--yes` to auto-confirm every
pause, `--from-source` to build from this checkout instead, `--version
vX.Y.Z` to validate a different release, `--pool` for a pool, `--dind` for
the DinD variant, or run a single step directly
(`scripts/demo-m0.sh verify`, etc.) — see `scripts/demo-m0.sh --help` for
the full command list and every environment variable it accepts.

**Modes are remembered between steps.** `preflight` (and `all`) record the
effective `PROVIDER_SOURCE`, `PROVIDER_VERSION`, `GARM_TARGET`,
`GATE_VARIANT`, `PROVIDER_INTERFACE_VERSION`, `RUNNER_IMAGE_NAME` and, when
set, `GATE_REGISTRY_MIRROR` / `GATE_INSECURE_REGISTRY` and the expected
`LOCAL_PROVIDER_SHA256` in
`$PROVIDER_CONFIG_DIR/gate-modes` (mode 0600, plain `KEY=value` lines that
are parsed, never sourced). A later single step run without a flag for a
mode uses the saved value and logs `using saved <mode>=<value>`, so
`scripts/demo-m0.sh --dind preflight` followed by a plain `pool`, `watch`,
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
  `v0.2.4`) binary asset for this host's architecture and its
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
  tag).

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
(commonly `/etc/garm/config.toml`, see `docs/research.md` §1.H and
[Providers](https://github.com/cloudbase/garm/blob/main/doc/providers.md)):
the provider executable's absolute path, the generated config file's
absolute path, and `interface_version = "v0.1.0"` by default
(`PROVIDER_INTERFACE_VERSION=v0.1.1` overrides it). Pauses for you to add it
and restart/reload GARM, then confirm with `garm-cli provider list`.

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
(fixed by design — this demo only ever needs one runner to exist at a
time), `--extra-specs` in the `--dind` variant, `--os-arch` set to this host's
auto-detected architecture (`amd64` or `arm64`; `OS_ARCH` may only restate it),
and placeholder `--image`/`--flavor` values (required by `garm-cli`, but
informational only for this provider: `CreateInstance` always uses the
config's — or a selected flavor's — `runner_image` and ignores the
bootstrap `image` field — `internal/provider/create.go`'s `ensureImage`).
Pauses for you to create it and confirm with `garm-cli scaleset list`
(`garm-cli pool list` in pool mode).

### Step 3 — trigger the workflow and watch

Prints a sample workflow YAML to copy into
`<your-test-repo>/.github/workflows/garm-m0-demo.yml` — `runs-on` set to
the scale set's name (or the pool's tags with `--pool`), and a job that does
`echo hello && env | sort | head` (plus a short `sleep` so the ephemeral
runner stays up long enough for Step 4's checks — this runner is
single-job ephemeral by design, `ADR-002`; the `--dind` variant adds
`docker run --rm hello-world`, a `docker build` step, a shared-workspace
step and a published-port step). Then polls
`docker ps --filter label=garm.docker/managed=true --filter label=garm.docker/role=runner` and GARM's runner list
(`garm-cli scaleset runner list <id>`, the id looked up by name, or
`SCALESET_ID`) every few seconds until you've triggered the workflow and a
runner appears (Ctrl+C to move on early). Always select the runner by
`garm.docker/role=runner`, never by `garm.docker/managed=true` alone: the
provider also starts short-lived cache-seeding helper containers
(`garm-seed-*`) that carry `managed=true`.

### Step 4(a-d) — verify

Run it once the runner is `idle` (GARM's runner list, or `Listening for Jobs`
in `docker logs`), not the moment its container appears: the entrypoint links
the credential files into the install directory a few seconds after the
container starts, and check (d) fails if it runs before that. The job's hold
(30 s, 60 s with `--dind`, 120 s with `--dind` and `GATE_REGISTRY_MIRROR`)
leaves enough time for it.

Resolves the current managed runner container and checks, printing
PASS/FAIL for each: ownership labels on the container, the labeled per-job
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
reaper/polling cadence governs how quickly this happens, `docs/research.md`
§1.F) and checks for any leftover `garm.docker/managed=true` container,
**network**, or volume. Only persistent cache volumes (`garm.docker/cache=true`
and no `garm.docker/instance-name`) are excluded by label and listed
informationally; a volume with an instance-name is job-scoped and counts as a
leftover even if it also carries `cache=true`. The per-job network and the named workspace volume
both carry `garm.docker/managed=true` (as of M1 — `ADR-001`/`ADR-004`), so
`DeleteInstance`'s teardown removes both explicitly; a leftover one is a
real failure this check catches. This is distinct from the persistent,
repo-scoped cache volumes (`ADR-003`), which carry `garm.docker/cache=true`
instead and are deliberately never swept by any teardown path.

### Cleanup

Prints, but never runs, the manual cleanup commands: disable and delete the
scale set (`garm-cli scaleset update <id> --enabled=false`, `garm-cli
scaleset delete <id>`; the pool equivalents with `--pool`),
confirm no managed Docker resources (containers, networks, volumes) remain,
remove the demo-local binary/config, and remove the `[[provider]]` section
from GARM's config. It also reminds you that persistent cache volumes are
never touched by cleanup and shows the manual `docker volume prune` command
to reclaim them.

## What each verification item proves

| Check | Mechanism | Why it can't be fully verified by unit tests alone |
|---|---|---|
| (c) exec-tar-into-tmpfs delivery | `docker exec <runner> test -e /run/garm/<file>` for each credential file plus `.delivered` | `internal/docker/fake.go`'s fake client cannot reproduce moby's real tmpfs/mount-namespace semantics that made the original `docker cp` design unbuildable (`ADR-002`'s amendment) — only a real daemon proves the `docker exec`-streamed tar actually lands in a real memory-backed tmpfs |
| (d) actions/runner symlink-compat | `docker exec <runner> ls -la /actions-runner/.runner` shows `-> /run/garm/runner`, plus `docker top` shows the runner process running | Whether the real, unmodified `.NET` `actions/runner` binary accepts a symlinked `.runner`/`.credentials`/`.credentials_rsaparams` instead of requiring a real file is an external-binary behavior this repo's own tests cannot exercise (`ADR-002`'s open questions) |

## Troubleshooting

| Symptom | Likely cause | What to check |
|---|---|---|
| Step 0 fails with "no published binary for this host OS" | Running `release` mode (the default) on a non-Linux host | Only `linux/amd64`/`linux/arm64` provider binaries are published. Re-run with `--from-source` (builds a native binary for this host), or run this gate from a Linux host/VM. |
| Step 0 fails with a checksum mismatch | A corrupted or tampered download, a stale local copy at `PROVIDER_BIN_DIR`, or a `PROVIDER_VERSION` whose `SHA256SUMS` doesn't list this asset | Re-run Step 0 (it re-downloads); check `PROVIDER_VERSION`/`OS_ARCH` are correct; check https://github.com/TzuH-Hsu/garm-provider-docker/releases for the release's actual asset names. |
| Step 0 fails to resolve the runner image digest | `docker buildx` missing, or this host cannot reach `ghcr.io` | Confirm `docker buildx version` works; check outbound network/proxy/firewall rules for `ghcr.io`. |
| No job is ever picked up in scale-set mode | The workflow's `runs-on` does not exactly match the scale set name, the scale set is disabled, or GARM cannot reach GitHub (outbound HTTPS) | `garm-cli scaleset show <id>` (enabled? name?); compare `runs-on` with the name in the script's output; check GARM's logs for message-queue errors. |
| No job is ever picked up in pool mode (`--pool`) | GitHub cannot deliver webhooks to GARM (e.g. GARM behind a NAT) | Use the default scale-set mode, or expose the webhook endpoint. |
| `--dind` Step 4(b) reports the sidecar missing or the runner cannot reach its daemon | `allowed_dind_modes`/`extra_specs` not applied, the sidecar failed to start, or `docker:dind` could not be pulled | `docker ps -a --filter label=garm.docker/role=dind`; `docker logs <sidecar>`; confirm the scale set was created with the printed `--extra-specs`. |
| `--dind` Step 4(b) reports the sidecar not in the runner's network namespace, or a published port not reachable at `127.0.0.1` | A provider older than the 2026-10-12 shared network namespace change (ADR-001), or a runner image without `curl` | Check the provider version; `docker inspect -f '{{.HostConfig.NetworkMode}}' <sidecar>` must print `container:<runner id>`. |
| Runner container never appears after `garm-cli scaleset add` / `pool add` | The provider failed silently from GARM's point of view, or the pool/provider isn't wired up correctly | Check GARM's own log output (`docker logs garm` if run via Docker, `journalctl -u garm` if run via systemd) for lines mentioning this provider's executable or name — GARM logs a provider subprocess's stderr on failure. Confirm `garm-cli provider list` shows the provider and `garm-cli pool show <POOL_ID>` shows it `enabled`. This provider logs structured key=value lines (`internal/logging`); look for `level=ERROR` entries. |
| Step 4(a)'s job-network or workspace-volume check fails | An older provider build predating M1's per-job network/named volume, or a config/topology bug | Confirm you are validating the intended `PROVIDER_VERSION`/build; check `docker network ls`/`docker volume ls --filter label=garm.docker/managed=true` directly for what actually exists. |
| Step 4(c) times out or reports missing credential files | The provider's fetch-first `docker exec`-streamed tar delivery didn't complete before the entrypoint's `GARM_CRED_WAIT_SECONDS` (120s default) expired, or the exec itself failed | Check the provider's own stderr (surfaced via GARM's logs, see above) for a `failed to deliver credentials` error; check `docker logs <runner>` for the entrypoint's `waiting up to ...s for the delivery marker` message and whether it timed out. A slow/overloaded Docker daemon or a metadata-service fetch that ran close to the 60s aggregate deadline are the two likely causes (`ADR-002`). |
| Step 4(d) reports `/actions-runner/.runner` is NOT a symlink with `ls: cannot access`, but the runner then starts and the job runs | `verify` ran while the runner was still `installing`, before the entrypoint had linked the credential files | Re-run the whole gate and start `verify` once the runner is `idle`; the link is in place a few seconds after the container starts. |
| Step 4(d) reports `/actions-runner/.runner` is NOT a symlink, or the runner process never starts | `actions/runner`'s `run.sh` (or the JIT/non-JIT path in `myoung34/github-runner`) rejected the symlinked config file, or replaced it with a real file | This is exactly the open question `ADR-002` flags as a verification item. If confirmed, the documented fallback is a container-scoped writable-layer copy destroyed at teardown (never host-backed shared storage) — see `ADR-002`'s "symlink compatibility" note for the fallback design; this repo's entrypoint (`runner-images/noble/entrypoint.sh`) would need updating to implement it before this gate could be considered passing. |
| Step 4(e) times out waiting for the container to disappear | GARM hasn't yet noticed (via its own GitHub-runner-list polling) that the ephemeral runner finished, or `DeleteInstance` itself is failing | Runner callbacks are not implemented in this provider (deferred, `ADR-002`) — there is no config field or code path for them — so GARM always learns a job finished by polling, and there is no fixed upper bound faster than GARM's own reap cycle. If it never resolves, check GARM's logs for a `DeleteInstance` error against this provider, and try `garm-cli runner list --repo <REPO>` to see GARM's own view of runner state. |
| Step 4(e) reports a leftover managed network or volume (persistent cache volumes — `garm.docker/cache=true` with no instance-name — are excluded) | The named job network/workspace volume (M1) failed to tear down — a real bug, not the old "anonymous volume" behavior | Check `docker network ls`/`docker volume ls --filter label=garm.docker/managed=true` for what's left, and the provider's stderr (via GARM's logs) for a teardown error (`internal/topology/teardown.go`). |
| `garm-cli pool add` rejects `--image`/`--flavor` as invalid | Confusing these with a real cloud image/flavor | For this provider these two flags are placeholders only — any non-empty value satisfies `garm-cli`; see Step 2 above. |

## Recording a result

Once this gate has actually been run against a real GARM instance, record
the following somewhere durable (e.g. a PR description, an issue, or a
note alongside the release) so the result can be cited later without
re-running it:

- **Versions**: the GARM version (`garm-cli controller show` prints the
  controller's GARM version), this provider's `PROVIDER_VERSION` (or the
  `git describe` version if `--from-source`), and the resolved
  `ghcr.io/tzuh-hsu/garm-runner-noble` or `garm-runner-resolute` digest Step 0
  printed.
- **Host**: OS and architecture (`uname -sm`) and `docker version`'s
  `Server.Version`.
- **The provider binary, when `LOCAL_PROVIDER_BIN` was used**: the git
  commit it was built from (and whether the tree was clean) and its sha256,
  as given in `LOCAL_PROVIDER_SHA256` and echoed by the preflight summary. A
  result from a local binary says nothing about the published release asset
  until the release's own `SHA256SUMS` entry is shown to equal that sha256
  (or the release is gated again in plain release mode), so state which it is.
- **Per-item PASS/FAIL**: the (a)–(e) results from Step 4's output (paste
  the script's own PASS/FAIL lines rather than summarizing from memory).
- **Mode**: `release`, `release` with a local binary, or `--from-source`;
  dispatch target (scale set or `--pool`); variant (`none` or `--dind`); and
  whether `-y`/`--yes` was used.
- **Date** the run happened, since GARM, this provider, and the runner
  image can all move independently between runs.

## Results

The gate has been run end to end against a live GARM with the published
`v0.2.4` release binary, in two variants on the Ubuntu 26.04 image
(`dind_mode=none` and `--dind`), on 2026-10-12, using `scripts/demo-m0.sh` at
0cf2a76. `v0.2.4` changes one thing the gate cares about: in
`privileged-sidecar` mode the sidecar now joins the runner's network namespace
(ADR-001, 2026-10-12 amendment). The Ubuntu 24.04 image was not part of this
run; its last recorded gate is the `v0.2.3` one below.

**Environment**

- GARM v0.2.1 (systemd service, API on loopback), scale-set dispatch (no
  webhook); provider `interface_version` v0.1.0.
- Docker Engine 29.9.0 on Ubuntu 26.04.1, amd64.
- Provider: the `v0.2.4` release binary
  (`garm-provider-docker_v0.2.4_linux_amd64`, sha256
  `f02df425a00c9526f0b7ae6a5d3b19c288503a0e50ee40e08584ff7500e5bd5f`,
  checked against the release's `SHA256SUMS`); `GetVersion` reports
  `v0.2.4`. Release mode, `-y`.
- Runner image, digest-pinned in the generated config (the `:v0.2.4` digest,
  which `:latest` also pointed at; the release workflow rebuilt it, so it
  differs from the `v0.2.3` one):
  - `ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:1c48eb526328814ff4b76a2e4dbd50d1f77135cb04054578ab225fbf934d484a`
- DinD image
  `docker:dind@sha256:0b6d18a4a222e71c88be6034926b4f6cde2fc33c2218643d03ccfddc65995470`
  (unchanged).

**Results**

| Variant | `verify` | GitHub job | `postjob` |
|---|---|---|---|
| Ubuntu 26.04, `--dind` | 53 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |
| Ubuntu 26.04, `none` | 28 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |

- No WARN lines in either `verify`. `postjob` reports "no managed runner
  container remains" and "no leftover job-scoped garm.docker/managed=true
  containers, networks, or volumes"; the persistent cache volumes are reported
  as informational.
- The `none` count matches the `v0.2.3` run (28). The `--dind` count is 53
  against 50: the two checks that the sidecar sits on the runner's per-job
  network are gone, and five are new (see below).
- Each variant was preflighted separately (`preflight --version v0.2.4
  --runner-image resolute --dind`, then the same without `--dind` and with the
  `none` scale set), and `verify` was started only after the runner logged
  `Listening for Jobs` (GARM status `idle`). The sample workflows the script
  prints were used (60 s and 30 s holds).

What is new against the `v0.2.3` run (28 and 50 PASS, script at 92d6c85):

- **Shared network namespace.** The sidecar's `NetworkMode` is
  `container:<runner id>`, it has no network attachment of its own, and the
  runner is the only container on the per-job network. In the live `--dind`
  run `docker inspect` showed the sidecar `Privileged=true` with no sysctls and
  the runner with `net.ipv6.conf.all.disable_ipv6=1` (the sysctl is applied
  once, by the namespace owner); the runner's `/etc/hosts` still has only the
  `127.0.0.1 localhost` line, no `::1`. `verify` reads the same network
  namespace inode from both containers.
- **Published ports reach the runner's localhost.** `verify` starts a nested
  `busybox` httpd with an ephemeral published port through the runner's
  `DOCKER_HOST` and fetches it from the runner at `127.0.0.1` and at
  `localhost` (2 of the 5 new checks; the other 3 are the namespace checks
  above). The workflow's new step does the same inside the job, and the job
  log shows `127.0.0.1:<port> -> published-ok` and
  `localhost:<port> -> published-ok`. The `v0.2.3` gate had no such check;
  the `ECONNREFUSED` this fixes is described in the ADR-001 amendment.
- **Runner status.** In both runs GARM's runner list showed `pending`,
  `installing`, `idle` (3 s after `installing`), and GARM logged the job as
  started 37 s (`--dind`) and 33 s (`none`) after `idle`. GARM's log has no
  `invalid runner status transition` line over the run windows, and no WARN
  or ERROR line for the gate's scale sets (see "Known upstream issue").
- **Teardown.** GARM began deleting the instance within 2 s of logging the
  job completed in both runs, and `postjob` found no leftover container,
  sidecar, job network, job-scoped volume or socket and `dind-state` volume.
- **Release plumbing.** The release carries the `linux/amd64` and
  `linux/arm64` binaries and `SHA256SUMS`, and the release-triggered
  "Container Images" run succeeded. `:latest` and `:v0.2.4` resolve to the same
  digest for `garm-runner-noble` (`14599ec9...`), `garm-runner-resolute`
  (`1c48eb52...`) and `garm-provider-docker` (`621d34ee...`).

## Previous release: v0.2.3

The gate had been run end to end against a live GARM with the published
`v0.2.3` release binary, in three variants (Ubuntu 24.04 image with
`dind_mode=none`, Ubuntu 26.04 image with `none`, and Ubuntu 26.04 image with
`--dind`), on 2026-10-12, using `scripts/demo-m0.sh` at 92d6c85. That revision
had neither the shared network namespace nor the published-port check, so this
run says nothing about either.

**Environment**

- GARM v0.2.1 (systemd service, API on loopback), scale-set dispatch (no
  webhook); provider `interface_version` v0.1.0.
- Docker Engine 29.9.0 on Ubuntu 26.04.1, amd64.
- Provider: the `v0.2.3` release binary
  (`garm-provider-docker_v0.2.3_linux_amd64`, sha256
  `f3786f450296ce68f9c6b731a32cdeb1f902bd36aa3172b24f24c47f2221499e`,
  checked against the release's `SHA256SUMS`); `GetVersion` reports
  `v0.2.3`. Release mode, `-y`.
- Runner images, digest-pinned in the generated config (the `:v0.2.3`
  digests, which `:latest` also pointed at):
  - `ghcr.io/tzuh-hsu/garm-runner-noble@sha256:a1f5beab548f690e6da742c11f885024a40539eb9ba23cfbea83691409624824`
  - `ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:805d733f90dfe872d531d00d2b8fea92f5977e9de2d46be7b8f528e2f1b77dea`
- DinD image
  `docker:dind@sha256:0b6d18a4a222e71c88be6034926b4f6cde2fc33c2218643d03ccfddc65995470`.

**Results**

| Variant | `verify` | GitHub job | `postjob` |
|---|---|---|---|
| Ubuntu 24.04, `none` | 28 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |
| Ubuntu 26.04, `none` | 28 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |
| Ubuntu 26.04, `--dind` | 50 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |

- No WARN lines in any `verify`. `postjob` reports "no managed runner
  container remains" and "no leftover job-scoped garm.docker/managed=true
  containers, networks, or volumes"; the persistent cache volumes are reported
  as informational.
- In the `--dind` run the job's `docker run hello-world` and `docker build`
  worked, and the shared-workspace step printed `sidecar saw: ok` and
  `runner read back: nested`.
- The counts match the `v0.2.2` run (28 and 50).

Each variant was preflighted separately (`preflight --version v0.2.3
--runner-image noble`, then `--runner-image resolute`, then `--dind
--runner-image resolute`), because the saved variant and image are part of the
remembered modes. The `none` runs used the `none` scale set and the `--dind`
run the scale set created with the DinD `extra_specs`; the sample workflows
the script prints were used (30 s and 60 s holds).

**One earlier attempt, not counted.** The first Ubuntu 24.04 run reported 27
PASS and 1 FAIL: check (d) found no `/actions-runner/.runner`. `verify` had
been started within seconds of the runner container appearing, while GARM
still listed the runner as `installing`; the entrypoint links the credential
files into the install directory shortly after the container starts, and the
link was in place when listed shortly afterwards. The runner then started,
the job succeeded and `postjob` passed (2 PASS). The run above was started
again, with `verify` held until the runner logged `Listening for Jobs`. See
"Step 4(a-d)" and the troubleshooting table.

What is new against the `v0.2.2` run (28 and 50 PASS, efe5014):

- **Ubuntu 26.04 runner image.** `ghcr.io/tzuh-hsu/garm-runner-resolute` is
  published from `v0.2.3`. Inside the live runner, as the `runner` user:

  | | Ubuntu 24.04 (`noble`) | Ubuntu 26.04 (`resolute`) |
  |---|---|---|
  | `gcc` | 13.3.0 | 15.2.0 |
  | `clang-format`, `clang-tidy` | 18.1.3 | 22.1.2 |
  | `cmake` | 3.28.3 | 4.2.3 |
  | `pipx` | 1.4.3 | 1.8.0 |

  On both images the runner user can write to `/usr/local/bin` and
  `sudo -n true` succeeds (the runner user is in the `sudo` group, as
  `runner-images/README.md` documents). The runner's PID 1 (`run.sh`, after the entrypoint) has an
  open-file soft limit of 65536 against a 524288 hard limit; `Runner.Listener`
  shows 524288. `/etc/hosts` has only the `127.0.0.1 localhost` line, no `::1`.
- **Caches keyed on the image's OS release.** The toolcache and pnpm volumes
  now carry the OS release in their name, so the two images never share one:
  `garm-cache-toolcache-<repo-key>-ubuntu-24.04-1` and
  `garm-cache-toolcache-<repo-key>-ubuntu-26.04-1` (likewise
  `garm-cache-pnpm-<repo-key>-ubuntu-24.04-9` and `...-ubuntu-26.04-9`)
  existed side by side after the runs, next to the unkeyed volumes that earlier
  releases created, which `v0.2.3` leaves alone. The externals cache is
  keyed by image digest, so each image got its own
  (`garm-cache-externals-<digest>`). The persistent cache volumes grew from 24
  to 27 over the first Ubuntu 26.04 run (its toolcache, pnpm and externals
  volumes).
- **Runner status.** In all runs GARM's runner list showed `pending`,
  `installing`, `idle`, then `active` once the job started (`idle` came 4 to 5 s
  after `installing`, and `active` 15 s (the earlier attempt), 34 s, 32 s and
  35 s after `idle`), and GARM's log has no `invalid runner status transition`
  line and no WARN or ERROR line over the run windows (see "Known upstream
  issue").
- **Teardown.** The runner container (and the sidecar in `--dind`) was gone
  within 1 to 2 s of GARM marking the instance `terminated`, with no leftover
  container, network or job-scoped volume, and no defunct process.
- **Release plumbing.** The release carries the `linux/amd64` and
  `linux/arm64` binaries and `SHA256SUMS`, and the release-triggered
  "Container Images" run succeeded. `:latest` and `:v0.2.3` resolve to the same
  digest for `garm-runner-noble` (`a1f5beab...`), `garm-runner-resolute`
  (`805d733f...`) and `garm-provider-docker` (`70d4bee4...`).

**First attempt failed, which is why the gate exists.** With the v0.2.0
release binary, `CreateInstance` rejected the `runner_install_template`
that GARM injects, and `DeleteInstance`/`GetInstance` failed with
"missing pool ID" for scale-set instances. A separate GARM v0.1.1
`ListInstances` bug was also hit. The first two are fixed in `v0.2.1`.

**Earlier runs** (superseded by the results above): `v0.2.2` passed the
revision at efe5014 with 28 PASS for `none` and 50 for `--dind` (runner image
digest `sha256:de8647b0...`), `v0.2.1` passed an earlier one with 26 and 48
(digest `sha256:10b82dc4...`), and before that the gate was run repeatedly
against pre-release builds as checks were added. Each passed when first added.

Verified as a consequence: tmpfs credential delivery against a real
daemon, a symlinked `.runner` being accepted by `actions/runner` in JIT
mode, the scale-set path against a live GARM, and the `--dind` variant
against a real daemon.

## Known upstream issue

Root cause (GARM v0.2.1, scale sets): GARM's runner-status state machine
has no `pending` to `active` edge (`RunnerStatusTransitions` in
`params/params.go`: `pending` may only go to `failed`, `installing` or
`terminated`; `active` is reachable only via `idle`). With runner
callbacks disabled (the default), the runner stays `pending`. The
scale-set `JobStarted` handler uses the transition-checked update, so it
fails with `invalid runner status transition from pending to active`. The
scale-set listener then does not acknowledge the message and refetches it,
so the scale set processes no further messages (no new job pickup, no
scale-up, `JobCompleted` delayed) until GARM's periodic consolidation
removes the runner, up to about 5 minutes after the job. The job itself
still succeeds. Pools are unaffected, because they use a forced update.

Fixed upstream in cloudbase/garm#817 (commit `1a9aeff2ae`, "Allow pending
and installing runners to become active"), present on `main` and
`release/v0.2` but not in a tagged release as of 2026-10-11.

**Narrowed by this provider since v0.2.1.** The provider reports the
runner `installing` and then `idle` to GARM itself (`[status_report]`, on
by default; `idle` follows `Listening for Jobs`, so it is not instant). This
shrinks the stall to a short window rather than removing it. If GARM
delivers `JobStarted` while the row is still `installing` (the runner can
start polling before `idle` is posted), the checked update fails with
`invalid runner status transition from installing to active`, because
`RunnerStatusTransitions` allows `installing` only to `failed`, `idle`,
`terminated` or `installing`. The scale-set listener
(`handleSessionMessage` and `loop` in `workers/scaleset/scaleset_listener.go`)
does not acknowledge the message and refetches it immediately, so once the
watcher posts `idle` the retried `JobStarted` applies: a brief, self-clearing
burst of that error. The original stall (until GARM's consolidation, up to
about 5 minutes) returns whenever `idle` is not posted: the wait times out, reporting is
disabled (`timeout_seconds = 0`), a callback still fails after its retries (a
failed `installing` report skips the wait, a failed `idle` report leaves the
row `installing`), the watcher cannot be started, or GARM restarts while the
watcher waits (see
[`[status_report]`](config-reference.md#status_report-runner-status-reported-to-garm)).
The gate runs above showed
`idle` 15 to 37 s before `active` and no transition error in GARM's log, but
that ordering is not guaranteed. Upstream cloudbase/garm#817 removes the
race and the stall entirely once it is in a release.

## Still unverified

- The Ubuntu 24.04 runner image with `v0.2.4` (it was gated with `v0.2.3`;
  `v0.2.4` rebuilt it).
- Published ports on the runner's `localhost` under `sysbox-runc`, whose
  sidecar keeps its own network namespace (ADR-001, 2026-10-12 amendment).
- The non-JIT fallback path (`config.sh` writing credentials through the
  pre-created symlinks onto the tmpfs, `ADR-002`).
- An arm64 host.
- Pool and webhook dispatch mode (`--pool`).
