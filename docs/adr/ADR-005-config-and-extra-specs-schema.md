# ADR-005: Config and `extra_specs` schema

Status: Accepted

## Context

Three trust tiers feed every provider decision ([ADR-001](ADR-001-dind-strategy.md)). Provider config, set by
the host operator, is the most trusted. Pool `extra_specs`, set by a GARM admin, is narrower and more
dynamic. The workflow payload, controlled by whoever can open a pull request, is untrusted and must never
influence image selection, mounts, privilege or resource limits.

`extra_specs` is read from the `extra_specs` field of the stdin payload, not from `GARM_POOL_EXTRASPECS`, which
GARM v0.2.1 leaves empty for scale-set instances ([`garm-contract.md`](../garm-contract.md)).

GARM's interface `v0.1.1` defines four optional methods: `GetSupportedInterfaceVersions`, `ValidatePoolInfo`,
`GetConfigJSONSchema` and `GetExtraSpecsJSONSchema`. GARM v0.2.1 does not call them, but they make a provider
document its own contract.

## Decision

### Format and validation

Provider config is **TOML**, like the other GARM providers. `examples/config.full.toml` lists every key with
its default, and [`docs/config-reference.md`](../config-reference.md) is the reference. The loader tolerates
unknown keys, so a file can carry keys from another version; a test keeps the shipped examples free of them.

`extra_specs` is validated against a published draft-07 JSON Schema with `gojsonschema`, **failing closed**
before any Docker operation. A rejected create is an error with nothing to roll back. The provider implements
all of interface `v0.1.1`. One binary serves `v0.1.1` when `GARM_INTERFACE_VERSION=v0.1.1` and `v0.1.0`
otherwise. The two schemas are embedded next to the structs they describe (`internal/config/schema.json` and
`internal/extraspecs/schema.json`) and returned by `GetConfigJSONSchema` and `GetExtraSpecsJSONSchema`. The
config schema is documentation: the loader does not validate against it. `ValidatePoolInfo` runs the same
parse-and-resolve path as `CreateInstance`, so a bad pool definition can fail early. A create can still reject
what it accepts: a DinD mode with no `dind_image` configured is checked only in `CreateInstance`. It ignores its positional `image` and `flavor` arguments, because the flavor a
pool uses is `extra_specs.flavor`.

### What `extra_specs` may set

The schema has `additionalProperties: false` and these keys: `flavor`, `dind_mode`, `runner_memory`,
`dind_memory`, `storage_driver`, `runner_labels` and `extra_env`. A raw image reference, `docker_host`, a host
socket mount and the privileged flag have no field at all, so they are absent, which is stronger than denied. The
managed label set is never settable either.

- **`flavor`** names a `[flavors.*]` entry. It is the only way to vary the runner image per pool
  ([ADR-002](ADR-002-runner-image-and-jit-delivery.md)). A flavor may override `runner_image`, `runner_memory`
  and `dind_memory`, and every field falls back to the top-level value.
- **`dind_mode`** passes through `config.EffectiveDindMode`, the one place the `allowed_dind_modes` ceiling
  is enforced. A mode outside the ceiling is rejected like any schema violation, so a pool cannot escalate.
- **`runner_memory` and `dind_memory`** must be positive byte sizes no larger than the effective ceiling (the
  flavor's value, else `[resources]`). A value over the ceiling is **rejected, not clamped**, so a pool asking
  for more than the operator allows is told no and not quietly given less. An unset ceiling accepts any finite
  request. Zero in any spelling (`"0"`, `"0GiB"`) is rejected, because Docker treats a zero memory limit as
  unlimited and accepting it would remove the operator's ceiling.
- **`storage_driver`** is `overlay2` or `vfs` and defaults to the configured driver.
- **`runner_labels`** are appended to the runner labels without JIT only, since GARM bakes the labels into
  the runner configuration with JIT.
- **`extra_env`** is governed by two gates. The operator lists the variable names a pool may set in
  `[extra_specs].allowed_env`, which is empty by default, so a pool can inject no environment unless the
  operator opts names in. A name outside the list is rejected. Independently, a hard-reserved set is always
  rejected even if allowlisted, case-insensitively: the prefixes `RUNNER_`, `DOCKER_`, `ACTIONS_RUNNER_INPUT_`,
  `GARM_` and `LD_`, and the names `JIT_CONFIG_ENABLED`, `GITHUB_URL`, `RUN_AS_ROOT`, `WAIT_FOR_DOCKER_SECONDS`,
  `BASH_ENV`, `ENV`, `IFS`, `PATH` and `NODE_OPTIONS`. The one exemption is the exact, case-sensitive name
  `GARM_JOB_HOOKS` (`fail`, `warn` or `off`), the mode of the runner image's job-started hook
  ([ADR-002](ADR-002-runner-image-and-jit-delivery.md)); it is read by a script that runs as the runner user and
  compares it with fixed words, and it still needs an `allowed_env` entry. The reasons: `RUNNER_EPHEMERAL=false` would break the
  single-job model that teardown relies on, `DOCKER_HOST` would defeat the socket guarantees of ADR-001,
  `RUN_AS_ROOT=true` would run the job as root, the entrypoint reads `GARM_CRED_WAIT_SECONDS` and
  `WAIT_FOR_DOCKER_SECONDS` into Bash arithmetic where a crafted value is command substitution as root, and the
  rest are shell and loader hijack vectors. Names must match `^[A-Za-z_][A-Za-z0-9_]*$`, enforced by the schema
  and again in Go before the other checks. Without that, the key `JIT_CONFIG_ENABLED=false` would reach Docker
  as `JIT_CONFIG_ENABLED=false=<value>`, which Docker parses as a variable named `JIT_CONFIG_ENABLED`, past the
  exact-name check. A non-reserved, allowlisted name that the provider also injects (for example
  `npm_config_store_dir`) is dropped in favour of the provider's value. Provider-injected environment always
  wins.

### Keys GARM adds

Since GARM v0.2.0, GARM injects `runner_install_template` (its rendered userdata wrapper, base64) into the
`extra_specs` of every `CreateInstance` unless the admin set it, and requires an admin to set it when a pool
or scale set has no install template. `additionalProperties: false` would reject every create. `Parse` therefore
removes keys on a fixed, provider-defined list, currently only `runner_install_template`, before validation, so
they never reach `Resolve`. The match is exact and case-sensitive, and the value must be a JSON string or
`null`. Any other unknown key, alone or next to a reserved one, is still rejected. GARM's other
settings (`extra_context`, `pre_install_scripts`, `enable_boot_debug`) are never injected and drive a userdata
flow this provider does not use, so a pool that sets them is told so by a rejection and not silently ignored.
The published schema does not list the reserved key as a property, and its description documents the exception.

### Reading the GARM environment

Under interface `v0.1.1`, GARM v0.2.1 sends an empty `GARM_POOL_ID` for scale-set `DeleteInstance`,
`GetInstance`, `StartInstance` and `StopInstance`, and `garm-provider-common` v0.1.9 requires one, so a
scale-set runner could never be deleted. `internal/garmenv` replaces the library's environment reader. It
mirrors it field for field and delegates every check to the library's own `Validate`, except that an empty
pool ID is accepted for exactly those four commands, none of which uses it. `CreateInstance` and
`ListInstances` still require a pool ID. Interface `v0.1.0` goes to the library unchanged. On GARM 0.2.1 and
earlier, register the provider with `interface_version = "v0.1.0"` because of an upstream `ListInstances` bug
under `v0.1.1` ([`garm-contract.md`](../garm-contract.md)).

### Other config rules

- `runner_image`, each flavor's `runner_image` and `dind_image` must be digest-pinned. The dev-only flags
  `allow_unpinned_runner_image` and `allow_unpinned_dind_image` relax this independently.
- `[network].enable_job_network` is reserved and not honoured: the network is always created because it is the
  isolation guarantee and the claim marker ([ADR-004](ADR-004-teardown-and-orphan-cleanup.md)).
- Only memory limits are configurable. There are no CPU or PID limits.
- The bearer token is removed from every error the metadata client returns. A hostile endpoint can reflect the
  token into a redirect URL, `net/http` puts that URL into its error, and `main` logs errors. This backs up the
  primary control, which is keeping the token out of the container.

## Rationale

TOML lowers the learning curve for operators who run other GARM providers. A validated schema keeps the
trust boundary in code and not in a README. The
`allowed_dind_modes` ceiling, the `extra_env` allowlist and the hard-reserved set are all instances of one
principle: validation is where the admin tier is stopped from reaching into operator territory, so every
field that could cross that line needs its own explicit bound there.

## Alternatives considered

- YAML or a `koanf` loader: less familiar to this ecosystem's operators.
- No `extra_specs` validation: it lets the admin tier widen unnoticed.
- No ceiling on `extra_specs.dind_mode`: an admin with pool-edit access could turn on `privileged-sidecar` on a
  host the operator meant to keep at `none`.
- A denylist-only `extra_env`: it accepts anything not on the list, and a pool could set controls the list does
  not name. The allowlist plus the hard-reserved set fails closed.
- Clamping out-of-range memory: it hides a mismatch between what a pool asks and what it gets.

## Consequences

- The two embedded schemas and their Go structs are kept in sync by review. No tool generates the
  documentation, so [`config-reference.md`](../config-reference.md) is maintained by hand.
- The `extra_specs` validator needs the live config (`allowed_dind_modes`, the memory ceilings, `allowed_env`
  and the flavors) at validation time, which couples it to provider config. That is necessary for what these
  checks protect.

## Open questions

- Whether flavors may inherit from one another.
- Whether the hard-reserved set should be extensible by the operator.
