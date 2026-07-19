# Research: GARM External Provider on Docker

This document is the Phase-1 ground-truth research for building `github.com/TzuH-Hsu/garm-provider-docker`, a new open-source (Apache-2.0) external provider that lets [cloudbase/garm](https://github.com/cloudbase/garm) spin up ephemeral GitHub Actions runners as Docker containers. It synthesizes three independent research passes: the GARM external-provider wire contract, prior-art reference provider implementations, and runner-image/JIT/DinD/release-engineering mechanics.

**Versions read:** `cloudbase/garm` @ `main` (HEAD `e8b0848`, latest tag `v0.2.1`) and `cloudbase/garm-provider-common` @ `main` (HEAD `4f5d9bf`, latest tag `v0.1.9`). Everything described below is present in the latest release **except** `ProxyConfig` on `BootstrapInstance` and http(s)_proxy/no_proxy env passthrough, which are main-only and unreleased as of this research.

**Research date:** 2026-07-19.

---

## 1. The GARM external provider contract

*(Code-verified against `cloudbase/garm` and `cloudbase/garm-provider-common`.)*

### 1.A Environment variables GARM sets

Source: `garm/doc/external_provider.md`, `garm/runner/providers/v0.1.0/external.go`, `v0.1.1/external.go`, `garm-provider-common/execution/v0.1.0/execution.go`, `v0.1.1/execution.go`.

Always set: `GARM_COMMAND`, `GARM_PROVIDER_CONFIG_FILE`, `GARM_CONTROLLER_ID`, `GARM_INTERFACE_VERSION` (this last one is **not documented** in `external_provider.md`).

Per-command additions:
- `CreateInstance` → `GARM_POOL_ID` (v0.1.1 adds `GARM_POOL_EXTRASPECS` = base64 JSON of the pool/scale-set `ExtraSpecs`)
- `Delete`/`Get`/`Start`/`Stop`Instance → `GARM_INSTANCE_ID` (v0.1.1 adds `GARM_POOL_ID` + `GARM_POOL_EXTRASPECS`)
- `ListInstances` → `GARM_POOL_ID` (+ extraspecs in v0.1.1)
- `RemoveAllInstances` → no extra vars

`GARM_COMMAND` values (`execution/common/commands.go`): `CreateInstance`, `DeleteInstance`, `GetInstance`, `ListInstances`, `StartInstance`, `StopInstance`, `RemoveAllInstances`, `GetVersion`; v0.1.1-only: `GetSupportedInterfaceVersions`, `ValidatePoolInfo`, `GetConfigJSONSchema`, `GetExtraSpecsJSONSchema`.

The actual command strings are `"StartInstance"`/`"StopInstance"` — the doc says `Start`/`Stop`, which is stale.

**Exit codes:** `0` success, `30` NotFound (DeleteInstance treats this as success), `31` Duplicate, anything else = generic failure (`1`).

**Doc staleness:** `doc/external_provider.md` is missing `GARM_INTERFACE_VERSION`, `GARM_POOL_EXTRASPECS`, the real command name casing, and the entire v0.1.1 surface. Those four v0.1.1-only commands are defined in `garm-provider-common` but — verified by code search — are **never invoked by garm's controller today**.

**Env passthrough:** `config/external.go`'s `GetEnvironmentVariables()` only forwards vars listed in `[provider.external] environment_variables` (prefix match), plus automatic `http(s)_proxy`/`no_proxy` passthrough (main-only, unreleased).

### 1.B CreateInstance stdin payload

There are **two** `BootstrapInstance` structs. `garm/params/params.go`'s version is swagger-only. The struct actually marshaled to stdin is `garm-provider-common/params/params.go`'s `BootstrapInstance`, confirmed at call sites `garm/runner/pool/pool.go:1086` and `garm/workers/provider/instance_manager.go`. Fields (JSON tags):

- `name`
- `tools[]` (`RunnerApplicationDownload`: `os`, `architecture`, `download_url`, `filename`, `temp_download_token`, `sha256_checksum`)
- `repo_url` (entity ForgeURL — repo/org/enterprise URL, forge-agnostic)
- `callback-url`
- `metadata-url`
- `instance-token` (per-instance JWT, Bearer auth for callback/metadata)
- `ssh-keys`
- `extra_specs` (`json.RawMessage`, opaque provider-defined)
- `github-runner-group`
- `ca-cert-bundle` (`[]byte`)
- `arch` (`amd64|i386|arm64|arm`)
- `os_type` (`linux|windows|unknown`)
- `flavor`
- `image`
- `labels` (`[]string` — **only** populated when JIT is NOT used)
- `pool_id` (real pool UUID for pools; **synthetic** `"<scaleset-name>-<entityID>"` for scale sets)
- `user_data_options` (`{disable_updates_on_boot, extra_packages, enable_boot_debug}`)
- `jit_config_enabled` (bool)
- `proxy_config` (`{http_proxy, https_proxy, no_proxy}`) — **unreleased, main-only**

No field carries pre-generated JIT content, scale-set ID, or runner-group ID. For per-entity identification a provider has: `repo_url`, `labels` (non-JIT only), `extra_specs`.

### 1.C JIT delivery

`jit_config_enabled` is a boolean only. GARM generates the actual JIT config server-side (`r.ghcli.GetEntityJITConfig`, `garm/runner/pool/pool.go:962`) and stores it on the DB `Instance.JitConfiguration map[string]string` (`json:"-"`, never serialized to the provider).

The instance itself fetches it from the metadata service:
```
GET {metadata-url}/credentials/{fileName}
```
Bearer `{instance-token}` (`garm/auth/instance_middleware.go:187`; HS256 JWT; TTL = pool `RunnerTimeout` + 5-min `PoolReapTimeoutInterval`). Handled by `apiserver/controllers/metadata.go` → `runner.GetJITConfigFile` (`garm/runner/metadata.go:614`).

Full metadata router (`garm/apiserver/routers/routers.go:170-200`):
- `/metadata/runner-metadata`
- `/metadata/runner-registration-token` (classic non-JIT)
- `/metadata/credentials/{fileName}`
- `/metadata/system/service-name`
- `/metadata/systemd/unit-file`
- `/metadata/system/cert-bundle`
- `/metadata/tools/garm-agent[...]`
- `/metadata/install-script`

**JIT-vs-token decision** is NOT pool-vs-scale-set — for pools it's `!provider.DisableJITConfig() && forge != Gitea`; for scale sets JIT is **unconditionally forced on** (`workers/provider/instance_manager.go` hardcodes `JitConfigEnabled: true`, ignoring `disable_jit_config` — a gap).

The `cloudconfig` helpers in `garm-provider-common` are cloud-init-only and optional; a container provider can ignore the install-script/systemd endpoints entirely and itself call the credentials endpoint with the instance token. Since GARM's metadata service delivers individual credential files — `.runner`, `.credentials`, `.credentials_rsaparams` — via `GET {metadata-url}/credentials/{fileName}` rather than a single base64 blob, a provider fetching directly from GARM places these files in the runner's working directory and execs `run.sh` directly (no `--jitconfig` argument), or uses `config.sh --token` for non-JIT pools.

### 1.D Scale sets vs pools

The wire protocol is identical (same `BootstrapInstance`, same interface). Pools go through `garm/runner/pool/pool.go` (`PoolID` = real UUID; `GARM_POOL_EXTRASPECS` = pool's real `ExtraSpecs`). Scale sets go through `garm/workers/provider/instance_manager.go`: `PoolID = pseudoPoolID() = "<scaleSet.Name>-<entityID>"` (commented `"This is temporary..."`); `getProviderBaseParams` fills only `ControllerInfo`, leaving `PoolInfo` zero — so `GARM_POOL_EXTRASPECS` decodes to empty for scale-set instances even though **stdin `extra_specs` IS correctly populated** from `scaleSet.ExtraSpecs`.

**Consequence: a provider cannot tell pools and scale sets apart** from `GARM_POOL_EXTRASPECS` alone; it must rely on stdin `extra_specs`.

Timeouts: `PoolReapTimeoutInterval` = 5 min (`garm/runner/common/pool.go:27`); `DefaultRunnerBootstrapTimeout` = 20 min (`garm/util/appdefaults/appdefaults.go:26`).

### 1.E Stdout contract

`ProviderInstance` JSON:
```
{provider_id, name, os_type, os_name, os_version, os_arch,
 addresses[{address,type}], status, provider_fault}
```
`status ∈ {running, stopped, error, pending_delete, pending_force_delete, deleting, deleted, pending_create, creating, unknown}`.

Validation (`garm/runner/providers/common/common.go` `ValidateResult`): `provider_id` and `name` must be non-empty, `status` must be a known value — otherwise the whole call is treated as a `ProviderError`.

**Matching:** GARM passes `instance.ProviderID` as `GARM_INSTANCE_ID`, falling back to `instance.Name` if `ProviderID` is empty (confirmed in both pool and scale-set delete paths). **The provider must accept either** and must be able to look up an instance by the GARM Name label regardless of which identifier it's given.

### 1.F Status/timeouts and GARM's reaper

An instance MAY POST `/api/v1/callbacks/status` (`InstanceUpdateMessage{status, message, agent_id}`) and `/callbacks/system-info` with the instance JWT — this is called by the instance itself, not the provider; the provider's own stdout status is the only provider-side channel.

garm reaps every 5 min: instances older than `runner_bootstrap_timeout` (default 20 min) that aren't live on GitHub → `DeleteRunner` → provider `DeleteInstance`. **The provider needs no reaper of its own**; `DeleteInstance` must simply be idempotent (exit `30`/`0` on not-found).

### 1.G RemoveAllInstances and Stop/Start

The doc says these are not currently used by garm — verified: zero call sites in current code, and the CLI has no stop/start command. They must still exist to satisfy the interface. A Docker implementation can back them with `docker stop`/`start` and label-scoped delete-all (manual rescue tooling).

### 1.H Provider config file

Content is completely free-form — GARM only checks existence + absolute path. `config.toml` registration:
- `[[provider]]`: `name`, `provider_type="external"`, `description`, `disable_jit_config`
- `[provider.external]`: `interface_version` (empty → v0.1.0), `config_file`, `provider_executable` (absolute, exec bit), `environment_variables` (prefix allowlist), `exec_timeout_seconds` (0 = unbounded for non-create; create is bounded by `min(exec_timeout, bootstrap_timeout)`)

`doc/providers.md` documents `garm-cli provider list`, multi-provider pools with `--priority`, and that the official Docker image ships provider binaries at `/opt/garm/providers.d/`.

---

## 2. Prior art: reference provider implementations

*(`garm-provider-lxd` is AGPL-3.0-or-later — its patterns were studied for design ideas only; no code was or will be copied into this Apache-2.0 repository.)*

### 2.A mercedes-benz/garm-provider-k8s (MIT, v0.3.2, 2025-02, dependabot-active)

Standalone Go binary, `CGO_ENABLED=0`, calls `execution.GetEnvironment()` then `execution.Run(ctx, prov, env)`.

- **CreateInstance → Pod:** `podName = lowercase(bootstrap name)`, single container `"runner"`, `RestartPolicyNever`, `Resources` derived from `Flavor` → `ResourceRequirements` config map (`internal/spec/spec.go` `FlavorToResourceRequirements`), `emptyDir` volume `"runner"` at `/runner`; a user-supplied `podTemplate` is strategic-merge-patched in (`pkg/diff`, k8s `strategicpatch`) — this is how sidecars/volumes get injected. `ProviderID` = pod name.
- **Ownership labels:** `garm/instance-name`, `controllerID`, `poolID`, `flavor`, `os_type`, `os_arch`, `os_name`, `os_version`, `runner-group`; List/RemoveAll operate via LabelSelector on `controllerID`+`poolID` — no state store.
- **Status map:** `Running→running`, `Succeeded→stopped`, `Pending→pending_create`, `Failed→error`, `Unknown→unknown`; `CreateInstance` returns `running` immediately.
- **Image contract:** two reference images (`runner/summerwind/Dockerfile` FROM `summerwind/actions-runner:ubuntu-22.04`; `runner/upstream/Dockerfile` FROM `ghcr.io/actions/actions-runner:2.317.0`), byte-identical `entrypoint.sh`: requires `RUNNER_HOME`(`/runner`), `METADATA_URL`, `CALLBACK_URL`; copies runner assets from `RUNNER_ASSETS_DIR` into `RUNNER_HOME`.
  - JIT mode (`JIT_CONFIG_ENABLED=true`): fetches `.runner`/`.credentials`/`.credentials_rsaparams` from `${METADATA_URL}/credentials/...` with `Bearer ${BEARER_TOKEN}` (= InstanceToken), boots `run.sh` directly (no `config.sh`).
  - Non-JIT: fetches registration token from metadata, `config.sh --unattended --ephemeral --disableupdate --no-default-labels` with retry; backgrounds a `check_runner` loop POSTing `installing`/`idle`/`failed` + system-info to `CALLBACK_URL` (this is how GARM learns `agent_id`); execs `run.sh` in the foreground — container lifetime = runner lifetime.
  - **Env contract emitted:** `RUNNER_ORG`, `RUNNER_REPO`, `RUNNER_ENTERPRISE`, `RUNNER_GROUP`, `RUNNER_NAME`, `RUNNER_LABELS`, `RUNNER_NO_DEFAULT_LABELS=true`, `DISABLE_RUNNER_UPDATE=true`, `RUNNER_WORKDIR=/runner/_work/`, `GITHUB_URL`, `RUNNER_EPHEMERAL=true`, `RUNNER_TOKEN=dummy` (placeholder), `METADATA_URL`, `BEARER_TOKEN`, `CALLBACK_URL`, `JIT_CONFIG_ENABLED`.
- No runner update mechanism (`DISABLE_RUNNER_UPDATE`; version = image tag). No built-in DinD (only generic `podTemplate` merge; no privileged toggle in code).
- **Config** (koanf/YAML): `kubeConfigPath`, `runnerNamespace`, `podTemplate`, `flavors` map. `extra_specs`: only `OSName`/`OSVersion` read, no schema validation.
- `DeleteInstance` idempotent (NotFound = success); no internal GC loop — GARM's reconciliation is the authority; `RemoveAllInstances` best-effort by label. No scale-set-specific code.
- **Release:** goreleaser, linux amd64+arm64 static, plus Black Duck FOSS scan artifacts.

### 2.B werdnum/garm-provider-docker (no license, unreleased)

Created 2026-01-11, last push 2026-01-13 — a single ~60-hour burst of 13 commits, all Claude-co-authored. No stars, no LICENSE file, no releases. This is early, unfinished exploratory work rather than a maintained project: it still contains unfinished LLM-generated comments left in code (e.g. `"Wait, checking the exact signature for v24.0.7..."` in `Stop()`), tests cover only Create/Delete/List (3 of the 8 required commands), and the README claims TOML+YAML config support but only YAML is actually wired up. It is useful as a proof of concept and a source of design ideas, not as code to build on directly (also license-incompatible: no LICENSE at all, so nothing is reusable regardless).

Structure notes worth keeping:
- `internal/provider` wraps the Docker SDK behind a small `DockerClient` interface (`ImagePull`, `ImageInspectWithRaw`, `ContainerCreate/Start/Remove/Inspect/List/Stop`) for mockability; client built from config `DockerHost` (default `unix:///var/run/docker.sock`) with API version negotiation.
- `CreateInstance`: conditional pull (`AlwaysPull` or missing), optional registry auth from a dockercfg-style file, `ContainerCreate` with `{Image, Env, Labels}` + `HostConfig{Runtime, NetworkMode, Privileged, Binds}`, start, then inspect for IPs.
- **Labels:** `garm.runner/instance-name`, `controller-id`, `pool-id`, `flavor`, `os-type`, `os-arch`; List by label filter; `ProviderID` = container ID; `GetInstance` passes the instance string straight to `ContainerInspect` (accepts either ID or name — matches the GARM matching requirement in §1.E).
- `spec.go` `GetRunnerEnvs` is near-identical to the k8s provider's (same env contract, same comments — clearly copy-adapted).
- **No runner image shipped** — it consumes images that already understand the k8s-provider-style env contract.
- **DinD:** config `Runtime` (default `sysbox-runc`) passed to `HostConfig.Runtime` — the only Sysbox integration point; a separate, mutually-exclusive `Privileged` flag → `Privileged=true` + `CgroupnsMode=host` + an anonymous volume at `/var/lib/docker`. **No per-job network or volume creation anywhere** (single global network string, static global binds; no `NetworkCreate`/`VolumeCreate` calls in the codebase).
- `DeleteInstance`: `ContainerRemove(Force, RemoveVolumes per config)`, not-found = success; `RemoveAllInstances` by controller label, best-effort.

### 2.C cloudbase/garm-provider-lxd (AGPL-3.0-or-later — patterns studied only; v0.1.5, 2026-04, active)

**License note:** AGPL-3.0-or-later is copyleft and incompatible with re-use in this Apache-2.0 project. Nothing from this repository's source was copied; only architectural patterns (config shape, schema-validated extra_specs, dead-man delete timeout) informed this project's design.

- TOML config: `unix_socket_path`, `project_name`, `include_default_profile`, `url` + mTLS certs, `secure_boot`, `instance_type`, `[image_remotes.*]` simplestreams.
- Flavor = LXD profile (existence-checked at create time).
- `extra_specs`: a struct with `jsonschema` tags validated via `xeipuuv/gojsonschema`; the README publishes the full schema (`extra_packages`, `disable_updates`, `enable_boot_debug` + `cloudconfig.CloudConfigSpec`: `runner_install_template`, `extra_context`, `pre_install_scripts`) — a good model for a documented, schema-validated `extra_specs` contract.
- Layout: flat root `main.go` + `config/` + `provider/`; pins the older `execution/v0.1.0` (implements `execution.ExternalProvider`, `executionEnv.Run(ctx, prov)`); version via `ldflags -X provider.Version=$(git describe --tags)`.
- `CreateInstance`: cloud-init user-data via `cloudconfig.GetCloudConfig` (VM model, not directly applicable to containers); ownership tracked via LXD instance config keys (`controller-id`/`pool-id`), matched by an `ExpandedConfig` scan.
- `DeleteInstance`: graceful stop, swallows NotFound/already-stopped, deletes with a 60-second dead-man goroutine as a safety net.
- **Release:** no goreleaser — a Dockerfile (Alpine musl cross toolchain) + Makefile `build-static` + `scripts/make-release.sh` → static linux amd64/arm64 + windows amd64 binaries + `.sha256` files; no SBOM/signing; CI is only `go-tests.yml`.

### 2.D Other providers surveyed

- `cloudbase/garm-provider-incus` (official, active, LXD-analog).
- Official cloud VM providers: openstack, aws, azure, gcp, oci, equinix — all active, following the cloud-init pattern.
- Community: `flatcar/linode`, `imtf-group/hetzner`, `nexthop-ai/cloudstack`, `nikolai-in/proxmox`; `AndyA/garm-provider-pm2` (process-based, stale since 2023).

**Market gap confirmed:** no other Docker-native GARM provider exists besides werdnum's unfinished proof-of-concept. None of the surveyed providers has scale-set-specific code — the wire-protocol identity between pools and scale sets (§1.D) makes them compatible by construction, and this new provider inherits that same compatibility for free.

---

## 3. Runner image, JIT mechanics, DinD patterns, release engineering

*(Source-verified against `actions/runner` @ main, 2026, plus image/tooling repos as cited.)*

### 3.A actions/runner JIT mechanics (source-verified)

`run.sh --jitconfig <base64>` → `Runner.Listener` parses via `CommandSettings.GetJitConfig` (`src/Runner.Listener/CommandSettings.cs`); `Runner.cs::ExecuteCommand` decodes the base64 to a dict and writes each entry to disk (`.runner`, `.credentials`, `.credentials_rsaparams`) — `config.sh` is fully skipped. The decoded `RunnerSettings.Ephemeral` forces `runOnce`; after one job the runner deletes its local config and exits (`Runner.cs` ~lines 234-332, 694-704, 877-880).

**Exit codes** (`Constants.cs` `ReturnCode`): `0` Success, `1` TerminatedError, `2` RetryableError, `3`/`4` updating, `5` SessionConflict, `6` ConfigRefreshed, `7` VersionDeprecated. The `run.sh` wrapper (`src/Misc/layoutroot/run.sh`) restarts only on exit `2`; everything else (including `0`) exits the wrapper with no restart — so an ephemeral container dies after exactly one job.

JIT config is issued by `POST /orgs|repos|enterprises/.../actions/runners/generate-jitconfig`, valid for ~60 minutes; it can expire mid-queue (tracked upstream: `actions/runner#4248`).

The `externals/` directory is Node runtime builds only (Node 20.20.2 + 24.18.0 per OS/arch, `src/Misc/externals.sh`) plus Windows `vswhere`; `NodeScriptActionHandler` invokes `externals/<ver>/bin/node`. Together with `bin/`, this is ~380MB across 9,000+ files (per a `myoung34` entrypoint comment). Known upstream issues: self-update can clobber symlinks (`actions/runner#2094`); permissions issues (`actions/runner#2486`).

`RUNNER_TOOL_CACHE` (alias `AGENT_TOOLSDIRECTORY`) is checked/populated by `setup-*` actions before downloading; the hosted default is `/opt/hostedtoolcache`; both the ARC image and myoung34's image set it. There is a history of `setup-python` quirks against this cache (`actions/setup-python#914`, `#824`) — worth a smoke test per action used.

### 3.B Reference runner images

**myoung34/docker-github-actions-runner:** NO native JIT support (`entrypoint.sh` only calls `config.sh --token` via `configure_runner`; there is no jitconfig path) — **a custom entrypoint is required** to add the pattern `exec ./run.sh --jitconfig "${ACTIONS_RUNNER_INPUT_JITCONFIG}"`. Deploy matrix `{jammy,focal,noble}×{amd64,arm64}`, nightly + tagged builds pushed to Docker Hub `myoung34/github-runner` (GHCR mirroring not verified in `deploy.yml`). Contents (`build/config.json`, `install_base.sh`): docker-cli + docker engine, git+git-lfs, gh, aws-cli, powershell, yq, container-tools, nodejs, python3+pip, curl/jq/gnupg/tar/unzip/zip/sudo/gosu/dumb-init; users `runner`(1001)/`docker` groups; `RUNNER_VERSION` via `ARG GH_RUNNER_VERSION` (2.335.1 at research date) into `/actions-runner`; `AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache` pre-created. If `RUN_AS_ROOT!=true`, the entrypoint groupmods the internal docker group to match the mounted socket GID then `gosu runner`. A deregistration trap on EXIT/INT/QUIT/TERM runs `config.sh remove`.

**ghcr.io/actions/actions-runner** (official): minimal `dotnet/runtime-deps:8.0-noble` base, runner tarball + `runner-container-hooks` + docker CLI + buildx, **no entrypoint** — the consumer supplies the command `/home/runner/run.sh` + `ACTIONS_RUNNER_INPUT_JITCONFIG`. Known to lack common tools per a comment in `gha-runner-scale-set`'s `values.yaml`.

**summerwind/actions-runner:** legacy, deprecated.

### 3.C DinD patterns

`docker:dind`: TLS is on by default (`DOCKER_TLS_CERTDIR=/certs` → tcp 2376 tlsverify); disable via `DOCKER_TLS_CERTDIR=""` or an explicit `dockerd` command; `--privileged` is required; a unix-socket-in-shared-volume is the standard CI sidecar pattern.

**Storage:** `overlay2` recommended; nested overlay issues surface as `"failed to mount overlay: invalid argument"`; `fuse-overlayfs` is unreliable inside dind (`containers/fuse-overlayfs#375`, `moby/moby#42171`). Synology DSM is btrfs-backed; forcing `overlay2` via `daemon.json` reportedly does not switch the host driver. Practical takeaway: give dind its own `/var/lib/docker` volume with an explicit `--storage-driver overlay2` (vfs as fallback) rather than trusting host autodetection. *(Flagged: this DSM-specific inference is drawn from adjacent storage-driver evidence — no direct DSM+dind bug report was found; see §5.)*

**Rootless dind:** needs `uidmap`, `/etc/subuid`+`subgid`, kernel ≥4.18 (5.11+ recommended), cgroup v2 + systemd for limits; the `dind-rootless` variant **still requires `--privileged` on the outer container**; cgroup v2 limits are unreliable (`moby/moby#42910`); DSM/Unraid are impractical targets (Unraid 6.12 has cgroup v2 regressions and no systemd — this delegation inference is flagged unverified in §5) → **out of initial scope**.

**Sysbox:** Docker acquired Nestybox in 2022-05; Sysbox remains OSS and active (~0.7.x era, CVE fixes, k8s 1.33–1.35 support — release dating is search-indexed, not gh-api-verified). Configuration: `daemon.json` `runtimes {"sysbox-runc": {"path": "/usr/bin/sysbox-runc"}}` + `--runtime=sysbox-runc`. Supported distros: Ubuntu focal/jammy/noble, Debian bullseye/buster (kernel ≥5.3; ≥5.12 drops the shiftfs requirement); Fedora/Rocky/Alma/CentOS-Stream/AmazonLinux require a source build; RHEL/Flatcar require Sysbox-EE. **Not in the supported-distro list: Synology DSM, Unraid** — there is no supported install path for either (a direct inference from the allowlist, not a documented exclusion). Raspberry Pi on standard distros is fine (an ARM Learning Path install guide exists).

### 3.D ARC (actions-runner-controller) dind mode — verified reference architecture

From `gha-runner-scale-set`'s `values.yaml`:
- `initContainer init-dind-externals` (image `actions-runner`, `cp -r /home/runner/externals/. →` shared `emptyDir`).
- `dind` container: `docker:dind`, privileged, `args: ["dockerd", "--host=unix:///var/run/docker.sock", "--group=$(DOCKER_GROUP_GID)"]`, `startupProbe: docker info` (24×5s), `restartPolicy: Always`, mounts `work` + `dind-sock`(`/var/run`) + `dind-externals`(`/home/runner/externals`).
- `runner` container: `DOCKER_HOST=unix:///var/run/docker.sock`, `RUNNER_WAIT_FOR_DOCKER_IN_SECONDS=120`, mounts `work` + `dind-sock`.
- All volumes are `emptyDir`.

Non-k8s equivalent: a shell poll (legacy `runner/startup.sh` timeout loop until `docker ps` succeeds; `WAIT_FOR_DOCKER_SECONDS` default 120).

**JIT injection convention:** env var `ACTIONS_RUNNER_INPUT_JITCONFIG` (ARC `constants.go` `EnvVarRunnerJITConfig`) is the de-facto standard across the ecosystem (`llvm-zorg`, `cncf/automation`, etc.) — this is the pattern to standardize on for the new provider's runner image.

### 3.E Release engineering

The cloudbase family uses **no goreleaser anywhere** — a Makefile `build-static` target running inside a throwaway Docker container + `scripts/make-release.sh` → per-arch `.tgz` + `.sha256` (linux amd64/arm64, windows amd64); provider repos have only a `go-tests.yml` CI workflow, with release cut manually. The garm main repo's `build-and-push.yml` does `docker buildx --provenance=false`, multi-arch amd64+arm64, OCI labels — **no SBOM**.

Best-practice reference for release engineering beyond the cloudbase baseline: `docker/build-push-action` with `provenance: mode=max` + `sbom: true` (per Docker's official docs); `argo-cd`'s `image-reuse.yaml` is an alternative pattern (provenance/sbom disabled, cosign keyless signing instead). Known issue: SBOM attestation on multi-arch manifest lists is awkward (`actions/attest-sbom#60`, `docker/build-push-action#1260`) — per-platform attestations or cosign are the workarounds. `actions/attest-build-provenance` v4 is now a thin wrapper over `actions/attest`.

---

## 4. Key design inputs derived from research

- **Use stdin `extra_specs`, not `GARM_POOL_EXTRASPECS`.** The env var is empty for scale-set instances (§1.D) — the stdin `BootstrapInstance.extra_specs` field is populated correctly for both pools and scale sets and is the only reliable source.
- **Key any internal cache/state on the normalized `repo_url`, not `pool_id`.** `pool_id` is a real UUID for pools but a synthetic, non-stable string for scale sets (§1.D) — `repo_url` is the one identity field both entity types provide consistently.
- **The instance-id resolver must accept either `ProviderID` or `Name`.** GARM falls back to `Name` when `ProviderID` is empty (§1.E); `Get`/`Delete`/`Start`/`Stop`Instance must look up containers by either.
- **`DeleteInstance` must be idempotent and exit `30` on not-found** (treated as success by GARM) — no bespoke reconciliation loop is needed on the provider side.
- **GARM's own 20-minute reaper (5-min poll interval) owns timeout enforcement** (§1.F) — the provider should not implement its own runner-timeout logic.
- **Adopt the ARC "socket-in-shared-volume" + externals-init-copy pattern as the DinD/externals blueprint** (§3.D): a privileged `docker:dind` sidecar-equivalent sharing a Unix socket volume with the runner container, and an init step that copies the runner's `externals/` payload into a shared volume rather than re-downloading it.
- **Runner JIT configuration approaches:** `actions/runner` supports two JIT delivery patterns: `run.sh --jitconfig <base64-blob>` (used by ARC via `ACTIONS_RUNNER_INPUT_JITCONFIG`, §3.D) and direct `.runner`/`.credentials`/`.credentials_rsaparams` file configuration (§3.A). GARM's metadata service specifically serves individual credential files via `GET {metadata-url}/credentials/{fileName}` (§1.C, §2.A), not a single blob, so a container provider directly fetching from GARM uses the file-based pattern, placing the three fetched files in the runner's working directory and executing `run.sh` directly — the reference k8s provider follows this same file-based approach.
- **Sysbox is not installable on Synology DSM or Unraid** (§3.C) — the supported-distro allowlist excludes both, so any Sysbox-based rootless/hardened DinD story is out of scope for those targets; plain privileged DinD (or no DinD) is the realistic path there.
- **Full SBOM + provenance attestation would exceed the current cloudbase-family baseline** (§3.E) — none of the cloudbase provider repos ship SBOM or provenance today; doing so here is worthwhile as a differentiator but is an intentional scope decision, not table stakes to match upstream.

---

## 5. Unverified items

Honestly flagged gaps carried over from the source reports — none of these blocked the design conclusions above, but they should be validated empirically before being relied upon:

- **myoung34 GHCR mirroring:** whether `myoung34/docker-github-actions-runner` images are also pushed to GHCR (not just Docker Hub) was not confirmed in `deploy.yml`.
- **Exact Sysbox 2026 release timestamps:** the "~0.7.x era" dating is search-indexed, not verified against the GitHub releases API.
- **Direct DSM + dind failure report:** the Synology DSM overlay2/storage-driver guidance (§3.C) is inferred from adjacent evidence (btrfs backing, `daemon.json` driver-override reports); no direct "dind fails on DSM" issue was found.
- **Unraid systemd/rootless delegation:** the claim that Unraid lacks the cgroup v2 + systemd delegation needed for rootless dind is an inference from Unraid 6.12's known cgroup v2 regressions and absence of systemd, not a directly cited compatibility statement.
