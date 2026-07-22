# Implementation Plan

## 1. Overview

`garm-provider-docker` is an external provider for [GARM](https://github.com/cloudbase/garm) that implements instance lifecycle management on a single Docker host. GARM itself handles GitHub App authentication, scale-set long-polling, JIT runner-config generation, and pool scheduling; this provider's job is narrow and deliberate: create the containers, networks, and volumes a job needs, deliver credentials to them safely, and tear everything down cleanly when the job ends. Its differentiators versus prior art are a per-job isolation topology (every job gets its own network, runner, and — where enabled — Docker-in-Docker sidecar, all destroyed together), first-class Docker-in-Docker support that does not require Sysbox, and a NAS-first target (Synology DSM, Unraid, Raspberry Pi, and generic Linux, on both `linux/amd64` and `linux/arm64`).

The design rationale behind every major decision below is recorded in `research.md` (the accompanying research document) and in five Architecture Decision Records:

- [ADR-001: Docker-in-Docker (DinD) Strategy](adr/ADR-001-dind-strategy.md)
- [ADR-002: Runner Image and JIT Config Delivery](adr/ADR-002-runner-image-and-jit-delivery.md)
- [ADR-003: Cache Keying and Lifecycle](adr/ADR-003-cache-keying-and-lifecycle.md)
- [ADR-004: Teardown and Orphan Cleanup](adr/ADR-004-teardown-and-orphan-cleanup.md)
- [ADR-005: Config and extra_specs Schema](adr/ADR-005-config-and-extra-specs-schema.md)

## 2. Repo layout & Go design

- Module: `github.com/TzuH-Hsu/garm-provider-docker`.
- Dependency: `github.com/cloudbase/garm-provider-common` **v0.1.9** — the current execution package, not the older v0.1.0 pin used by some existing community providers.
- Flat root `main.go`: wires `execution.GetEnvironment()` into `execution.Run(...)` and delegates everything else to internal packages.
- `internal/provider` — the `ExternalProvider` implementation, one file per command family (`CreateInstance`, `DeleteInstance`, `GetInstance`, `ListInstances`, `RemoveAllInstances`, `GetVersion`, plus the v0.1.1 self-description methods from ADR-005).
- `internal/docker` — a narrow, mockable `DockerClient` interface wrapping the moby SDK, so `internal/provider` and `internal/topology` never talk to the SDK directly.
- `internal/topology` — `createAllocation`, `teardownAllocation`, `sweepOrphans`: the orchestration logic from ADR-001 and ADR-004.
- `internal/spec` — pure functions: label builders, environment builders, name builders, cache-key derivation (ADR-003), and JIT config plumbing (ADR-002). Kept pure and side-effect-free specifically so they are cheap to table-test.
- `internal/config` — TOML loading, defaults, and validation (ADR-005), plus the `go:embed`-ed provider-config JSON Schema (`schema.json`, exposed via `JSONSchema()`).
- `internal/extraspecs` — the `extra_specs` Go struct, its `go:embed`-ed draft-07 JSON Schema (`schema.json`), fail-closed `gojsonschema` validation, the reserved-env denylist, and `Resolve` against the config ceiling/flavor/memory machinery (ADR-005, M3-W1). The two JSON Schemas from ADR-005 are co-located with the structs they describe (config schema in `internal/config`, extra_specs schema here) rather than in a single `internal/schema` package, to keep each next to its struct and minimize drift.
- `internal/version` — build-time version metadata injected via `-ldflags`.
- `runner-images/noble/` — the `Dockerfile` and `entrypoint.sh` for the runner image (ADR-002).
- `docs/` — this plan, the ADRs, and `research.md`.
- `.github/workflows/` — CI: build, lint, unit tests, integration harness (M3), and release automation (M4).

## 3. Milestones

### M0 — Spike (go/no-go)

Scope: pool mode only, `dind_mode = "none"` (ADR-001), `linux/amd64` only, manual/local config — the smallest slice that proves the credential-handling design end to end.

1. Module bootstrap, `execution` wiring, and `GetVersion` (test: invoke the built binary with `GARM_COMMAND=GetVersion` and assert the JSON response shape).
2. Minimal `internal/config` package (parse, defaults, no schema validation yet).
3. `DockerClient` interface plus a moby SDK implementation.
4. `internal/spec` env/label/name builders, covered by table tests.
5. Provider-side JIT credential-file fetch and delivery (ADR-002): the create → start → entrypoint-poll → `docker exec`-run `tar -x` streaming sequence into a job-scoped tmpfs mount, gated by an atomic `.delivered` marker (the tar's last entry) so the entrypoint never observes a partial credential set, tested against a fake metadata HTTPS server that asserts the `Bearer` token and the `ca-cert-bundle` are presented/trusted correctly by the provider and never leak past it — no host-side temp file, no environment variable.
6. Runner image v0: digest-pinned `myoung34` base plus custom entrypoint (ADR-002), `linux/amd64` build only.
7. `CreateInstance` in `none` mode (ADR-001).
8. `DeleteInstance`/`GetInstance`/`ListInstances`: idempotent, exit code 30 on already-gone, ID-or-name resolver, label-filter-based lookup (ADR-004).
9. A demo script: point the built provider at a real GARM instance and a throwaway test repository running a trivial echo workflow, and drive the full lifecycle — `CreateInstance` → JIT registration → job runs → `DeleteInstance` → runner confirmed gone from GitHub.

**Go/no-go gate:** the demo script passes, **and** `docker inspect` of the runner container shows no host Docker socket, no GARM credentials, and no GitHub App key, in either its environment or its mounts.

### M1 — DinD, isolation, and teardown

1. Job network provisioning (ADR-001), including sizing `default-address-pools` for the target concurrency ceiling and a clean `provider_fault` on pool exhaustion rather than a generic error.
2. Socket-volume wiring and `DOCKER_HOST` injection for DinD modes.
3. `privileged-sidecar` mode: explicit storage driver, TLS disabled, entrypoint wait-for-docker loop (ADR-001).
4. `sysbox-runc` mode: the two differing `HostConfig` fields (ADR-001).
5. Full allocation orchestration in `internal/topology`: creation guard, delete ordering, idempotency (ADR-004).
6. Final label schema and `RemoveAllInstances` (ADR-004).
7. Opportunistic orphan sweep during `CreateInstance`/`ListInstances` (ADR-004).

**Acceptance:** all five per-allocation resource kinds (runner, DinD sidecar, socket volume, `dind-state` volume, job network) are destroyed on delete, with no cross-job residue; the host Docker socket is unreachable from inside the runner in every mode.

### M2 — Caches

1. Cache-key derivation from normalized repository URL (ADR-003).
2. Toolcache volume wiring.
3. pnpm store volume — resolve the mount path/environment-variable question (ADR-003 open questions) before implementing.
4. Externals volume: keyed by image digest, with init-copy seeding on first use (ADR-003).
5. Opportunistic GC plus the 7-day diagnostic-log prune (ADR-003).

**Acceptance:** toolcache and pnpm store are hit (not re-populated) across successive jobs against the same repository; generation and pnpm-major bumps produce fresh volumes; no cache content is shared across repositories except the externals volume.

### M3 — Hardening and tests

1. Unit tests at ≥80% coverage for `internal/spec`, `internal/config`, and `internal/topology`, all against the mocked `DockerClient`.
2. An integration test harness: CI runs `docker:dind` as a disposable daemon, and tests exec the provider binary directly with crafted environment variables, a crafted stdin `BootstrapInstance` JSON payload, and a fake metadata server standing in for GARM.
3. Integration scenarios: full lifecycle; cancellation (delete mid-run); timeout (metadata server withholds the JIT config, simulating GARM's own delete); image-pull failure (the creation guard must leave zero orphans); a simulated provider crash (the orphan sweep must collect the leftovers on the next invocation); ID-or-name resolution; and both idempotency exit codes (30, 31).
   - **Concurrent-creation race** (ADR-004): launch two or more `CreateInstance` subprocesses in parallel for distinct instance names, with one intentionally delayed/timed-out mid-create (e.g. its metadata fetch stalls); assert the orphan sweep triggered by the other's `CreateInstance`/`ListInstances` calls never deletes the delayed allocation's claim-marker network/volumes while it is still within the concurrency grace window, and assert a genuine timed-out create is still eventually cleaned up once past that window. Also assert a duplicate `CreateInstance` for the same instance name racing against an in-flight create is correctly detected via the claim marker (exit 31), not a runner-container check.
   - **Runner-callback visibility enumeration** (ADR-002, F10): with `enable_runner_callbacks = true`, assert exactly which values become newly visible in `docker inspect` (the instance token, at minimum) versus the `enable_runner_callbacks = false` baseline, so this deliberate exposure stays bounded and is caught by CI if it ever silently grows.
   - **Foreign-resource non-interference** (negative test): before running the suite, seed the Docker host with unmanaged/foreign containers, volumes, and networks (no `garm.docker/*` labels at all). After `DeleteInstance`, the orphan sweep, and `RemoveAllInstances` all run, assert every foreign resource is untouched (still present, unmodified), and assert `docker system prune` (or any other unscoped destructive command) is never invoked by the provider at any point in the test.
4. `extra_specs` JSON Schema validation and `ValidatePoolInfo` (ADR-005).
5. Structured logging, an error taxonomy, and `provider_fault` reporting.

**Acceptance:** a provider restart or a GARM restart neither disrupts a currently running job nor causes double-provisioning of an instance.

### M4 — Release

1. GHCR images, `linux/amd64` + `linux/arm64`: `ghcr.io/tzuh-hsu/garm-provider-docker` (the provider binary image, if useful as a packaging format) and `ghcr.io/tzuh-hsu/garm-runner-noble` (the runner image, ADR-002).
2. SBOM and provenance via `docker/build-push-action` (`provenance: mode=max`, `sbom: true`); the multi-arch attestation caveat (per-platform attestations vs. cosign keyless signing) is decided at this milestone, not before.
3. Static provider binaries for `linux/amd64` and `linux/arm64`, published with `sha256` checksums.
4. README covering the config reference, the `extra_specs` schema, the DinD-mode matrix, and NAS platform notes, plus example configs.
   - **Trust-topology note**: GARM itself and this provider are the trusted parties in this system and legitimately need host Docker access to do their job — that is by design, not a gap. If the provider is packaged and run as a container (item 1 above), *it* mounts the host Docker socket; the runner and DinD sidecar containers it creates for jobs never do, in any mode (ADR-001). The README states this distinction explicitly and warns operators against ever co-mounting the host socket into a runner container themselves (e.g. via a custom `podTemplate`-equivalent override) — doing so would silently defeat this project's core isolation guarantee regardless of what the provider's own code does correctly.
5. An upstream pull request adding this provider to `cloudbase/garm`'s `doc/providers.md`.

## 4. Acceptance criteria

These are the project's headline acceptance criteria, mapped to the milestone that delivers them:

- Job end results in the runner being auto-removed from GitHub, **and** the runner, DinD sidecar, workspace volume, socket volume, and job network are all gone — **M1**.
- The next job scheduled after a prior job sees no rootfs modifications or leftover files from that prior job — **M1**.
- Toolcache and pnpm store are hit across successive jobs against the same repository — **M2**.
- `docker inspect`/environment of a running runner contains no host Docker socket, no GARM credentials, and no GitHub App key — **M0**.
- A provider or GARM restart neither interrupts a currently running job nor causes double-provisioning — **M3**.

## 5. Open questions (deferred, non-blocking)

- pnpm store mount path and environment-variable convention (resolve before M2).
- Whether out-of-range memory requests from `extra_specs` should be rejected or clamped.
- Whether a single-container Sysbox variant is worth adding as a fourth DinD mode.
- Whether `cgroupns=host` is needed on older NAS kernels.
- Whether an optional diagnostic-log shipping path (syslog/Loki) is worth adding.
- Whether toolcache generation should track image-generation bumps only, or also runner minor-version bumps.
- Ownership of the DinD readiness probe: entrypoint-only, or also provider-side.
- The default value of `allowed_dind_modes` (ADR-001/ADR-005) — all three modes today, versus a more conservative out-of-the-box ceiling given the documented residual host-compromise risk of `privileged-sidecar`.
- Robustness of the entity-scope (repo vs. org vs. enterprise) detection heuristic across forges beyond GitHub (ADR-003) — currently inferred from `repo_url` path depth only.
- Final tuning of the concurrency-safe orphan-sweep grace window introduced to close the concurrent-`CreateInstance` race (ADR-004), and whether it should be a value independent from the existing exited-container grace period.

## 6. Out of scope for v1

- Gitea and other forges — GitHub only for v1, though the non-JIT registration-token fallback path (ADR-002) is still implemented and is not GitHub-specific in shape.
- Rootless Docker-in-Docker.
- Windows and macOS runners.
- Multi-host orchestration — this provider targets a single Docker host by design.
