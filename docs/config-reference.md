# Config and `extra_specs` reference

This document is the operator- and pool-admin-facing reference for
`garm-provider-docker`'s configuration and for the pool `extra_specs` contract
(ADR-005). Both contracts are published as machine-readable JSON Schemas that the
binary itself can emit (v0.1.1 self-description commands), so this document is
generated from the same source of truth the code validates against, not
maintained separately.

## Trust tiers (why there are two contracts)

Three inputs feed every provider decision, in strict trust order (ADR-001/ADR-005):

1. **Provider config** (TOML) — set by the host **operator**, the most trusted tier.
2. **Pool `extra_specs`** (JSON) — set by a **GARM admin**, a narrower and more dynamic tier. Read from the stdin `BootstrapInstance.extra_specs` field, **never** `GARM_POOL_EXTRASPECS` (which GARM leaves empty for scale-set commands other than `CreateInstance`).
3. **The workflow payload** — controlled by anyone who can open a PR; **never** consulted for a privileged decision.

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
as of 2026-10-11 — see the README's "Register with GARM" section).

```sh
# The extra_specs JSON Schema (draft-07)
GARM_INTERFACE_VERSION=v0.1.1 GARM_COMMAND=GetExtraSpecsJSONSchema \
  GARM_CONTROLLER_ID=x GARM_PROVIDER_CONFIG_FILE=/path/config.toml \
  ./garm-provider-docker

# The provider-config JSON Schema
GARM_INTERFACE_VERSION=v0.1.1 GARM_COMMAND=GetConfigJSONSchema \
  GARM_CONTROLLER_ID=x GARM_PROVIDER_CONFIG_FILE=/path/config.toml \
  ./garm-provider-docker

# Validate a pool's extra_specs early (what `garm-cli pool update --extra-specs`
# would call): exits non-zero with a descriptive error if it violates the schema,
# the allowed_dind_modes ceiling, a memory ceiling, the extra_env allowlist, or a
# hard-reserved env name.
GARM_INTERFACE_VERSION=v0.1.1 GARM_COMMAND=ValidatePoolInfo \
  GARM_CONTROLLER_ID=x GARM_PROVIDER_CONFIG_FILE=/path/config.toml \
  GARM_POOL_EXTRASPECS='{"dind_mode":"privileged-sidecar"}' \
  ./garm-provider-docker
```

## `extra_specs` allowlist (what a pool admin may set)

Only these keys are accepted. **Any other key is rejected** (`additionalProperties:
false`) — which is what structurally keeps a raw image reference, `docker_host`,
and the `privileged` flag out of this channel: they have no field at all.

| Key | Type | Bound |
|---|---|---|
| `flavor` | string | Must name a `[flavors.<name>]` entry in the provider config. The **only** channel that varies the runner image (ADR-002) — `extra_specs` never accepts a raw image ref. |
| `dind_mode` | `none` \| `privileged-sidecar` \| `sysbox-runc` | Bounded by the operator's `allowed_dind_modes` **ceiling**: a mode outside that list is rejected. |
| `runner_memory` | byte-size string (`"4GiB"`, `"512MiB"`, `"2GB"`) | Bounded by the effective configured `runner_memory`. Must be **positive** (a zero value in any form is rejected — H1). **Rejected if it exceeds the ceiling** (reject, not clamp). |
| `dind_memory` | byte-size string | Bounded by the effective configured `dind_memory`, same rules (positive-only; reject-not-clamp). |
| `storage_driver` | `overlay2` \| `vfs` | Defaults to the configured `storage_driver`. |
| `runner_labels` | array of strings | Appended to the runner's labels. **Non-JIT mode only** — in JIT mode GARM bakes the label set server-side, so these are inert. |
| `extra_env` | object of string→string | Extra runner-container env. Governed by an **operator allowlist** and a **hard-reserved** set (see below). |

### GARM-reserved keys (accepted and ignored)

GARM itself writes some keys into `extra_specs`. These — and only these — are
removed before the schema check, so they are accepted and have **no effect**
on this provider:

| Key | Why it appears | Effect here |
|---|---|---|
| `runner_install_template` | GARM v0.2.0+ injects its rendered userdata wrapper (base64) into every `CreateInstance` payload unless the admin set the key; GARM also *requires* an admin to set it on a pool/scale set with no install template. | None — it drives GARM's userdata install flow, which this provider does not use (prebuilt runner image, ADR-002). Must be a JSON string. |

Every other unknown key is still rejected, including next to a reserved key.
GARM's other install-flow keys (`extra_context`, `pre_install_scripts`,
`enable_boot_debug`) are never injected by GARM and have no effect here, so
they are rejected rather than silently ignored (ADR-005 amendment 2026-10-11).

### Ceilings, the extra_env allowlist, and the hard-reserved set (read this)

- **`allowed_dind_modes` ceiling (ADR-001 F7), fail-closed by default.** The operator's `allowed_dind_modes` is the final word on `dind_mode`, and it **defaults to `["none"]`**: DinD is unavailable on a host whose config never mentions it, so no pool's `extra_specs.dind_mode` can obtain a privileged sidecar by omission. To enable DinD you must widen the ceiling *and* set `dind_image`, e.g. `allowed_dind_modes = ["none", "privileged-sidecar"]` — read ADR-001's "Residual risk" section first. `dind_mode` itself must be a member of this list, so widening the ceiling is also what makes a non-`none` `dind_mode` load at all. No pool's `extra_specs.dind_mode` can escalate past it, full stop.
- **A DinD mode with no `dind_image` is rejected before any Docker work.** Config load already requires `dind_image` when the *config's own* `dind_mode` is non-`none`, but that cannot see a pool's `extra_specs`. If a pool selects a DinD mode within a widened ceiling on a host that has no `dind_image` set, `CreateInstance` rejects it up front — before the orphan sweep, the claim network, the volumes, or the credential fetch — rather than failing at the sidecar image pull with a partial allocation to roll back.
- **Memory is positive-only and reject-not-clamp (H1).** A `runner_memory`/`dind_memory` over the configured (or flavor-resolved) ceiling is rejected with a clear error; it is never silently reduced. A request at or below the ceiling is accepted; an unset ceiling (unlimited) accepts any finite request. A **zero** value in any representation (`"0"`, `"0GiB"`, `"0 B"`) is rejected — Docker treats a 0 memory limit as *unset* (unlimited), so accepting it would silently remove the operator's ceiling.
- **`extra_env` is fail-closed via an operator ALLOWLIST (H2).** The operator lists the env NAMES a pool may set in `[extra_specs].allowed_env` (default **empty**). A name not on the allowlist is **rejected** — by default a pool can inject **no** extra environment at all.
- **`extra_env` names must match a strict charset (H3):** `^[A-Za-z_][A-Za-z0-9_]*$`. A key containing `=`, whitespace, unicode, or a leading digit is rejected — this closes the bypass where `JIT_CONFIG_ENABLED=false` would be emitted as `JIT_CONFIG_ENABLED=false=<value>` and flip a reserved variable.
- **Hard-reserved `extra_env` names — always rejected, even if allowlisted, case-insensitive:**
  - prefixes `RUNNER_*`, `DOCKER_*`, `ACTIONS_RUNNER_INPUT_*`, `GARM_*`, `LD_*`
  - exact `JIT_CONFIG_ENABLED`, `GITHUB_URL`, `RUN_AS_ROOT`, `WAIT_FOR_DOCKER_SECONDS`, `BASH_ENV`, `ENV`, `IFS`, `PATH`, `NODE_OPTIONS`

  These are names the provider, the runner-image entrypoint, or the interpreter relies on. Letting a pool set `RUNNER_EPHEMERAL=false` or `DOCKER_HOST=…` would break the single-job teardown model or the DinD socket-reachability guarantees; `RUN_AS_ROOT=true` would run the runner and its job as **root** (defeating non-root isolation); the entrypoint reads `GARM_CRED_WAIT_SECONDS`/`WAIT_FOR_DOCKER_SECONDS` into Bash arithmetic where a crafted value is a command-substitution RCE as root; and `BASH_ENV`/`LD_*`/`PATH`/… are classic shell/loader hijack vectors. A non-reserved, allowlisted `extra_env` name that merely collides with another provider-injected variable is dropped in favor of the provider's value — **provider-injected environment always wins**.

### Never settable from `extra_specs` (structurally)

`docker_host`; any host-socket mount (no such field exists); the `privileged`
flag (derived from `dind_mode` only, capped by `allowed_dind_modes`); the
`garm.docker/*` managed-label set (ADR-004); a raw image reference; and the
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
{ "image": "attacker/evil:latest" }              // unknown key — raw image is not a channel
{ "dind_mode": "privileged-sidecar" }            // if allowed_dind_modes = ["none"]
{ "runner_memory": "64GiB" }                     // if the ceiling is 8GiB
{ "runner_memory": "0GiB" }                      // H1 — zero is not a valid limit
{ "extra_env": { "RUNNER_EPHEMERAL": "false" } } // hard-reserved name
{ "extra_env": { "RUN_AS_ROOT": "true" } }       // hard-reserved — would run the job as root
{ "extra_env": { "MY_CI_FLAG": "on" } }          // if MY_CI_FLAG is not on [extra_specs].allowed_env
{ "extra_env": { "JIT_CONFIG_ENABLED=false": "x" } } // H3 — '=' in the key
```

## Provider config (TOML)

See ADR-005 for the full illustrative config and rationale, and emit the
authoritative schema with `GetConfigJSONSchema`. The keys most relevant to the
`extra_specs` bounds above are `allowed_dind_modes` (the `dind_mode` ceiling),
`[resources].runner_memory`/`dind_memory` (the memory ceilings), `storage_driver`
(the default), the `[flavors.*]` map (the only image-varying channel), and
`[extra_specs].allowed_env` (the `extra_env` allowlist).

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
(`garm-cli runner show`). The watcher's own output goes nowhere (its stdout and
stderr are `/dev/null`, so it never holds GARM's pipes open); GARM's events and
the runner status are where its result shows.

**Interaction with GARM's provider timeouts.** GARM v0.2.1 puts no deadline on
a provider's `CreateInstance` call. Newer GARM (from
[cloudbase/garm#817](https://github.com/cloudbase/garm/pull/817), unreleased as
of 2026-10-11) bounds it by the pool's or scale set's `runner_bootstrap_timeout`
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

This is required for **GARM v0.2.1 scale sets**: their job-started handler moves
a runner to `active` with a checked status transition that only `idle` (or
`offline`) may take, so a runner left at GARM's initial `pending` stalls the
scale-set listener until GARM removes the runner. Upstream relaxed this in
[cloudbase/garm#817](https://github.com/cloudbase/garm/pull/817) (commit
`1a9aeff2`), which is not in any release as of 2026-10-11. Pools work either
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
