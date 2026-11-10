# Config and `extra_specs` reference

This document is the operator- and pool-admin-facing reference for
`garm-provider-docker`'s configuration and for the pool `extra_specs` contract
([ADR-005](adr/ADR-005-config-and-extra-specs-schema.md)). Both contracts are also
published as machine-readable JSON Schemas that the binary can emit (see below).
This page and the schemas are maintained by hand next to the code. If they
disagree with the binary's behaviour, the code wins; please report it.

## Trust tiers (why there are two contracts)

Three inputs feed every provider decision, in strict trust order (ADR-001/ADR-005):

1. **Provider config** (TOML), set by the host **operator**, the most trusted tier.
2. **Pool `extra_specs`** (JSON), set by a **GARM admin**, a narrower and more dynamic tier. Read from the stdin `BootstrapInstance.extra_specs` field, **never** `GARM_POOL_EXTRASPECS` (which GARM v0.2.1 sets to `base64("null")` for scale-set instances on every command except `CreateInstance`).
3. **The workflow payload**, controlled by anyone who can open a pull request; **never** consulted for a privileged decision.

`extra_specs` can select *within* what the operator config allows; it can never
escalate beyond it.

## Emitting the schemas from the binary (v0.1.1)

These commands are v0.1.1-only, but GARM v0.2.1 never invokes them; they are
for operators and tooling. They work regardless of the `interface_version`
GARM is configured with, because you set `GARM_INTERFACE_VERSION=v0.1.1` on
the command line yourself. On GARM <= v0.2.1 register the provider with
`interface_version = "v0.1.0"` (GARM v0.2.1's v0.1.1 `ListInstances` wrapper
misreports every successful list as an error; fixed upstream by
[cloudbase/garm#878](https://github.com/cloudbase/garm/pull/878), unreleased
when checked on 2026-10-11; see the README's "Register with GARM" section).

```sh
# The extra_specs JSON Schema (draft-07)
GARM_INTERFACE_VERSION=v0.1.1 GARM_COMMAND=GetExtraSpecsJSONSchema \
  GARM_CONTROLLER_ID=x GARM_PROVIDER_CONFIG_FILE=/path/config.toml \
  ./garm-provider-docker

# The provider-config JSON Schema
GARM_INTERFACE_VERSION=v0.1.1 GARM_COMMAND=GetConfigJSONSchema \
  GARM_CONTROLLER_ID=x GARM_PROVIDER_CONFIG_FILE=/path/config.toml \
  ./garm-provider-docker

# Validate a pool's extra_specs early: exits non-zero with a descriptive error if it
# violates the schema, the allowed_dind_modes ceiling, a memory ceiling, the extra_env
# allowlist, or a hard-reserved env name.
GARM_INTERFACE_VERSION=v0.1.1 GARM_COMMAND=ValidatePoolInfo \
  GARM_CONTROLLER_ID=x GARM_PROVIDER_CONFIG_FILE=/path/config.toml \
  GARM_POOL_EXTRASPECS='{"dind_mode":"privileged-sidecar"}' \
  ./garm-provider-docker
```

## `extra_specs` allowlist (what a pool admin may set)

Only these keys are accepted. **Any other key is rejected** (`additionalProperties:
false`), which structurally keeps a raw image reference, `docker_host`,
and the `privileged` flag out of this channel: they have no field at all.

| Key | Type | Bound |
|---|---|---|
| `flavor` | string | Must name a `[flavors.<name>]` entry in the provider config. The **only** channel that varies the runner image ([ADR-002](adr/ADR-002-runner-image-and-jit-delivery.md)). `extra_specs` never accepts a raw image reference. |
| `dind_mode` | `none` \| `privileged-sidecar` \| `sysbox-runc` | Bounded by the operator's `allowed_dind_modes` **ceiling**: a mode outside that list is rejected. |
| `runner_memory` | byte-size string (`"4GiB"`, `"512MiB"`, `"2GB"`) | Bounded by the effective configured `runner_memory`. Must be **positive** (a zero value in any form is rejected). **Rejected if it exceeds the ceiling** (reject, not clamp). |
| `dind_memory` | byte-size string | Bounded by the effective configured `dind_memory`, same rules (positive only, rejected and not clamped). |
| `storage_driver` | `overlay2` \| `vfs` | Defaults to the configured `storage_driver`. |
| `job_hooks` | `fail` \| `warn` \| `off` | What the runner image's job-started hook does when a check finds damage. Overrides the configured `job_hooks` for this pool. No ceiling: turning a check off grants no privilege. See [Job hooks](#job-hooks-job_hooks). |
| `runner_labels` | array of strings | Appended to the runner's labels. **Non-JIT mode only.** With JIT, GARM bakes the label set server-side, so these are inert. |
| `extra_env` | object of string→string | Extra runner-container env. Governed by an **operator allowlist** and a **hard-reserved** set (see below). |

### GARM-reserved keys (accepted and ignored)

GARM itself writes some keys into `extra_specs`. Only these are
removed before the schema check, so they are accepted and have **no effect**
on this provider:

| Key | Why it appears | Effect here |
|---|---|---|
| `runner_install_template` | GARM v0.2.0+ injects its rendered userdata wrapper (base64) into every `CreateInstance` payload unless the admin set the key; GARM also *requires* an admin to set it on a pool/scale set with no install template. | None. It drives GARM's userdata install flow, which this provider does not use (prebuilt runner image, [ADR-002](adr/ADR-002-runner-image-and-jit-delivery.md)). Must be a JSON string. |

Every other unknown key is still rejected, including next to a reserved key.
GARM's other install-flow keys (`extra_context`, `pre_install_scripts`,
`enable_boot_debug`) are never injected by GARM and have no effect here, so
they are rejected rather than silently ignored.

### Ceilings, the `extra_env` allowlist and the hard-reserved set

- **`allowed_dind_modes` ceiling, fail-closed by default.** The operator's `allowed_dind_modes` is the final word on `dind_mode`, and it **defaults to `["none"]`**: DinD is unavailable on a host whose config never mentions it, so no pool's `extra_specs.dind_mode` can obtain a privileged sidecar by omission. To enable DinD you must widen the ceiling *and* set `dind_image`, for example `allowed_dind_modes = ["none", "privileged-sidecar"]`. Read ADR-001's "Residual risk" section first: a privileged sidecar is root-equivalent on the host for the jobs that run in it. `dind_mode` itself must be a member of this list, so widening the ceiling is also what makes a non-`none` `dind_mode` load at all. No pool's `extra_specs.dind_mode` can escalate past it.
- **A DinD mode with no `dind_image` is rejected before any Docker work.** Config load already requires `dind_image` when the *config's own* `dind_mode` is non-`none`, but that cannot see a pool's `extra_specs`. If a pool selects a DinD mode within a widened ceiling on a host that has no `dind_image` set, `CreateInstance` rejects it up front, before the orphan sweep, the claim network, the volumes or the credential fetch, rather than failing at the sidecar image pull with a partial allocation to roll back.
- **Registry mirrors for the sidecar are provider config only.** Every DinD job starts with an empty image store. The operator sets `registry_mirrors` and `insecure_registries` in the provider config (see [Registry mirrors for the sidecar](#registry-mirrors-for-the-sidecar)); no `extra_specs` key carries them, so a pool admin cannot redirect a sidecar's pulls.
- **Memory is positive only and rejected, not clamped.** A `runner_memory`/`dind_memory` over the configured (or flavor-resolved) ceiling is rejected with a clear error; it is never silently reduced. A request at or below the ceiling is accepted, and an unset ceiling (unlimited) accepts any finite request. A **zero** value in any representation (`"0"`, `"0GiB"`, `"0 B"`) is rejected, because Docker treats a 0 memory limit as unset (unlimited), so accepting it would remove the operator's ceiling.
- **`extra_env` is fail-closed through an operator allowlist.** The operator lists the variable names a pool may set in `[extra_specs].allowed_env`, which is **empty** by default. A name not on the list is **rejected**, so by default a pool can inject no extra environment at all.
- **`extra_env` names must match `^[A-Za-z_][A-Za-z0-9_]*$`.** A key containing `=`, whitespace, non-ASCII characters or a leading digit is rejected. Otherwise the key `JIT_CONFIG_ENABLED=false` would reach Docker as `JIT_CONFIG_ENABLED=false=<value>` and set a reserved variable.
- **Hard-reserved `extra_env` names are always rejected, even if allowlisted (case-insensitive):**
  - prefixes `RUNNER_*`, `DOCKER_*`, `ACTIONS_RUNNER_INPUT_*`, `GARM_*`, `LD_*`
  - exact `JIT_CONFIG_ENABLED`, `GITHUB_URL`, `RUN_AS_ROOT`, `WAIT_FOR_DOCKER_SECONDS`, `BASH_ENV`, `ENV`, `IFS`, `PATH`, `NODE_OPTIONS`

  The provider, the runner-image entrypoint or the interpreter relies on these names. `RUNNER_EPHEMERAL=false` or `DOCKER_HOST=...` would break the single-job teardown model or the DinD socket guarantees. `RUN_AS_ROOT=true` would run the runner and its job as **root**. The entrypoint reads `GARM_CRED_WAIT_SECONDS` and `WAIT_FOR_DOCKER_SECONDS` into Bash arithmetic, where a crafted value is command substitution as root. `BASH_ENV`, `LD_*` and `PATH` are shell and loader hijack vectors. A non-reserved, allowlisted name that collides with another provider-injected variable is dropped in favour of the provider's value: **provider-injected environment always wins**.
- **`GARM_JOB_HOOKS` is reserved like every other `GARM_` name.** The runner image's job-started hook reads it, but the provider sets it from the typed `job_hooks` setting (next section), so `extra_env` cannot, even if you allowlist the name.

### Job hooks (`job_hooks`)

A job can leave a persistent cache, such as the pnpm store, in a state that makes later jobs of the same repository pass without doing their work. The runner images run a job-started hook that checks for this before the first step of every job (today: a 0-byte `pnpm` binary in a repository's pnpm store, see [`package-store-installs.md`](package-store-installs.md#pnpm-switching-its-own-version-pnpm-11-before-1122)). `job_hooks` chooses what the hook does when a check finds damage:

| Value | Effect |
|---|---|
| `fail` (default) | The job fails before its first step, with an annotation that names the cause and the fix. |
| `warn` | The annotation is printed and the job runs. |
| `off` | The checks do not run. |

Set the default for every pool with the top-level key `job_hooks` in the provider config, and override it for one pool with `{"job_hooks": "warn"}` in its `extra_specs`. The pool's value wins, then the config's, then `fail`. An unknown value is a config error at load and an `extra_specs` rejection. There is no `allowed_*` ceiling, because a pool that turns a check off gains no privilege and only loses a safety net. The provider always sets the resolved value in the runner container as `GARM_JOB_HOOKS`. Runner images that do not carry the hook ignore it, and a custom image that sets `GARM_JOB_HOOKS` itself gets the same behaviour ([`runner-images/README.md`](../runner-images/README.md#job-started-hook)).

### Never settable from `extra_specs` (structurally)

`docker_host`; any host-socket mount (no such field exists); the `privileged`
flag (derived from `dind_mode` only, capped by `allowed_dind_modes`); the
`garm.docker/*` managed-label set ([ADR-004](adr/ADR-004-teardown-and-orphan-cleanup.md)); a raw image reference; and the
reserved runner-contract env names above.

## Published `extra_specs` JSON Schema (draft-07)

This is the exact schema the binary validates against (`internal/extraspecs/schema.json`):

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "$id": "https://github.com/TzuH-Hsu/garm-provider-docker/schemas/extra_specs.json",
  "title": "garm-provider-docker extra_specs",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "flavor": { "type": "string", "minLength": 1 },
    "dind_mode": { "type": "string", "enum": ["none", "privileged-sidecar", "sysbox-runc"] },
    "runner_memory": { "type": "string", "pattern": "^[1-9][0-9]*\\s*(?i:b|kb|mb|gb|kib|mib|gib)?$" },
    "dind_memory": { "type": "string", "pattern": "^[1-9][0-9]*\\s*(?i:b|kb|mb|gb|kib|mib|gib)?$" },
    "storage_driver": { "type": "string", "enum": ["overlay2", "vfs"] },
    "job_hooks": { "type": "string", "enum": ["fail", "warn", "off"] },
    "runner_labels": { "type": "array", "items": { "type": "string", "minLength": 1 } },
    "extra_env": {
      "type": "object",
      "additionalProperties": { "type": "string" },
      "propertyNames": { "pattern": "^[A-Za-z_][A-Za-z0-9_]*$" }
    }
  }
}
```

The memory patterns are positive-only (`^[1-9][0-9]*…`, no leading-zero value), and
`extra_env.propertyNames.pattern` enforces the env-name charset; the operator
allowlist and hard-reserved set are enforced in Go (they cannot be expressed as a
case-insensitive prefix denylist in JSON Schema under RE2).

Field descriptions are carried inline in the emitted schema
(`GetExtraSpecsJSONSchema`); they are elided here for brevity.

### Examples

Select a bigger flavor and a DinD mode the operator allows:

```json
{ "flavor": "large", "dind_mode": "privileged-sidecar", "runner_memory": "12GiB" }
```

Add extra labels and a couple of environment variables (each name must be on the
operator's `[extra_specs].allowed_env` allowlist):

```json
{ "runner_labels": ["gpu", "cuda"], "extra_env": { "MY_CI_FLAG": "on", "TZ": "UTC" } }
```

Rejected examples (each fails closed, before any container is created):

```jsonc
{ "image": "attacker/evil:latest" }              // unknown key: a raw image is not a channel
{ "dind_mode": "privileged-sidecar" }            // if allowed_dind_modes = ["none"]
{ "runner_memory": "64GiB" }                     // if the ceiling is 8GiB
{ "runner_memory": "0GiB" }                      // zero is not a valid limit
{ "extra_env": { "RUNNER_EPHEMERAL": "false" } } // hard-reserved name
{ "extra_env": { "RUN_AS_ROOT": "true" } }       // hard-reserved: would run the job as root
{ "extra_env": { "MY_CI_FLAG": "on" } }          // if MY_CI_FLAG is not on [extra_specs].allowed_env
{ "extra_env": { "JIT_CONFIG_ENABLED=false": "x" } } // '=' in the key
```

## Provider config (TOML)

[`examples/config.full.toml`](../examples/config.full.toml) lists every key with its default, and
[`examples/config.minimal.toml`](../examples/config.minimal.toml) is the smallest valid file (only
`runner_image`). A test loads both. The loader ignores unknown keys, so a misspelled key is silently ignored
in a real config. `GetConfigJSONSchema` emits the machine-readable schema.

| Key | Default | Notes |
|---|---|---|
| `docker_host` | `unix:///var/run/docker.sock` | The provider's own connection to the Docker host. Not settable from `extra_specs`. |
| `runner_image` | required | Must be pinned by a `sha256` digest. |
| `allow_unpinned_runner_image`, `allow_unpinned_dind_image` | `false` | Development only. Each relaxes the digest rule for one image. |
| `dind_mode` | `none` | `none`, `privileged-sidecar` or `sysbox-runc`. Must be in `allowed_dind_modes`. |
| `allowed_dind_modes` | `["none"]` | The ceiling for pools. See above. |
| `dind_image` | unset | Required when `dind_mode` is not `none`, and checked at load whenever it is set. Digest-pinned. |
| `storage_driver` | `overlay2` | `overlay2` or `vfs`. |
| `job_hooks` | `fail` | `fail`, `warn` or `off`. What the runner image's job-started hook does when a check finds damage. A pool can override it with `extra_specs.job_hooks`. See [Job hooks](#job-hooks-job_hooks). |
| `registry_mirrors`, `insecure_registries` | empty | See the next section. |
| `[resources]` `runner_memory`, `dind_memory` | unset (unlimited) | Byte sizes such as `8GiB` or `512MiB`: a whole number with an optional `B`, `KB`, `MB`, `GB` (decimal) or `KiB`, `MiB`, `GiB` (binary) suffix, in any case. `8GB` and `8GiB` are different sizes, and fractions such as `1.5GiB` are rejected. They are the ceilings for `extra_specs`. There are no CPU or PID limits. |
| `[network]` `internal` | `false` | Docker's `internal` flag on each job network. `true` gives a job no egress at all. |
| `[network]` `enable_job_network` | `true` | Reserved. The network is always created, and a warning is logged if this is `false`. |
| `[cache]` `enabled` | `true` | Turns the persistent caches off, including the externals volume. JavaScript actions then cannot run in container jobs in DinD modes. |
| `[cache]` `generation`, `pnpm_major` | `"1"`, `"9"` | Salts in the volume names. A new value starts a new volume. `pnpm_major` must match the pnpm in the runner image. |
| `[cache]` `toolcache_path`, `pnpm_store_path` | `/opt/hostedtoolcache`, `/opt/pnpm-store` | Absolute, canonical, different from each other, and not under a reserved runner mount. |
| `[cache]` `allow_org_shared` | `false` | Allows organization and enterprise pools persistent caches. It treats the whole organization as one trust domain. |
| `[cache]` `stale_cache_eviction_days` | `30` | Age after which a cache volume is logged as stale. Nothing is deleted. `0` turns the age check off. |
| `[cache]` `diagnostic_log_retention_days` | `7` | Runner diagnostic logs older than this are deleted by the provider. `0` deletes every file as soon as a prune round runs. |
| `[extra_specs]` `allowed_env` | empty | See above. |
| `[status_report]` `timeout_seconds` | `90` | `0` to `600`. |
| `[logging]` `file` | unset | See below. |
| `[flavors.<name>]` | none | `runner_image`, `runner_memory`, `dind_memory`. Unset fields use the top-level value. |

### Registry mirrors for the sidecar

Two optional top-level keys, next to `dind_image` and `storage_driver`, configure
the registry settings of every DinD sidecar's `dockerd`. They are emitted as
typed flags appended after the fixed ones (`--host`, `--storage-driver`,
`--group`), in the configured order: one `--registry-mirror=<url>` per mirror,
then one `--insecure-registry=<entry>` per entry.

```toml
registry_mirrors    = ["http://mirror.lan.example:5000"]
insecure_registries = ["registry.lan.example:5001"]
```

| Key | Entries | Default |
|---|---|---|
| `registry_mirrors` | A bare origin: `http://` or `https://`, a host, an optional port, and no path (a single trailing `/` is allowed), query, fragment or userinfo. | empty (no flag) |
| `insecure_registries` | `host[:port]` (a DNS name, an IPv4 address, or an IPv6 address, bracketed when it carries a port) or a CIDR such as `192.168.0.0/16`. Never a URL. | empty (no flag) |

- **Config only, global only.** They are not `extra_specs` keys (`extra_specs`
  rejects them, as it rejects any key outside its allowlist) and not
  `[flavors.<name>]` fields. A provider config describes one Docker host, and a
  mirror is a property of that host's network, so a per-flavor override has no
  use that the global setting does not cover.
- **Validated at load**, whatever `dind_mode` is, so a typo fails at startup. A
  malformed entry, or a duplicate (mirrors compare case-insensitively and
  ignore one trailing `/`; CIDRs compare by their masked form), is rejected
  with an error that names the key and the entry. The rules follow dockerd's
  own registry configuration, a little stricter in five places: a mirror may
  not have a path, a mirror's scheme must be lowercase, an insecure registry
  may not carry a scheme, duplicates are rejected instead of dropped, and
  ports are 1-65535.
- **Address the mirror from inside the sidecar.** Use the Docker host's LAN IP
  or a DNS name, and not an address in `172.17.0.0/16` such as the host's
  `docker0` gateway `172.17.0.1`: the sidecar's own `dockerd` owns a `docker0`
  bridge with that range in the shared network namespace, so the address
  reaches the nested bridge, the dial is refused, and dockerd falls back to
  Docker Hub without any error (pulls succeed, but not through the mirror;
  check the mirror's access log). See
  [`dind-image-pulls.md`](dind-image-pulls.md#reachability).
- **Docker Hub only.** dockerd consults `registry_mirrors` only for Docker Hub
  images, tries the mirrors before Hub, and falls back to Hub when a mirror is
  unreachable or does not have the image. Pulls from other registries are not
  redirected; see [`dind-image-pulls.md`](dind-image-pulls.md).
- **An `http://` mirror needs no `insecure_registries` entry.** dockerd takes
  the scheme from the mirror URL, so a mirror configured as `http://host:5000`
  is contacted over plain HTTP whether or not it is also listed as insecure.
  This follows from Moby's registry code for the Engine 29.x line, for both the
  classic graph-driver and the containerd image-store pull paths, and was
  confirmed on Engine 29.9.2. An `insecure_registries` entry matters for an
  `https://` mirror whose certificate the sidecar does not trust (it turns off
  certificate verification for that host), and for non-mirror registries that
  jobs pull from by name over plain HTTP.
- **Logged.** When either list is non-empty, `CreateInstance` logs the
  effective values at sidecar creation.
- **Do not combine with a custom `dind_image` that sets the same options.** A
  `daemon.json` in the image with `registry-mirrors` or `insecure-registries`
  conflicts with these flags, and dockerd refuses to start ("the following
  directives are specified both as a flag and in the configuration file"), so
  every DinD job fails waiting for the daemon. Use either the config keys or
  the image's `daemon.json`, not both.

The keys appear in the machine-readable schema (`GetConfigJSONSchema`) and,
with illustrative values, in `examples/config.full.toml`. The setup around the
mirror itself (running a pull-through cache, reachability, TLS) is covered in
[`dind-image-pulls.md`](dind-image-pulls.md).

### Flavors

A `[flavors.<name>]` entry overrides `runner_image`, `runner_memory` and/or
`dind_memory` for the pools whose `extra_specs` select it by name. The usual
case is running some pools on the Ubuntu 26.04 runner image while the default
stays on Ubuntu 24.04 (see [`runner-images/README.md`](../runner-images/README.md)):

```toml
# Default for every pool: the Ubuntu 24.04 runner image.
runner_image = "ghcr.io/tzuh-hsu/garm-runner-noble@sha256:<digest>"

# Pools with extra_specs {"flavor": "resolute"} run the Ubuntu 26.04 image
# (GCC 15, LLVM 22).
[flavors.resolute]
  runner_image = "ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:<digest>"
```

A flavor's `runner_image` must be digest-pinned like the top-level one. Pools
of one repository on the two images get separate toolcache and pnpm store
volumes (keyed on the image's `garm.docker/os-release` label,
[ADR-003](adr/ADR-003-cache-keying-and-lifecycle.md)) and share its diagnostic-log volume.

### `[extra_specs]` (the `extra_env` allowlist)

The `extra_env` allowlist is an operator opt-in. It defaults to empty (a pool can
set no extra env at all); add the names you trust a pool admin to set:

```toml
[extra_specs]
# Environment-variable NAMES a pool's extra_specs.extra_env may set. Default empty.
# Hard-reserved names (RUNNER_*, DOCKER_*, GARM_*, RUN_AS_ROOT, PATH, LD_*, ...) are
# rejected even if listed here.
allowed_env = ["MY_CI_FLAG", "TZ"]
```

### `[status_report]` (runner status reported to GARM)

After the runner container is started, its credentials are delivered, and it
is verified running, the provider reports the runner's status to GARM's
instance callback (`POST {callback-url}/status`, authenticated with the
instance token it already holds for the credential fetch):

1. `installing` right away;
2. `idle` once the runner's own stdout shows `Listening for Jobs` (read
   through the Docker logs API), waiting at most `timeout_seconds`.

Neither report is made inside `CreateInstance`: a detached watcher process
(the provider binary re-executed in its own session) makes both, so
`CreateInstance` returns as soon as the runner is started and the watcher has
received its request (a handoff bounded by 5 seconds). See "Interaction
   with GARM's provider timeouts" below.

Only **JIT** runners are reported (scale sets always use JIT; pools do unless
`disable_jit_config` is set). A non-JIT runner registers inside the container,
so GARM has no agent ID for it until its own bootstrap reports one with `idle`;
an ID-less report would leave GARM unable to remove the runner's GitHub
registration on delete. Non-JIT pools do not need the reports: pools move
runners to `active` with forced updates.

**Log driver.** Detection reads the runner's stdout through the Docker logs
API, which works with the daemon's default `json-file` and `local` drivers (and
remote drivers with dual logging enabled). With the `none` driver the daemon
refuses to return logs ("configured logging driver does not support reading"),
so the provider falls back to waiting for the runner's `Runner.Listener`
process (via `docker top`) to stay up for 5 seconds, within the same
`timeout_seconds`, and records that in the `idle` event message. The fallback is weaker evidence (the
listener process starts slightly before it is connected to GitHub), so prefer a
readable log driver. The provider never changes the container's log driver.

A readiness signal is confirmed against the container's state before `idle` is
reported: a stopped container's logs still replay a `Listening for Jobs` line
it printed before exiting, so a runner that is no longer running is reported
`installing` with "runner container stopped before it was ready for jobs"
instead.

If the runner is not listening in time, it is left `installing`, so GARM's
`runner_bootstrap_timeout` still reaps it if it never comes up. `idle` is never
reported early for that reason. Both reports are best-effort: a callback error
never affects `CreateInstance`. If the watcher cannot be started, `CreateInstance`
logs a warning (token redacted) and still succeeds. If the `installing` report
fails, the watcher does not wait for the runner. A watcher that gives up
(timeout, or the runner container stopped) reports `installing` once more with
a message saying why, so the outcome is visible in GARM's instance events
(`garm-cli runner show`). The watcher's stdout and stderr are `/dev/null`, so it
never holds GARM's pipes open. Its warnings (a failed status report, for
example) go to the [`[logging]` file](#logging-persistent-log-file) when one is
set; otherwise they are discarded, and GARM's events and the runner status are
where its result shows.

**Interaction with GARM's provider timeouts.** GARM v0.2.1 puts no deadline on
a provider's `CreateInstance` call. Newer GARM (from
[cloudbase/garm#817](https://github.com/cloudbase/garm/pull/817), in no
release when checked on 2026-10-11) bounds it by the pool's or scale set's `runner_bootstrap_timeout`
and, if set, the smaller `[provider.external].exec_timeout_seconds`; on expiry
GARM kills the provider binary and marks the create failed. Because only the
watcher handoff runs inside `CreateInstance`, neither the callback's latency
nor `timeout_seconds` counts against either limit, and no coordination with
them is needed. The watcher
holds the instance token in memory for at most `timeout_seconds` plus about 25
seconds for its final report, then exits; it receives the token over a private
stdin pipe, never through its command line or environment. The provider's
proxy settings (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, either case) travel the
same way, so the watcher's requests to GARM and to a `tcp://` Docker host take
the same network path as the provider's own; proxy credentials are never
logged. `SSL_CERT_FILE`, `SSL_CERT_DIR` and `GODEBUG` (when set) are the only
variables in the watcher's own environment: Go's Unix root-CA loader honours
the first two, so a GARM callback trusted through them rather than the
bootstrap CA bundle stays trusted, and the runtime reads `GODEBUG` at start-up.
They hold no secrets. If GARM (or its
service manager) is restarted while a watcher runs, the watcher may be stopped
with it; the runner then stays `installing` and GARM's bootstrap timeout
applies.

**GARM in a container: run it with an init.** When the provider exits, the
detached watcher is adopted by PID 1 of its PID namespace, which must wait for
it when it finishes. systemd (GARM as a service) does. In a container, PID 1 is
whatever the image starts. In GARM's own image that is `/bin/garm` (its
`ENTRYPOINT`), and GARM's Docker quickstart starts it without an init, and
GARM never waits for processes it did not start, so each finished watcher
would stay a zombie (one per runner) until the container restarts. Start GARM's
container with an init: `docker run --init`, or `init: true` for the service
in Docker Compose.

The provider checks this itself: it reads `/proc/1/comm` and detaches the
watcher only when PID 1 is a known init (`systemd`, `docker-init`, `tini`,
`dumb-init`, `catatonit`, `s6-svscan`, and similar). Otherwise it logs a
warning and runs the watcher inside `CreateInstance` instead, so
`CreateInstance` then takes up to `timeout_seconds` (plus the two reports)
longer. GARM v0.2.1 does not bound `CreateInstance`; on GARM versions that do
(`exec_timeout_seconds`, `runner_bootstrap_timeout`), keep `timeout_seconds`
plus about a minute below that bound, or run GARM with an init.

This is required for **GARM v0.2.1 scale sets**: their job-started handler moves
a runner to `active` with a checked status transition that only `idle` (or
`offline`) may take, so a runner left at GARM's initial `pending` stalls the
scale-set listener until GARM removes the runner. Upstream relaxed this in
[cloudbase/garm#817](https://github.com/cloudbase/garm/pull/817) (commit
`1a9aeff2`), which was in no release when checked on 2026-10-11. Pools work either
way and also get accurate status.

```toml
[status_report]
# Bound on the detached watcher's wait for "Listening for Jobs".
# Default 90; range 0-600. CreateInstance itself does not wait.
# 0 disables status reporting entirely (no callback is made).
timeout_seconds = 90
```

| Key | Type | Default | Bound |
|---|---|---|---|
| `timeout_seconds` | integer | `90` | `0`–`600`; `0` disables |

The callback URL gets the same transport rules as the metadata URL (https, or
plain http only to a loopback host; no userinfo; `ca-cert-bundle` honored), and
no redirect is followed. The runner container is unchanged: it never sees the
instance token or the callback URL, and `docker inspect` shows neither. This
key is config-only; `extra_specs` cannot change it.

### `[logging]` (persistent log file)

The provider logs to stderr, and GARM keeps a provider's stderr only when the
call fails. A successful call's log lines are therefore lost, including the ones
that explain what happened after the fact: orphan-sweep teardowns, the
`orphan sweep: runner exited on its own` and
`teardown: runner was not running at delete` lines, a `DeleteInstance` that found
nothing to delete, `CreateInstance` progress and the `teardown: DinD sidecar IO`
line. Set `file` to also append every line to a file:

```toml
[logging]
# Absolute path. Unset (the default) means stderr only, exactly as before.
file = "/var/log/garm-provider-docker/provider.log"
```

| Key | Type | Default | Notes |
|---|---|---|---|
| `file` | string | unset | Absolute path; the directory must already exist and be writable by the user GARM runs the provider as |

How it behaves:

- **Same lines as stderr.** The file gets the same slog text format, the same
  level (`GARM_PROVIDER_DOCKER_LOG_LEVEL`, default `info`; GARM passes it on only
  if it is listed in `[provider.external] environment_variables`) and the same
  redaction as stderr; stderr keeps receiving every line too. Each line starts
  with a `time=` field.
- **Opened per call.** The provider is exec'd once per GARM call, so it opens the
  file with `O_APPEND|O_CREATE` once the config has loaded and closes it at the
  end of the call. A new file is created with mode `0640` (narrowed further by
  the umask). An existing file keeps its mode and owner.
- **Never fails a call.** If the file cannot be opened (missing directory, no
  permission, a relative path), the provider logs one warning on stderr and
  carries on with stderr only. If a write fails mid-call, it logs one warning,
  stops writing to the file for the rest of that call, and carries on.
- **Early lines are stderr only.** The path lives in this config file, so it is
  known only after the config has loaded. The `starting garm-provider-docker`
  line and any failure to read GARM's environment or to load the config are
  logged before that point and go to stderr only. Every line after the config
  loads, including `command completed`, goes to both.
- **The status watcher logs here too.** The detached watcher (see
  `[status_report]`) never loads this config and has no environment, so
  `CreateInstance` hands it the `file` path and the log level over the same
  private pipe as the rest of its request. It appends to the file like any other
  provider process (same format, same `O_APPEND` writes, safe with
  `copytruncate`), at the level the creating call used. It never logs the
  instance token or the callback URL. If the file cannot be opened the watcher
  carries on without logging, and its results still appear in GARM's instance
  events. With `file` unset its output stays on `/dev/null`.
- **Concurrent calls.** GARM may run several provider processes at once. Each
  record is written with a single `write(2)` on an `O_APPEND` descriptor, which
  local Linux filesystems apply atomically, so lines from different processes do
  not interleave or split, whatever their length. Do not point `file` at a
  network filesystem (NFS does not give `O_APPEND` that guarantee).
- **Contents.** The file holds instance names, pool IDs and repository names, as
  stderr does. No credential is ever logged (see `internal/logging`). Keep its
  directory readable only by the people who should see them.

**Rotation.** The provider never rotates or prunes the file; use logrotate.
Rotate with `copytruncate`: the file is copied and then emptied in place, so a
provider process that has it open keeps writing to the live path.

```text
/var/log/garm-provider-docker/provider.log {
    weekly
    rotate 8
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
```

`copytruncate` can lose lines written between the copy and the truncate, a
window of milliseconds. Rename-based rotation (without `copytruncate`) is also
safe, because every call reopens the path, but a call in flight during the
rename finishes its lines in the rotated file.

**Reading the DinD teardown line.** `teardown: DinD sidecar IO` is logged at
info, so it reaches stderr on every successful `DeleteInstance` and is then
discarded by GARM. It is available for the analysis in
[dind-image-pulls.md](dind-image-pulls.md#measuring-what-a-job-writes-the-teardown-log-line)
only when `[logging] file` is set.

**Reading the exited-runner line.** When a runner container exits on its own and
GARM has not deleted it within the exited-runner grace (about two minutes from
its `State.FinishedAt`), the next orphan sweep (run by `ListInstances` and at the
start of `CreateInstance`) tears the allocation down. Just before it does, it
logs one `orphan sweep: runner exited on its own` line with `instance`, `pool`,
the runner's `state`, `exit_code`, `finished_at` and `oom_killed`, the same
fields prefixed `dind_` when the DinD sidecar has stopped too (`dind_state`
alone while it still runs), `runner_phase`, and `log_tail`: the runner's last 20
lines of stdout and stderr. This is the record of why a runner that took a job
died before running it, so it is worth keeping with `file`.

If GARM reaches the runner first, as it does when a job's completed message
arrives after a mid-job crash or when scale-down picks a dead runner,
`DeleteInstance` logs the same fields just before it removes the container, on a
line of its own: `teardown: runner was not running at delete`. It logs at `debug`
for the normal end of an ephemeral runner (`runner_phase=job_completed`, exit code
0, not OOM-killed) and at `warn` otherwise. A running runner gets no line, and
neither does one that never started (`created`). Whichever
path runs first removes the container, so one exit is normally logged once; two
paths overlapping, or a removal that fails and is retried, can repeat the line.

`runner_phase` says how far the runner got, read from the listener's own lines in
the tail, the latest one deciding:

| `runner_phase` | The tail shows |
|---|---|
| `before_listening` | no `Listening for Jobs`, and the tail is short enough that it would show |
| `listening_no_job` | `Listening for Jobs` last |
| `job_started` | `Running job: ...` last |
| `job_completed` | `Job ... completed with result: ...` last |
| `unknown` | the phase cannot be told: 20 lines with no marker, a tail cut by the 64 KiB cap (it loses the newest lines) with anything short of `job_completed`, or logs that cannot be read |

The phase is read before the screen below, so a line withheld from `log_tail`
still counts, and only the phase's name reaches the log.

The tail is the runner's own output, so it is screened before it is logged: any
line that might carry a credential (a token, JIT config or `.credentials`
content, an authorization header, a metadata or callback URL, a URL with
userinfo or a query string, key material, or 20 or more letters, digits or
`+/=_-` in a row) is replaced whole by
`[line withheld: it may carry a credential]`, and `log_tail_withheld` counts
them. The run rule goes by length alone, so long paths, image digests and long
identifiers are withheld too; the runner's short status lines (`Listening for
Jobs`, `Running job: ...`, listener errors) come through. Each line is cut to
240 bytes, and at most
64 KiB is read (`log_tail_truncated=true` when that cut the tail short). The
report has a two-second budget. If the logs cannot be read (for example the
`none` log driver), the line is logged at `warn` with `log_tail_error` instead
of the tail, and the sweep or teardown carries on. On the `DeleteInstance` path
that budget is added to the call, only when a runner is not running, on top of
the DinD IO report's own.

This key is config-only; `extra_specs` cannot change it.
