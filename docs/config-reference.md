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
2. **Pool `extra_specs`** (JSON) — set by a **GARM admin**, a narrower and more dynamic tier. Read from the stdin `BootstrapInstance.extra_specs` field, **never** `GARM_POOL_EXTRASPECS` (which is empty for scale sets).
3. **The workflow payload** — controlled by anyone who can open a PR; **never** consulted for a privileged decision.

`extra_specs` can select *within* what the operator config allows; it can never
escalate beyond it.

## Emitting the schemas from the binary (v0.1.1)

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
