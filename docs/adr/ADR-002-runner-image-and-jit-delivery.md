# ADR-002: Runner Image and JIT Config Delivery

Status: Accepted (2026-07-19)

## Context

GARM (see ADR-001 for the provider/GARM split of responsibilities) generates just-in-time (JIT) runner configuration for scale-set pools and expects the provider to get that configuration onto the instance it creates. The provider receives a bootstrap payload on stdin that includes an instance token; it must turn that into a running, registered GitHub Actions runner without ever letting GARM's credentials — the instance token, the metadata/callback URLs backing it, or (further upstream) the GitHub App key — reach the container's environment or filesystem. This is one of the project's explicit security red lines and the primary acceptance criterion for the M0 milestone (see plan.md): `docker inspect` of a running runner must show no host Docker socket, no GARM credentials, and no GitHub App key, in either its environment or its mounts.

Two realistic runner base images exist in the ecosystem: `myoung34/github-runner` (tool-rich, Ubuntu/Debian based, actively used by many self-hosted-runner projects) and the official `ghcr.io/actions/actions-runner` (minimal, no entrypoint, designed for JIT-style bootstrapping as used by ARC). Neither ships a ready-made "fetch JIT config yourself and never expose the token" entrypoint; both require the provider to make its own choice about who fetches the JIT config and how it reaches the container.

## Decision

Build and maintain an own runner image at `runner-images/noble/` in this repository, `FROM` a digest-pinned `myoung34/github-runner` Noble (Ubuntu 24.04) base image, with a custom entrypoint. Publish it as `ghcr.io/tzuh-hsu/garm-runner-noble`, as a multi-arch (`linux/amd64` + `linux/arm64`) manifest, digest-pinned in provider config (see ADR-005).

**JIT delivery is provider-side fetch, not container-side fetch.** The provider holds the instance token from the stdin bootstrap payload and never passes it into the container. Per the GARM wire contract (research.md §1.C, §2.A), there is no single base64 `--jitconfig` blob for the provider to fetch or inject: GARM's metadata service serves three **individual** credential files — `.runner`, `.credentials`, `.credentials_rsaparams` — one at a time via `GET {metadata-url}/credentials/{fileName}`, sourced server-side from `Instance.JitConfiguration`. The provider fetches these three files itself over HTTPS, trusting the `ca-cert-bundle` (`[]byte`) supplied in the stdin bootstrap payload for TLS verification — not just the host's default trust store — so that private-CA GARM deployments don't fail this fetch.

Delivery into the container is a four-step sequence, not a one-shot pre-populate, because a Docker `tmpfs` mount only materializes once its container is running: there is no host-side path to write into before the container's main process starts.

1. **Create** the runner container (status `created`, not yet started) with a `tmpfs` mount at the credential directory (e.g. `/run/garm`, `type=tmpfs`, memory-backed, `mode=0700`). The mount is empty at this point.
2. **Start** the container. The entrypoint's first action is to block in a poll loop waiting for the credential files to appear under `/run/garm`, bounded by a timeout that fails the runner cleanly (see Open questions) rather than hanging indefinitely.
3. **Stream** the three fetched credential files into the now-running container's tmpfs via `docker cp` of an in-memory tar stream built directly from the fetched bytes. The provider never writes the credential files to a host-side temporary file at any point — the tar archive is constructed and piped entirely in memory.
4. The entrypoint's poll loop detects the files, places them in the runner's working directory, and execs `run.sh` directly — `config.sh` is skipped entirely, because a JIT-configured runner is single-job ephemeral by construction and needs no separate registration step, and `run.sh` reads `.runner`/`.credentials`/`.credentials_rsaparams` straight from the working directory (matching the reference k8s provider's pattern, research.md §2.A). There is no `--jitconfig` argument anywhere in this path.

In Docker-in-Docker (DinD) modes, the entrypoint's credential-file poll and Docker daemon readiness wait (via `until docker info`) are independent and concurrent; the runner execs `run.sh` only after both complete. In `none` mode, only the credential-file poll applies.

**Resulting properties:** the credential files live only in the container's memory-backed tmpfs — never in the image rootfs, never as a `docker inspect`-visible environment variable, never on host disk — and are destroyed with the rest of the container's tmpfs at teardown (ADR-004). The tmpfs mount is anonymous and per-container, so it is not mountable by, or visible to, any other container on the host; there is no shared credential-staging volume of any kind.

**Base image analysis.** `myoung34/github-runner` has no native JIT support: its stock entrypoint only performs `config.sh --token`, so a custom entrypoint is required regardless of which base is chosen. The official `ghcr.io/actions/actions-runner` image is JIT-friendly out of the box (no entrypoint at all; ARC's convention is to set the `ACTIONS_RUNNER_INPUT_JITCONFIG` environment variable and let the container's default command consume it) but ships almost no build tooling, which would force per-job tool installation and defeat the toolcache design in ADR-003. The tool-rich `myoung34` base wins on that basis; its Noble variant is confirmed to build and run on both target architectures.

**Environment contract injected by the provider, clarified per delivery mode.** The `myoung34`/actions-runner-controller k8s-provider convention informs the variable names, but the two delivery modes genuinely diverge in which variables are meaningful — the entrypoint must not assume the non-JIT set applies in JIT mode:

- **Always set**: `JIT_CONFIG_ENABLED` (`true`/`false`), `RUNNER_WORKDIR`, `GITHUB_URL` (connectivity-check / informational use inside the entrypoint), `DISABLE_RUNNER_UPDATE=true`, and, in DinD modes, `DOCKER_HOST` (ADR-001).
- **JIT mode only** (`JIT_CONFIG_ENABLED=true`): the provider does **not** set `RUNNER_ORG`/`RUNNER_REPO`/`RUNNER_ENTERPRISE`/`RUNNER_GROUP`/`RUNNER_NAME`/`RUNNER_LABELS`/`RUNNER_NO_DEFAULT_LABELS`/`RUNNER_EPHEMERAL`. These are not the source of truth in JIT mode: GARM bakes the runner's name, labels, group, and ephemeral flag into the `.runner`/`.credentials` files themselves at JIT-config-generation time (research.md §1.C), and `run.sh` reads them from those files. An env var named `RUNNER_LABELS` in JIT mode would be inert at best, misleading at worst, so it is deliberately absent rather than set-and-ignored.
- **Non-JIT fallback only** (`disable_jit_config`): `RUNNER_ORG`/`RUNNER_REPO`/`RUNNER_ENTERPRISE` (whichever applies), `RUNNER_GROUP`, `RUNNER_NAME`, `RUNNER_LABELS`, `RUNNER_NO_DEFAULT_LABELS`, and `RUNNER_EPHEMERAL=true` are all provider-injected and consumed by `config.sh --ephemeral --token …` before `run.sh` runs.

**Deliberately absent** from the container's environment: the instance token / bearer token, the metadata URL, and the callback URL. As a direct result, `docker inspect` output — both `Config.Env` and `Mounts` — contains no bearer token and no credential value in any form. This is the only design considered that literally satisfies the acceptance criterion quoted above, because every alternative that puts a JIT config or a bearer token into container environment variables makes that value visible to `docker inspect` by definition.

JIT config validity (~60 minutes per credential-file set) is ample headroom, since the provider fetches and delivers the credential files immediately after starting the container rather than on any delayed schedule.

**Runner callbacks default to off** (`enable_runner_callbacks = false` in config, ADR-005). Enabling this flag is an explicit, informed operator **opt-out** of the credential-invisibility guarantee that is this ADR's central design goal — not a minor trade-off to wave through. Turning it on puts the instance token into the runner container's environment, which means: (1) the token is visible in plaintext to anyone with `docker inspect` access on the host, and (2) it is reachable by the untrusted workflow job code running inside the runner, which could read its own environment and call the metadata service with that token on the job's own behalf. With callbacks off (the default), GARM still reconciles runner state by polling GitHub's own runner list plus this provider's stdout status responses — callbacks are a latency optimization for faster `agent_id` correlation, not a correctness requirement. plan.md's M3 milestone includes a test that enumerates exactly what becomes newly visible when callbacks are turned on, so this exposure is bounded and deliberate rather than assumed.

**Non-JIT fallback**: needed when a pool explicitly sets `disable_jit_config`. In that case the provider fetches a registration token from `/runner-registration-token` instead of the three JIT credential files, delivers it into the container's tmpfs via the identical create → start → poll → `docker cp` sequence described above (now carrying one token file instead of three credential files), and the entrypoint runs `config.sh --ephemeral --token "$(cat …)"` followed by `run.sh`. In practice, GARM scale sets force `jit_config_enabled = true` upstream, so JIT is the primary and expected path; the non-JIT fallback exists for completeness and for non-scale-set pools, and deliberately shares the same provider-side-fetch, never-a-host-temp-file mechanism for a uniform threat model across both paths.

**Version updates**: bumping the base image digest or the `GH_RUNNER_VERSION` build argument triggers a rebuild, which produces a new digest; the operator then updates the digest pinned in provider config. **Image selection is config-only**, via a named flavor map (ADR-005); `extra_specs` cannot carry a raw image reference under any circumstance — this is an explicit design choice to keep image provenance under operator control.

## Rationale

The combination of "tool-rich base + custom entrypoint + provider-side JIT fetch" is the only path that satisfies both the toolcache requirement (ADR-003) and the credential-invisibility requirement simultaneously. Any design that lets the container itself fetch its JIT config, or that passes the config/token through environment variables, fails the `docker inspect` acceptance criterion by construction, regardless of how carefully the fetch is implemented. The corrected create → start → poll → `docker cp` delivery sequence preserves this invariant exactly: the credential files still never touch environment variables or host disk, only a container-local, memory-backed tmpfs populated after the container is already running.

## Alternatives considered

- **Official `ghcr.io/actions/actions-runner` base**: rejected for v1 — no bundled tooling, which would force per-job tool installation and defeat the toolcache.
- **`summerwind/actions-runner`**: rejected — the project is deprecated upstream.
- **From-scratch Ubuntu build**: rejected — would re-implement most of what `myoung34/github-runner` already provides, for no clear benefit.
- **JIT via `ACTIONS_RUNNER_INPUT_JITCONFIG` environment variable** (the ARC/actions-runner convention): rejected — puts the JIT config, and thus the credential, directly into `docker inspect`-visible environment. Note this convention does not actually map onto GARM's wire contract anyway: GARM's metadata service never hands out a single base64 jitconfig blob to fetch (research.md §1.C, §2.A), only the three individual credential files — so this alternative was rejected both on the visibility grounds above and because it would require inventing a blob GARM does not provide.
- **k8s-provider-style `BEARER_TOKEN` entrypoint-fetch pattern** (container fetches its own JIT config using a bearer token supplied via environment): rejected — the GARM-issued bearer token itself becomes visible in `docker inspect`.

## Consequences

- This project must maintain its own `Dockerfile` and entrypoint script and track a rebuild cadence against both `myoung34/github-runner` and upstream `actions/runner` releases.
- Forge scope for v1 is GitHub only; Gitea support is deferred, though the non-JIT registration-token fallback path is still implemented and forge-agnostic in shape.

## Open questions

- The exact tmpfs directory convention for delivered credentials (`/run/garm` above is a working default, not yet finalized) and the exact filenames used inside it.
- The exact timeout for the entrypoint's credential-poll loop before it fails the runner cleanly (a value in the tens of seconds, comparable to the DinD readiness wait in ADR-001, is the likely default — not yet finalized).
- Whether the three credential files should be streamed to the container in a single `docker cp` tar archive or as three separate calls; a single archive is simpler and reduces the window during which the entrypoint's poll loop can observe a partial file set.
- Whether a `ca-cert-bundle` TLS-verification failure during the provider's metadata fetch should be surfaced as its own `provider_fault` category, distinct from a generic fetch failure, to make private-CA misconfiguration easier for operators to diagnose.

Resolved by this revision (previously open): the non-JIT fallback path is confirmed to stay fully provider-side, sharing the same create → start → poll → `docker cp` mechanism as JIT delivery, for a uniform code path and threat model.

See research.md §1.C and §2.A for the ground truth on GARM's per-file `/credentials/{fileName}` metadata contract and the reference k8s provider's direct-`run.sh`-boot pattern this decision is built on, and §3 for the base-image comparison and the upstream JIT/callback behavior. See ADR-001 for the DinD sidecar this runner pairs with and the `DOCKER_HOST` wiring, ADR-003 for the toolcache this base image choice is optimized for, and ADR-005 for how `runner_image`, `dind_image`, and `enable_runner_callbacks` are exposed in config.
