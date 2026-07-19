# ADR-001: Docker-in-Docker (DinD) Strategy

Status: Accepted (2026-07-19)

## Context

`garm-provider-docker` is an external provider executable for [GARM](https://github.com/cloudbase/garm) (`cloudbase/garm`). GARM owns GitHub App authentication, scale-set long-polling, JIT runner-config generation, and pool scheduling; this provider is responsible only for instance lifecycle on a single Docker host, invoked as a subprocess (environment variables and a stdin JSON payload in, a stdout JSON payload out), built against `garm-provider-common` v0.1.9.

The target deployment is a single Docker host, NAS-first: Synology DSM, Unraid, Raspberry Pi, and generic Linux hosts, across `linux/amd64` and `linux/arm64`. Many CI jobs need Docker-in-Docker (DinD) to build and run containers themselves. The intended differentiator versus prior art (in particular the GitHub Actions Runner Controller, ARC, and existing community Docker providers) is per-job isolation topology combined with first-class, configurable DinD support that does not hang a hard dependency on Sysbox — Sysbox has no straightforward install path on Synology DSM or Unraid, the two most common NAS targets.

The trust model that shapes this decision (see also ADR-005): provider config, set by the host operator, is the most trusted input; pool `extra_specs`, set by a GARM admin and read from the stdin `extra_specs` field (not the `GARM_POOL_EXTRASPECS` environment variable, which is empty for scale sets due to an upstream gap — see research.md §1), is a narrower trust tier; the workflow payload running inside the job is untrusted and is never consulted for privileged decisions. The hardest security red line this ADR must satisfy: a runner or the job it executes must never be able to reach the host's Docker socket, under any configuration.

## Decision

Support three config-selectable DinD modes — `none`, `privileged-sidecar` (default), and `sysbox-runc` — sharing one uniform per-allocation topology. DinD, when enabled, is always run as a **sidecar container** sharing a job-scoped Unix socket volume with the runner container, following the pattern established by ARC's `gha-runner-scale-set` Helm chart. The host Docker socket is never mounted into any container in any mode.

**Operator ceiling on `dind_mode`.** The three modes above are what a pool's `extra_specs` may *request* (ADR-005), but the operator gets the final word: provider config defines `allowed_dind_modes`, a list defaulting to all three modes (`["none", "privileged-sidecar", "sysbox-runc"]`), and any `dind_mode` selected via `extra_specs` is validated against this list and rejected if it falls outside it. `extra_specs` can select a mode *within* the allowed set; it can never escalate beyond it, no matter what a GARM admin configures at the pool level. This is the concrete mechanism that keeps the trust-tier boundary described above (operator config > pool `extra_specs` > workflow payload) enforced in code: on a shared or otherwise sensitive host, an operator can set `allowed_dind_modes = ["none"]` to forbid any privileged workload on that host outright, regardless of what any pool's `extra_specs` requests. See ADR-005 for the full `extra_specs` schema and validation behavior this ceiling is implemented in.

Per-allocation resources, created on `CreateInstance` and destroyed on `DeleteInstance` (see ADR-004 for teardown ordering):

- Job network (a labeled bridge network) — all modes.
- Runner container — all modes.
- DinD sidecar container, job-scoped socket volume, and a dedicated `dind-state` volume mounted at `/var/lib/docker` — DinD modes only (`privileged-sidecar`, `sysbox-runc`).
- Workspace volume — all modes.

**`privileged-sidecar` wiring** (default): the DinD sidecar runs `dockerd --host=unix:///var/run/docker.sock --storage-driver=overlay2` with `DOCKER_TLS_CERTDIR=""` — TLS is disabled because the daemon socket is exposed only over the shared volume, never over TCP. This mirrors ARC's own `gha-runner-scale-set` DinD values file. The runner container mounts the same socket volume and sets `DOCKER_HOST=unix:///var/run/docker.sock`; the entrypoint polls for daemon readiness (`until docker info; do sleep …; done`, budget roughly 120 seconds) before proceeding.

**`sysbox-runc` mode**: identical topology to `privileged-sidecar`. Only two `HostConfig` fields differ on the DinD sidecar: `HostConfig.Runtime = "sysbox-runc"` and `Privileged = false`. Everything else — networking, volumes, entrypoint, readiness wait — is shared code.

**Storage driver**: DSM's and other NAS distributions' host filesystems (frequently btrfs, sometimes overlay-on-overlay) make storage-driver autodetection inside the DinD container unreliable. The provider therefore always allocates a dedicated, ephemeral `dind-state` volume and passes an explicit `--storage-driver` flag: `overlay2` by default, with `vfs` documented as the safe (slower, more compatible) fallback. This is a config knob (see ADR-005), never something the payload or `extra_specs` can override implicitly.

**`none` mode**: no DinD sidecar, no socket volume, no `dind-state` volume; `DOCKER_HOST` is left unset in the runner. Job network, workspace volume, and persistent caches (ADR-003) are still provisioned. Any `docker` invocation inside the job fails fast, which is the correct signal for a job that needs Docker but was scheduled onto a `none`-mode pool. This is the M0 spike target (see plan.md) because it lets the rest of the lifecycle (create/delete/list, labeling, JIT delivery) be proven without the added complexity of DinD wiring.

**Job network capacity is finite and must be sized deliberately.** Every allocation, in every mode, creates its own per-job bridge network (see Decision above). Docker draws these from its **default address pools**, a finite resource on any single host — the number of per-job networks the daemon can hand out concurrently is bounded by how that pool is sized, not just by host CPU/RAM. This sets a hard concurrency ceiling on simultaneous jobs that is independent of runner/DinD resource limits, and it is a ceiling operators must size for explicitly (via the daemon's `default-address-pools` configuration) rather than discover by surprise. Pool exhaustion must fail cleanly: `CreateInstance` reports a clear `provider_fault` (not a generic, unattributed error) when network creation fails because the address pool is exhausted, so the failure is immediately diagnosable as a capacity problem rather than a Docker or provider bug.

**Job networks default to `internal=true`.** Per-job bridge networks are created with Docker's `internal` flag set by default, denying them a route to the external network beyond what the runner/DinD containers explicitly need — this is defense-in-depth alongside the `extra_specs` reserved-env denylist (ADR-005) and complements, rather than replaces, the structural "never mount the host socket" guarantee above. Operators who need broader job network egress (e.g. package registry access) can disable `internal` via config; this is a config-only knob, never an `extra_specs` one, consistent with this ADR's trust model. Independently of this, operators are advised to ensure the host Docker daemon itself is not configured to listen on TCP (`dockerd -H tcp://…` or an equivalent `daemon.json` `hosts` entry) — a TCP-exposed daemon socket would bypass every one of this ADR's per-job containment guarantees regardless of `internal` networking, since it is a host-level exposure, not a per-job one.

## Rationale

Sysbox is not installable on Synology DSM or Unraid through any supported path, so it cannot be the default — a NAS-first provider that defaulted to Sysbox would be unusable out of the box for its primary audience. `privileged-sidecar` is the only DinD mechanism universally available on every target platform, so it is the default; `sysbox-runc` remains available as an opt-in for hosts that have it installed and want reduced-privilege isolation. Keeping the host socket out of every mode's mount table is a **structural** guarantee of the "no host socket" red line rather than a conventional one enforced only by code review: there is no code path, in any mode, that mounts `/var/run/docker.sock` from the host into a job-scoped container. The `allowed_dind_modes` operator ceiling exists because the trust-tier ordering in Context (operator config strictly outranks pool `extra_specs`) would otherwise be only aspirational for this one setting — without it, a GARM admin's `extra_specs` could unilaterally opt a pool into `privileged-sidecar` on a host the operator intended to keep at `none`, which is exactly the kind of privilege escalation this project's trust model exists to prevent.

## Alternatives considered

- **Single privileged runner-with-Docker container** (Docker-in-the-runner, no sidecar): rejected — couples the runner and DinD lifecycles, and offers weaker isolation than a dedicated sidecar with its own crash/restart boundary.
- **Rootless DinD**: rejected for v1 — needs `uidmap` and cgroup v2 with systemd, the outer container still typically needs to run privileged, and it is impractical to guarantee on DSM/Unraid kernels today. Left as an open question for a future iteration.
- **Host socket passthrough**: rejected outright — direct violation of the "runner can never reach the host Docker socket" red line.
- **Shared static network/volumes across jobs** (in the style of the `werdnum` community provider): rejected — defeats the per-job isolation topology that is this project's core differentiator; see research.md §2 for the prior-art comparison.

## Consequences

- Privileged containers are required on NAS platforms in the default mode; this is documented honestly as a platform ceiling rather than hidden.
- Teardown must cover five distinct resource kinds (network, runner, DinD sidecar, socket volume, `dind-state` volume) in a defined order (ADR-004).
- Two DinD-adjacent images (the runner image from ADR-002 and the `docker:dind`-derived sidecar image) must be digest-pinned and tracked for updates.
- Job network concurrency is bounded by the host's Docker `default-address-pools` sizing, a capacity limit operators must plan for explicitly.

### Residual risk (host compromise under `privileged-sidecar`)

This ADR's isolation guarantees are real but bounded, and should be stated honestly rather than implied to be absolute. Per-job isolation (own network, own volumes, own DinD sidecar) protects **other jobs'** data from a compromised job, and the "never mount the host socket" rule keeps the **host** Docker socket structurally unreachable from inside any container, in every mode — that red line holds regardless of what follows below.

But in the default `privileged-sidecar` mode, a malicious workflow's job code holds `DOCKER_HOST` pointed at a **privileged** dind daemon that it fully controls. From there, that job can: run a nested privileged container of its own choosing, access host devices passed through to (or reachable from) the privileged dind container, potentially mount the host's root filesystem via a crafted nested container, and — from any of those — compromise the underlying host itself. None of this goes through the host's own Docker socket (the letter of red line 1 in this ADR's Context holds throughout), but it is nonetheless a real, direct host-takeover path, achieved via the privileged dind daemon this project itself provisions on the job's behalf.

**Privileged dind is not a containment boundary against malicious job code.** It contains accidents and well-behaved jobs; it does not contain an adversarial one. Concretely:

- For hosts that run genuinely untrusted job code (public-fork PR workflows, multi-tenant CI, or any workload where the pool operator does not fully trust every contributor), `sysbox-runc` mode or `none` mode are the honest choices — `sysbox-runc` narrows this residual risk considerably (no `Privileged=true` on the sidecar), and `none` removes it entirely by removing DinD altogether.
- Where neither is viable, `privileged-sidecar` should be restricted to pools running only trusted repositories/branches, as an explicit operator policy decision, not a provider-enforced one.
- NAS platforms without a Sysbox install path (Synology DSM, Unraid — research.md §3.C) inherently accept this residual-risk ceiling if they need DinD at all; this is a platform limitation to document plainly for those operators, not a gap to paper over.

## Open questions

- Whether `cgroupns=host` vs `cgroupns=private` is needed on older NAS kernels, and how to detect/configure it.
- Whether DinD readiness should also be probed provider-side (in addition to the entrypoint's own wait loop) for faster failure reporting.
- Whether a single-container Sysbox variant (no sidecar, Sysbox itself runs Docker inside the runner) is worth adding as a fourth mode.
- The pinning/versioning surface for `dockerd` inside the DinD sidecar image, independent of the runner image's own update cadence.
- Whether `allowed_dind_modes` should default to all three modes unconditionally, or whether a more conservative out-of-the-box default (e.g. excluding `privileged-sidecar`) is warranted given the residual host-compromise risk documented above — currently defaulting to all three on the theory that an operator who never touches the config should get the previously-documented default behavior, with the ceiling available for those who need to tighten it.
- A concrete, documented `default-address-pools` sizing recommendation for the NAS-first target hosts, so operators have a starting point rather than having to derive one from Docker's own defaults.
- Whether the provider should actively verify (and warn, or refuse to start) if the host Docker daemon appears to be listening on TCP, versus documenting the "no TCP-exposed daemon" recommendation only in the README.

See research.md §1–§2 for the upstream `GARM_POOL_EXTRASPECS` gap and the prior-art comparison this decision responds to, and §3.C for the Sysbox/DSM/Unraid platform-support gap underlying the residual-risk discussion above. See ADR-002 for the runner image the sidecar pairs with, ADR-004 for teardown ordering of the five per-allocation resource kinds, and ADR-005 for how `dind_mode`, `allowed_dind_modes`, and `storage_driver` are exposed in config and `extra_specs`.
