# M0 End-to-End Demo Runbook

This is the runbook for `scripts/demo-m0.sh`, the harness for `docs/plan.md`
§3 M0 item 9: point the built provider at a real GARM instance and a
throwaway test repository running a trivial echo workflow, and drive the
full lifecycle — `CreateInstance` → JIT registration → job runs →
`DeleteInstance` → runner confirmed gone from GitHub.

Passing this demo is the M0 **go/no-go gate** (`docs/plan.md` §3): the demo
script passes, **and** `docker inspect` of the runner container shows no
host Docker socket, no GARM credentials, and no GitHub App key, in either
its environment or its mounts.

This document does not reproduce GARM's own installation or credential-setup
instructions — it links to the official docs for those and focuses on what
is specific to this provider and this demo.

<!-- TOC -->
- [Prerequisites](#prerequisites)
- [What this demo proves](#what-this-demo-proves)
- [Running the demo](#running-the-demo)
- [What each verification item proves](#what-each-verification-item-proves)
- [Troubleshooting](#troubleshooting)
- [Known unverified until this demo passes](#known-unverified-until-this-demo-passes)
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
   (`garm-cli repo add --owner ... --name ... --credentials ... --install-webhook`,
   see [First Steps §3](https://github.com/cloudbase/garm/blob/main/doc/first-steps.md#3-add-a-repository)).
   Never point this demo at a production repository: it creates a real,
   if trivial, workflow run and a real self-hosted-runner registration.
4. **A Linux Docker host** (or macOS with Docker Desktop) with `docker`
   reachable, and a **Go toolchain** to build the provider binary
   (`go.mod` pins `go 1.25.0`).
5. This repository checked out locally, with `scripts/demo-m0.sh`
   executable (`chmod +x scripts/demo-m0.sh` if needed).

The demo builds its own runner image locally (`docker build
runner-images/noble`) with a mutable dev tag rather than requiring a
published, digest-pinned image — see
[runner-images/noble/README.md](../runner-images/noble/README.md) for the
digest-pin policy this bypasses for local development only
(`allow_unpinned_runner_image = true`, `ADR-002`/`ADR-005` F10).

## What this demo proves

| # | Demo verification item | Maps to |
|---|---|---|
| (a) | Runner container carries the expected `garm.docker/*` ownership labels | `ADR-004`'s label schema |
| (b) | `docker inspect` shows no `BEARER_TOKEN`/`METADATA_URL`/`CALLBACK_URL`-like env var and no host `docker.sock` mount | The M0 go/no-go gate (`docs/plan.md` §3) and the headline acceptance criterion (`docs/plan.md` §4) |
| (c) | The credential files (plus the `.delivered` marker) actually land in `/run/garm` inside the running container | `ADR-002`'s **exec-tar-into-tmpfs** M0-demo verification item — proves `docker exec`-streamed delivery works against a real Docker daemon, not just the fake client used in unit tests |
| (d) | `/actions-runner/.runner` is a working symlink into `/run/garm`, and the runner process is actually running | `ADR-002`'s **actions/runner symlink-compat** M0-demo verification item — proves the real `.NET` runner accepts a symlinked config file instead of falling back to a writable-layer copy |
| (e) | After the job completes: the runner container is gone, the runner is gone from GitHub, and no managed containers/volumes are left over | `docs/plan.md` §4's "job end results in the runner being auto-removed... and the runner... [is] gone" criterion (the DinD/network/volume parts of that criterion are M1-scope; M0 "none" mode only has the runner container and its anonymous workspace volume — `internal/provider/delete.go`) |

## Running the demo

```sh
export REPO=<garm-repo-id-or-owner/name>   # from `garm-cli repo list`
scripts/demo-m0.sh
```

This runs every step below in order, pausing for a confirmed manual action
wherever one is needed (adding the `[[provider]]` section to GARM's config,
creating the pool, confirming the GitHub UI). Pass `-y`/`--yes` to
auto-confirm every pause, or run a single step directly
(`scripts/demo-m0.sh verify`, etc.) — see `scripts/demo-m0.sh --help` for
the full command list and every environment variable it accepts.

No step ever reads, generates, or prints a secret: no GitHub PAT/App key, no
GARM API token, no runner bearer token. `garm-cli`'s own stored profile is
what authenticates against GARM; this script only shells out to it.

### Step 0 — preflight

Checks `docker`, `go`, and `garm-cli` are present and reachable/
authenticated, then:

1. Builds the provider binary with a version stamp:
   `go build -ldflags "-X .../internal/version.Version=<git describe>"`.
2. Builds the runner image locally: `docker build -t <tag>
   runner-images/noble`.
3. Generates a demo provider `config.toml` (at a temp path, printed by the
   script) with `allow_unpinned_runner_image = true` — required because the
   locally built image tag isn't digest-pinned (`internal/config/config.go`).

### Step 1 — register the provider with GARM

Prints the exact `[[provider]]` section to add to GARM's own `config.toml`
(commonly `/etc/garm/config.toml`, see `docs/research.md` §1.H and
[Providers](https://github.com/cloudbase/garm/blob/main/doc/providers.md)):
the provider executable's absolute path, the generated config file's
absolute path, and `interface_version = "v0.1.0"` (shown explicitly; an
empty/omitted value defaults to the same thing, and v0.1.0 is the only
interface surface this M0 provider implements —
`internal/provider/provider.go`). Pauses for you to add it and restart/
reload GARM, then confirm with `garm-cli provider list`.

### Step 2 — create a pool

Prints the exact `garm-cli pool add` command against your `REPO`, with
`--min-idle-runners 0 --max-runners 1` (fixed by design — this demo only
ever needs one runner to exist at a time) and placeholder `--image`/
`--flavor` values (required by `garm-cli`, but informational only for this
provider in M0: `CreateInstance` always uses the config's `runner_image`
and ignores the bootstrap `image` field — `internal/provider/create.go`'s
`ensureImage`). Pauses for you to create it and confirm with `garm-cli pool
list`.

### Step 3 — trigger the workflow and watch

Prints a sample workflow YAML to copy into
`<your-test-repo>/.github/workflows/garm-m0-demo.yml` — `runs-on` set to
your pool's tags, and a job that does `echo hello && env | sort | head`
(plus a short `sleep 30` so the ephemeral runner stays up long enough for
Step 4's checks — this runner is single-job ephemeral by design,
`ADR-002`). Then polls `docker ps --filter label=garm.docker/managed=true`
and `garm-cli runner list` every few seconds until you've triggered the
workflow and a runner appears (Ctrl+C to move on early).

### Step 4(a-d) — verify

Resolves the current managed runner container and checks, printing
PASS/FAIL for each: ownership labels (a); no forbidden env var name or
`docker.sock` mount in `docker inspect` (b) — values are never printed,
only variable names, and even the informational `Config.Env` dump redacts
anything `TOKEN`/`METADATA_URL`/`CALLBACK_URL`-shaped as a defense-in-depth
display safeguard; the credential tmpfs contents (c); and the install-dir
symlink plus a running-process check (d). None of these ever print
credential file *contents* — only filenames, permissions, and variable
*names*.

### Step 4(e) — post-job

Run once the workflow has finished. Asks you to confirm in the GitHub UI
that the runner is gone (this script never asks for a GitHub token, so it
cannot check this itself), then polls for the runner container to
disappear (bounded by `POST_JOB_WAIT_SECONDS`, default 300s — GARM's own
reaper/polling cadence governs how quickly this happens, `docs/research.md`
§1.F) and checks for any leftover `garm.docker/managed=true` container or
volume.

### Cleanup

Prints, but never runs, the manual cleanup commands: disable and delete the
pool (`garm-cli pool update --enabled=false`, `garm-cli pool delete`),
confirm no managed Docker resources remain, remove the demo-built image/
binary/config, and remove the `[[provider]]` section from GARM's config.

## What each verification item proves

| Check | Mechanism | Why it can't be fully verified by unit tests alone |
|---|---|---|
| (c) exec-tar-into-tmpfs delivery | `docker exec <runner> test -e /run/garm/<file>` for each credential file plus `.delivered` | `internal/docker/fake.go`'s fake client cannot reproduce moby's real tmpfs/mount-namespace semantics that made the original `docker cp` design unbuildable (`ADR-002`'s amendment) — only a real daemon proves the `docker exec`-streamed tar actually lands in a real memory-backed tmpfs |
| (d) actions/runner symlink-compat | `docker exec <runner> ls -la /actions-runner/.runner` shows `-> /run/garm/runner`, plus `docker top` shows the runner process running | Whether the real, unmodified `.NET` `actions/runner` binary accepts a symlinked `.runner`/`.credentials`/`.credentials_rsaparams` instead of requiring a real file is an external-binary behavior this repo's own tests cannot exercise (`ADR-002`'s open questions) |

## Troubleshooting

| Symptom | Likely cause | What to check |
|---|---|---|
| Runner container never appears after `garm-cli pool add` | The provider failed silently from GARM's point of view, or the pool/provider isn't wired up correctly | Check GARM's own log output (`docker logs garm` if run via Docker, `journalctl -u garm` if run via systemd) for lines mentioning this provider's executable or name — GARM logs a provider subprocess's stderr on failure. Confirm `garm-cli provider list` shows the provider and `garm-cli pool show <POOL_ID>` shows it `enabled`. |
| Step 4(c) times out or reports missing credential files | The provider's fetch-first `docker exec`-streamed tar delivery didn't complete before the entrypoint's `GARM_CRED_WAIT_SECONDS` (120s default) expired, or the exec itself failed | Check the provider's own stderr (surfaced via GARM's logs, see above) for a `failed to deliver credentials` error; check `docker logs <runner>` for the entrypoint's `waiting up to ...s for the delivery marker` message and whether it timed out. A slow/overloaded Docker daemon or a metadata-service fetch that ran close to the 60s aggregate deadline are the two likely causes (`ADR-002`). |
| Step 4(d) reports `/actions-runner/.runner` is NOT a symlink, or the runner process never starts | `actions/runner`'s `run.sh` (or the JIT/non-JIT path in `myoung34/github-runner`) rejected the symlinked config file, or replaced it with a real file | This is exactly the open question `ADR-002` flags as an M0-demo verification item. If confirmed, the documented fallback is a container-scoped writable-layer copy destroyed at teardown (never host-backed shared storage) — see `ADR-002`'s "M0-demo verification item (symlink compatibility)" note for the fallback design; this repo's entrypoint (`runner-images/noble/entrypoint.sh`) would need updating to implement it before M0 could be considered passing. |
| Step 4(e) times out waiting for the container to disappear | GARM hasn't yet noticed (via its own GitHub-runner-list polling) that the ephemeral runner finished, or `DeleteInstance` itself is failing | `enable_runner_callbacks` defaults to `false` (`ADR-002`), so GARM only learns a job finished by polling — there is no fixed upper bound faster than GARM's own reap cycle. If it never resolves, check GARM's logs for a `DeleteInstance` error against this provider, and try `garm-cli runner list --repo <REPO>` to see GARM's own view of runner state. |
| `garm-cli pool add` rejects `--image`/`--flavor` as invalid | Confusing these with a real cloud image/flavor | For this provider in M0 these two flags are placeholders only — any non-empty value satisfies `garm-cli`; see Step 2 above. |

## Known unverified until this demo passes

Being honest about what is still a design assumption rather than a proven
fact, until this demo has actually been run against a real GARM instance
and a real `actions/runner` binary:

- **Whether `actions/runner` accepts symlinked `.runner`/`.credentials`/
  `.credentials_rsaparams` files at all** (JIT path) — this is the entire
  point of Step 4(d) and `ADR-002`'s symlink-compatibility open question.
  If it does not, `runner-images/noble/entrypoint.sh` needs the documented
  writable-layer fallback before M0 can be considered passing.
- **Whether `config.sh` writes generated credentials *through* the
  pre-created symlinks onto the tmpfs**, rather than replacing them with a
  real file on the writable layer, in the non-JIT fallback path
  (`ADR-002`). The scrub-on-failure trap in the entrypoint is a safety net
  for the failure path only; the success-path assumption itself is
  unverified until exercised.
- **Whether the credential-delivery `docker exec`-streamed tar mechanism
  behaves as designed against a real Docker daemon and a real memory-backed
  tmpfs** — the fake client in `internal/docker/fake.go` cannot reproduce
  the real moby mount-namespace semantics that made the original `docker
  cp`-based design unbuildable in the first place (`ADR-002`'s amendment);
  Step 4(c) is the first time this runs against a real daemon.
- The overall M0 go/no-go gate itself (`docs/plan.md` §3): until this demo
  passes end to end, M1 work should not begin.

Once this demo passes, these move from "design assumption" to "verified
behavior," and any fallback path that turned out to be necessary should be
implemented and this document updated accordingly before M1 begins.
