# How GARM calls an external provider

What GARM sends to this provider and what it expects back, as implemented by `internal/` and
`garm-provider-common` v0.1.9. Statements about GARM itself describe GARM v0.2.1, the latest release at the time
of writing. GARM's own description is
[`doc/external_provider.md`](https://github.com/cloudbase/garm/blob/main/doc/external_provider.md).

## Invocation

GARM starts the provider executable once per call. Nothing persists between calls, and calls can overlap.

| Command | Environment and input |
|---|---|
| every command | `GARM_COMMAND`, `GARM_CONTROLLER_ID`, `GARM_PROVIDER_CONFIG_FILE`, `GARM_INTERFACE_VERSION` (empty means `v0.1.0`) |
| `CreateInstance` | `GARM_POOL_ID`, and a `BootstrapInstance` JSON document on stdin |
| `ListInstances` | `GARM_POOL_ID` |
| `DeleteInstance`, `GetInstance`, `StartInstance`, `StopInstance` | `GARM_INSTANCE_ID` |
| `RemoveAllInstances`, `GetVersion` | nothing further |
| interface `v0.1.1` | adds `GARM_POOL_ID` and `GARM_POOL_EXTRASPECS` to the instance commands, and the commands `GetSupportedInterfaceVersions`, `ValidatePoolInfo`, `GetConfigJSONSchema`, `GetExtraSpecsJSONSchema` |

- The command names are `StartInstance` and `StopInstance`, although GARM's documentation says `Start` and `Stop`.
- GARM v0.2.1 never calls `GetVersion`, `StartInstance`, `StopInstance` or the four `v0.1.1` commands. The
  provider implements all of them.
- Exit codes: `0` success, `30` not found, `31` duplicate, anything else `1`. GARM treats `30` from
  `DeleteInstance` as success. This provider returns `31` only from `CreateInstance`, when a claim network for
  the instance name already exists.
- Stdout carries only the result JSON. Logs go to stderr, and GARM keeps a provider's stderr only when the call
  fails (see [`[logging]`](config-reference.md#logging-persistent-log-file)).
- GARM forwards only the environment variables listed in `[provider.external] environment_variables`. It runs
  `provider_executable` as a path, so the provider cannot run as a container (see the README).

## The `CreateInstance` payload

The provider reads `name`, `os_type`, `arch`, `repo_url`, `metadata-url`, `callback-url`, `instance-token`,
`ca-cert-bundle`, `jit_config_enabled`, `extra_specs`, `pool_id`, and without JIT also `labels` and
`github-runner-group`. With JIT, GARM bakes the labels and group into the runner configuration. `image` and
`flavor` are ignored: the image comes from provider config and the flavor from `extra_specs.flavor`.

- `os_type` must be `linux` and `arch` `amd64` or `arm64`. Anything else is rejected before any Docker call.
- `repo_url` is the repository, organization or enterprise URL. It keys the caches and, without JIT, builds the
  registration URL. Only GitHub URL shapes are understood.
- `pool_id` is a pool UUID for pools and `<scale set name>-<entity id>` for scale sets. It labels the allocation.
- No field carries the JIT configuration. The provider reads `extra_specs` from this stdin field and never from
  `GARM_POOL_EXTRASPECS`, which GARM v0.2.1 sends as `base64("null")` for scale-set instances on every command
  except `CreateInstance`.

## Credentials

GARM generates the JIT configuration itself and serves it per file, with `Authorization: Bearer {instance-token}`:
`GET {metadata-url}/credentials/runner`, `.../credentials` and `.../credentials_rsaparams`. Without JIT the
provider calls `GET {metadata-url}/runner-registration-token`. Pools use JIT unless `disable_jit_config` is set.
Scale sets always use it.

The provider makes these requests itself and never gives the token to the runner container. It requires https
(plain http only to a loopback host), refuses URLs with userinfo, trusts `ca-cert-bundle`, follows no
cross-origin redirect and removes the token from every error it reports. The fetch has a 60 second budget.

## Results

GARM passes `provider_id` as `GARM_INSTANCE_ID` and falls back to `name` when `provider_id` is empty. The
provider returns the instance name as `provider_id`, so both values are the same.

The result is a `ProviderInstance` (`provider_id`, `name`, `os_type`, `os_arch`, `addresses`, `status`,
`provider_fault`). GARM rejects a result with an empty `provider_id` or `name`, or an unknown `status`. The
provider maps Docker `running` to `running`, `exited` to `stopped`, `created` to `pending_create`, an OOM-killed
or dead container to `error`, and anything else to `unknown`. A failed `CreateInstance` returns an error and no
result, because the library discards the result on error.

## Callbacks and timeouts

A runner reports status with `POST {callback-url}/status` and the instance token. On GARM v0.2.1 a scale-set
runner must be `idle` before GARM can mark it `active`. This provider posts `installing` and `idle` from its own
process, so the runner never holds the token ([ADR-002](adr/ADR-002-runner-image-and-jit-delivery.md)).

GARM's reaper runs every 5 minutes and removes instances that are not online after `runner_bootstrap_timeout`
(20 minutes by default), by calling `DeleteInstance`. The provider keeps no runner timeout of its own.
`DeleteInstance` only has to be idempotent.

## Known upstream issues

| GARM | Issue | Provider behaviour |
|---|---|---|
| 0.2.0 and later | GARM adds `runner_install_template` to the `extra_specs` of every `CreateInstance`. | Accepted and ignored. Any other unknown key is rejected ([ADR-005](adr/ADR-005-config-and-extra-specs-schema.md)). |
| 0.2.1, scale sets | Under `v0.1.1`, `GARM_POOL_ID` is empty on `DeleteInstance`, `GetInstance`, `StartInstance` and `StopInstance`, and `garm-provider-common` v0.1.9 rejects that. | `internal/garmenv` tolerates the empty value for those four commands only. |
| 0.2.1 | The `v0.1.1` `ListInstances` wrapper tests `err == nil` instead of `err != nil` and reports every successful list as an error. Fixed by [cloudbase/garm#878](https://github.com/cloudbase/garm/pull/878), in no release when checked on 2026-10-11. | Register with `interface_version = "v0.1.0"` on GARM 0.2.1 and earlier. GARM does not call the `v0.1.1`-only commands, so nothing is lost. |
| 0.2.1, scale sets | A runner at `pending` or `installing` cannot become `active`, so the scale set stalls on the first job. Relaxed by [cloudbase/garm#817](https://github.com/cloudbase/garm/pull/817), in no release when checked on 2026-10-11. | The status reports above narrow the window. See [`[status_report]`](config-reference.md#status_report-runner-status-reported-to-garm). |
| 0.2.1 | A pool with `extra_specs` of JSON `null` makes GARM panic before it calls the provider ([cloudbase/garm#787](https://github.com/cloudbase/garm/issues/787)). The fix was planned for 0.2.2 when checked on 2026-10-11. | Set `extra_specs` to `{}`. |

GARM's `main` branch replaces the scale-set pseudo pool ID with a UUIDv5. `ListInstances` is scoped to whatever
pool ID GARM sends, so both forms work. Releases newer than 0.2.1 have not been checked.

## What has been verified

The live gate ([`demo-m0.md`](demo-m0.md)) runs a real job through GARM v0.2.1 with scale-set dispatch and JIT, on
a `linux/amd64` host, with `dind_mode = "none"` and `privileged-sidecar`. Pool (webhook) dispatch, the non-JIT
path, `sysbox-runc` and `linux/arm64` hosts have not been verified end to end.
