# ADR-005: Config and extra_specs Schema

Status: Accepted (2026-07-19); amended 2026-07-22 (M3-W1 and M3-W2) and 2026-07-26 (M4-W1: `allowed_dind_modes` now defaults to `["none"]`, fail-closed — see ADR-001's "fail-closed `allowed_dind_modes` default" Amendment; this ADR's text below is corrected in place)

## Context

Three trust tiers feed into every provider decision (see ADR-001): provider config, set by the host operator, is the most trusted; pool `extra_specs`, set by a GARM admin, is a narrower and more dynamic trust tier; the workflow payload, effectively controlled by whoever can open a pull request or push a branch, is untrusted and must never influence privileged decisions such as image selection, mount layout, privilege escalation, or resource limits. `extra_specs` must be read from the stdin bootstrap payload's `extra_specs` field — **not** the `GARM_POOL_EXTRASPECS` environment variable, which is empty for scale sets due to an upstream gap (research.md §1).

GARM's `ExternalProvider` interface (v0.1.1, per `garm-provider-common` v0.1.9) defines optional methods — `GetSupportedInterfaceVersions`, `ValidatePoolInfo`, `GetConfigJSONSchema`, `GetExtraSpecsJSONSchema` — that GARM does not currently call for every provider, but that exist in the interface contract and are a low-cost way to make a provider self-documenting.

## Decision

Use **TOML** for the provider configuration file, matching the convention already established across the `garm` ecosystem's other providers. Publish a **JSON Schema** for `extra_specs` and validate every `CreateInstance` call's `extra_specs` against it using `gojsonschema`, failing closed (rejecting the pool's request) on any validation error.

Implement the full `ExternalProvider` interface v0.1.1, including the four methods listed above, even though GARM does not invoke all of them yet: the cost is two `go:embed`-ed JSON files (one for provider config, one for `extra_specs`) and the corresponding Go structs, and the benefit is a provider that documents its own contract in a machine-readable form — following the same published-schema practice used by the LXD provider's README.

**Illustrative provider config** (TOML; all values below are generic placeholders, not a real deployment's configuration):

```toml
docker_host = "unix:///var/run/docker.sock"
runner_image = "ghcr.io/tzuh-hsu/garm-runner-noble@sha256:REPLACE_WITH_DIGEST"
dind_image = "docker:dind@sha256:REPLACE_WITH_DIGEST"
dind_mode = "privileged-sidecar"  # none | privileged-sidecar | sysbox-runc — an example operator-chosen value; when OMITTED the default is "none" (ADR-001 F11 Amendment: DinD is off unless the operator opts in)
allowed_dind_modes = ["none", "privileged-sidecar", "sysbox-runc"]  # ceiling on what extra_specs may select (ADR-001) — an example operator-WIDENED ceiling; when OMITTED the default is ["none"] (fail-closed: no DinD mode at all until the operator opts in)
storage_driver = "overlay2"      # or "vfs"
# enable_runner_callbacks: NOT a real field — the opt-in that ADR-002 designs
# for was never implemented through M3 and is deferred (post-M4 follow-up);
# it is intentionally absent from this example and from the config JSON Schema.

[resources]
runner_memory = "8GiB"
dind_memory = "4GiB"
# No cpu or pids limits by design — see Rationale.

[cache]
enabled = true
generation = "1"
pnpm_major = "9"
toolcache_path = "/opt/hostedtoolcache"
pnpm_store_path = "/opt/pnpm-store"  # resolved M2-W1 (ADR-003 Amendment): mounted volume + npm_config_store_dir
stale_cache_eviction_days = 30
diagnostic_log_retention_days = 7
allow_org_shared = false  # operator opt-in for org/enterprise-keyed shared caches — see ADR-003

[network]
enable_job_network = true
internal = false  # default (2026-07-21 owner ruling, ADR-001 Amendment): the runner needs
# egress to reach GitHub and DinD needs it for registry pulls; per-job isolation comes from
# each allocation's own separate network, not this flag. Set true only for a fully airgapped
# job (reserved building block for a future proxy-sidecar egress-allowlist opt-in — ADR-001).
# Also see ADR-001: the host Docker daemon must not be configured to listen on TCP.

# Note: the reserved-name denylist for extra_specs env (RUNNER_*, DOCKER_*, JIT_CONFIG_ENABLED,
# GITHUB_URL, ACTIONS_RUNNER_INPUT_*, ...) is fixed by the provider itself and is not an
# operator-configurable TOML block — see the Decision text below.

[flavors.default]
# A named flavor is the ONLY way to vary the runner image per pool.
runner_memory = "8GiB"
dind_memory = "4GiB"
# runner_image = "..." (optional per-flavor override)
```

**`extra_specs` allowlist** (the GARM-admin trust tier — safe to accept from `extra_specs`): flavor selection (by name, from the `[flavors.*]` map only), `dind_mode` (bounded by `allowed_dind_modes` — see below), `runner_memory`/`dind_memory` (clamped or rejected against the config-defined maximum — see Open questions), `storage_driver`, extra runner labels, and an operator-allowlisted set of extra environment variables (itself bounded by the reserved-name denylist below).

**`dind_mode` is bounded by the operator's `allowed_dind_modes` ceiling (see ADR-001 for rationale).** Provider config defines `allowed_dind_modes`, a list defaulting to `["none"]` — fail-closed, so no DinD mode is selectable until the operator widens the ceiling explicitly (2026-07-26; see ADR-001's "fail-closed `allowed_dind_modes` default" Amendment). Every `CreateInstance` call's `extra_specs.dind_mode`, whatever a pool requests, is validated against this list at the same JSON-Schema-validation step described above; a request for a mode outside `allowed_dind_modes` is rejected exactly like any other schema violation — a pool cannot escalate to a mode the operator has not allowed, full stop. On a shared or security-sensitive host, an operator can set `allowed_dind_modes = ["none"]` to forbid privileged workloads outright, regardless of what any pool's `extra_specs` requests.

Per-pool `extra_specs.dind_mode` selection itself — the schema field and its JSON-Schema validation wiring described in this paragraph — is deferred to M3, exactly like named-flavor selection (both are pool-level overrides that need the not-yet-built `extra_specs` schema-validation path to reach `CreateInstance`). What already exists as of M1/WP4 is the ceiling's enforcement point, ahead of that wiring: `internal/config.Config.EffectiveDindMode(poolMode string)` resolves the effective `dind_mode` — the config default when `poolMode` is empty (all of M1), or `poolMode` once a caller supplies one — and errors if the result is outside `AllowedDindModes`; `internal/provider.Provider.resolveDindMode` calls it on every `CreateInstance`, before any Docker operation, so a misconfigured ceiling fails closed today even without `extra_specs.dind_mode` existing yet. When M3 lands the `extra_specs` field, it MUST thread the pool's requested mode through as `EffectiveDindMode`'s `poolMode` argument rather than adding a second, parallel ceiling check: this is deliberately the single place the ceiling is enforced, so M3's job is limited to parsing/validating the field's presence and shape and handing the resulting string to the existing call.

**Reserved-name denylist for `extra_specs` extra environment variables.** The operator-allowlisted "extra environment variables" mechanism above lets a GARM admin add environment variables to the runner container — but it must never be able to set or override a name the provider itself relies on for the runner contract (ADR-002) or for connectivity control. The following names/prefixes are denylisted unconditionally, independent of whatever the operator's own allowlist contains: `RUNNER_*`, `DOCKER_*`, `JIT_CONFIG_ENABLED`, `GITHUB_URL`, `ACTIONS_RUNNER_INPUT_*`, and any other provider-injected name (ADR-002's environment contract is the authoritative list). **Provider-injected environment always wins the merge**: if an operator's `environment_variables` allowlist and a pool's `extra_specs` env both name a reserved key, the provider-injected value is used and the attempted override is dropped (and logged), never silently accepted. Without this, a GARM admin could set `RUNNER_EPHEMERAL=false` — breaking the single-job-ephemerality assumption the whole teardown model (ADR-004) is built on — or `DOCKER_HOST` pointed somewhere the operator never intended, undermining the DinD socket-reachability guarantees in ADR-001.

**Never overridable from `extra_specs` or the workflow payload, under any circumstance**: `docker_host`; any host-socket mount (no such field exists in the schema at all — it is structurally absent, not merely rejected); the privileged flag (derived exclusively from `dind_mode`, never set directly, and itself capped by `allowed_dind_modes`); the managed label set (ADR-004); the reserved runner-contract environment names above; and a raw image reference string — image selection is named-flavor-only, per the design in ADR-002.

## Rationale

TOML keeps this provider consistent with the rest of the `garm` provider ecosystem, which lowers the learning curve for operators who already run other GARM providers. Publishing and validating a JSON Schema for `extra_specs` is a stricter posture than most existing external providers take (see research.md §2 for the k8s-provider and `werdnum` provider's lack of schema validation) and is a direct, low-cost way to keep the trust-tier boundary enforced in code rather than only in documentation — a malformed or over-broad `extra_specs` payload is rejected before it can influence any privileged decision. Implementing the not-yet-invoked interface methods costs two embedded files and pays for itself in self-documentation, matching the practice already established by the LXD provider. The `allowed_dind_modes` ceiling and the reserved-env denylist are both instances of the same underlying principle: schema validation is where the operator/admin trust-tier boundary is actually enforced, so every field that could otherwise let the admin tier reach into operator-only territory (privilege mode, the runner's contractual environment) needs its own explicit bound in that same validation step, not just a description in a README.

## Alternatives considered

- **YAML or a `koanf`-based config loader** (the Kubernetes-ecosystem convention): rejected in favor of TOML for consistency with existing `garm` providers, which is a meaningful onboarding benefit for this ecosystem's operators.
- **No `extra_specs` schema validation** (the practice followed by both the k8s-provider and the `werdnum` provider): rejected — the LXD provider's published-schema practice is strictly better, and schema validation is exactly the mechanism that keeps the admin trust tier from silently expanding over time.
- **No ceiling on `extra_specs.dind_mode`** (trust the pool-level `extra_specs` allowlist alone): rejected — a per-pool `extra_specs` allowlist entry for `dind_mode` is not the same guarantee as an operator-controlled ceiling; without `allowed_dind_modes`, any GARM admin with pool-edit access could unilaterally turn on `privileged-sidecar` on a host the operator meant to keep at `none`.
- **No reserved-name denylist for extra_specs env** (trust the operator's own `environment_variables` allowlist to never include a dangerous name): rejected — an allowlist of *which* variables an admin may set is not the same thing as protecting the small set of names the *provider itself* depends on for correctness (`RUNNER_EPHEMERAL`, `DOCKER_HOST`, etc.); the denylist is a second, independent line of defense against operator misconfiguration of that allowlist, not a substitute for it.

## Consequences

- Two embedded schema files (provider config, `extra_specs`) must be kept in sync with their corresponding Go structs; documentation should be generated from the schemas rather than hand-maintained separately, to avoid drift.
- The `extra_specs` validator must know the full reserved-name denylist and the current `allowed_dind_modes` value at validation time, which couples schema validation to live provider config in a way the original two-embedded-files design didn't strictly require — an acceptable, necessary coupling given what these two checks protect.

## Open questions

- ~~Whether out-of-range `runner_memory`/`dind_memory` requests from `extra_specs` should be rejected outright or clamped to the configured maximum.~~ **Resolved (M3-W1, owner ruling): rejected, not clamped** — see the Amendment below.
- ~~Whether `storage_driver` should remain operator-only (config-only) rather than being on the `extra_specs` allowlist at all.~~ **Resolved (M3-W1): `storage_driver` IS on the `extra_specs` allowlist** (bounded to the `overlay2`/`vfs` enum) — see the Amendment below.
- The final shape of the `[flavors.*]` map, including whether flavors may inherit from one another.
- ~~The exact default value of `allowed_dind_modes` (currently all three modes, matching the previously-documented default `dind_mode` behavior) versus a more conservative out-of-the-box ceiling — see ADR-001's Open questions for the same trade-off framed from the residual-risk angle.~~ **Resolved (M4-W1, cross-family review F9): the ceiling defaults to `["none"]`, fail-closed** — see ADR-001's "fail-closed `allowed_dind_modes` default" Amendment for the reasoning, the two-step opt-in an operator now performs, and the companion pre-Docker `dind_image` check.
- Whether the reserved-env denylist should be operator-extensible (an allowlist of additional reserved names beyond the fixed provider-contract set) or kept fixed and provider-defined only, as specified above.

See research.md §1 for the `GARM_POOL_EXTRASPECS` environment-variable gap that governs where `extra_specs` must be read from, and §2 for the schema-validation practices of prior-art providers this ADR responds to. See ADR-001 for `dind_mode`/`allowed_dind_modes`/`storage_driver` semantics and the residual-risk framing behind the ceiling, ADR-002 for the flavor-map-only image-selection ruling and the full reserved runner-contract environment names, ADR-003 for the `[cache]` block fields including `allow_org_shared`, and ADR-004 for the `garm.docker/*` managed-label set that `extra_specs` can never touch.

## Amendment (2026-07-22) — M3-W1: extra_specs schema, validation, and v0.1.1 self-description implemented

This Decision is now implemented. The pieces landed in M3-W1:

- **`extra_specs` schema + validation** — `internal/extraspecs` embeds a published draft-07 JSON Schema (`schema.json`, `go:embed`) and validates every `CreateInstance`'s `extra_specs` against it with `xeipuuv/gojsonschema`, **failing closed** on any error, before any Docker operation (so a rejection is a `provider_fault` with zero partial allocation). The accepted allowlist is exactly this ADR's: `flavor`, `dind_mode`, `runner_memory`, `dind_memory`, `storage_driver`, `runner_labels`, `extra_env`. `additionalProperties: false` makes a raw image reference, `docker_host`, and the `privileged` flag **structurally absent** (rejected as unknown keys), not merely denied.
- **Ceiling/denylist/flavor reuse (not reinvented)** — `extraspecs.Resolve` bounds the payload through the EXISTING config machinery: `dind_mode` through `config.EffectiveDindMode(poolMode)` (this ADR's single ceiling enforcement point, with the pool's requested mode threaded in as `poolMode`, exactly as the Decision mandated); `flavor` through the `[flavors.*]` map + `EffectiveRunnerImage`; memory through `Effective{Runner,Dind}MemoryBytes` as the ceiling. There is no second, parallel ceiling check.
- **v0.1.1 self-description** — `internal/provider/v011.go` implements all four methods: `GetSupportedInterfaceVersions` → `["v0.1.0", "v0.1.1"]`; `GetConfigJSONSchema` / `GetExtraSpecsJSONSchema` return the two `go:embed`'ed schemas (config schema in `internal/config/schema.json`, exposed via `config.JSONSchema()`); `ValidatePoolInfo` runs the same `Parse`+`Resolve` path a create runs, so a GARM admin's `garm-cli pool update --extra-specs` fails early on exactly what a create would reject. `*Provider` now satisfies `executionv011.ExternalProvider`; the one binary serves v0.1.1 when `GARM_INTERFACE_VERSION=v0.1.1` and v0.1.0 by default (the top-level `execution` dispatch type-asserts against the versioned interface, and a v0.1.1 provider satisfies both).

**Open questions resolved:**

1. **Memory over-range: reject, not clamp (owner ruling).** A `runner_memory`/`dind_memory` request from `extra_specs` that exceeds the effective configured ceiling (flavor-resolved, or the `[resources]` default) is **rejected**, never silently clamped. A request equal to or below the ceiling is accepted; a ceiling of "unset" (unlimited) accepts any finite request. Rejecting is the honest posture — a pool asking for more than the operator allows should hear "no", not be quietly given less than it asked for and behave as if it succeeded.
2. **`storage_driver` is on the `extra_specs` allowlist**, bounded to the `overlay2`/`vfs` enum by the schema, defaulting to the configured `storage_driver` when omitted.

**Reserved-env denylist — reconciliation (stricter than originally worded).** The Decision above said an `extra_specs` env override that names a reserved key is "dropped (and logged), never silently accepted". M3-W1 implements this as a **fail-closed reject**: a reserved `extra_env` name (any `RUNNER_*`, `DOCKER_*`, `ACTIONS_RUNNER_INPUT_*` prefix, or `JIT_CONFIG_ENABLED`/`GITHUB_URL`, matched case-insensitively) causes the whole `extra_specs` to be rejected at parse time, rather than the single key being dropped. This is the more conservative reading and is consistent with the fail-closed stance the rest of validation takes. The "dropped (and logged), provider-injected value wins" behavior the Decision describes now covers the **residual** case: a *non*-reserved but still provider-injected name (e.g. `npm_config_store_dir`, `GARM_DIAG_DIR`) that an `extra_env` entry happens to collide with is dropped in favor of the provider's value at merge time — provider-injected environment always wins.

**Notes for implementers / operators:**

- The **flavor a pool selects is `extra_specs.flavor`**, not `BootstrapInstance.Flavor`; `ValidatePoolInfo` therefore ignores its positional `image`/`flavor` arguments (image selection is named-flavor-only per ADR-002) and validates the `extra_specs` payload. `extra_specs.runner_labels` are appended to the runner label set in **non-JIT** mode only (in JIT mode GARM bakes the label set into the runner config server-side, so they are inert there).
- The two schemas are **co-located with the structs they describe** (`internal/extraspecs/schema.json`, `internal/config/schema.json`) rather than in a single `internal/schema` package as plan.md §2 sketched, to keep each schema next to its struct and minimize the drift this ADR's Consequences warn about.
- The published `extra_specs` schema and an operator-facing summary of the allowlist, reserved denylist, and ceiling live in [docs/config-reference.md](../config-reference.md).

## Amendment (2026-07-22) — M3-W2: extra_env moves from a denylist to an operator allowlist + hard-reserve; memory positive-only; metadata token redaction

A cross-family security review of the `extra_specs` privilege channel found that the M3-W1 shape, while fail-closed on schema violations, still trusted `extra_specs` too much on three axes. `extra_specs` is a GARM-admin input channel, so each of these had to fail **closed**. This amendment supersedes the denylist-only text above where they conflict.

**extra_env is now an operator ALLOWLIST + a hard-reserved set (supersedes the "reserved-name denylist" model above).** The original design accepted any `extra_env` name that was not on a fixed reserved denylist — an accept-by-default posture. A pool could therefore set entrypoint/interpreter controls the denylist did not enumerate, e.g. `RUN_AS_ROOT=true` (the runner image entrypoint then runs registration **and the job** as **root**, defeating M1's non-root isolation) or a timeout var like `GARM_CRED_WAIT_SECONDS` that the entrypoint reads straight into Bash arithmetic `(( elapsed >= timeout ))`, where a value such as `a[$(cmd)]` is an arithmetic **command-substitution RCE as root**. The model is now two layered gates:

1. **Operator allowlist (`[extra_specs].allowed_env`, default EMPTY).** A new provider-config table lists the env NAMES a pool's `extra_specs.extra_env` may set. The default is empty, so **nothing** extra reaches the runner unless the operator opts a name in — fail-closed, consistent with the rest of `extra_specs`. A name not on the allowlist is **rejected** (not dropped), before any Docker op.
2. **Hard-reserved set — always rejected regardless of the allowlist.** A name the provider/entrypoint/interpreter relies on is rejected even if an operator mistakenly allowlists it. The set (case-insensitive) is the original runner-contract names — `RUNNER_*`, `DOCKER_*`, `ACTIONS_RUNNER_INPUT_*`, `JIT_CONFIG_ENABLED`, `GITHUB_URL` — **plus** the interpreter/entrypoint/privilege controls this review added: `RUN_AS_ROOT`, `GARM_*` (covering the entrypoint's `GARM_CRED_WAIT_SECONDS`/`GARM_EXTERNALS_WAIT_SECONDS` arithmetic reads), `WAIT_FOR_DOCKER_SECONDS` (the entrypoint's docker-wait arithmetic read), and the classic shell/loader hijack vectors `BASH_ENV`, `ENV`, `IFS`, `PATH`, `LD_*`, `NODE_OPTIONS`. The hard-reserved check runs in `extraspecs.Parse` (config-independent), so it wins before the allowlist gate in `Resolve` ever runs.

**Strict env-name charset (closes an exact-name-match bypass).** A key like `JIT_CONFIG_ENABLED=false` was emitted to Docker as `JIT_CONFIG_ENABLED=false=<value>`; Docker parses the NAME as `JIT_CONFIG_ENABLED`, flipping a reserved variable past the exact-name reserved check and the provider-wins merge. Every `extra_env` key must now match `^[A-Za-z_][A-Za-z0-9_]*$` (no `=`, whitespace, unicode, or leading digit), enforced both by the schema's `extra_env.propertyNames.pattern` and in Go **before** the reserved/allowlist checks and before any merge.

**Memory overrides are positive-only (0 in any representation rejected).** `runner_memory`/`dind_memory` of `"0"`, `"00"`, `"0GiB"`, `"0 B"` passed the old schema pattern and the ceiling comparison, but Docker treats a 0 memory limit as **unset (unlimited)** — so a pool could silently **remove** the operator's finite memory ceiling. The schema pattern is now `^[1-9][0-9]*…` (excludes any leading-zero value) and the Go bound (`config.ParsePositiveByteSize`, used by `extraspecs.boundMemory`) rejects a non-positive request. A memory override must be a genuine **positive** value ≤ the ceiling.

**Metadata errors are token-redacted (defense-in-depth for the bearer instance token).** A hostile or misbehaving metadata endpoint can reflect the bearer instance token into a redirect Location; net/http surfaces that token-bearing URL inside the transport/redirect error, which `main.go` logs to stderr. The metadata `Client` now scrubs `c.instanceToken` from **every** error it returns (`Client.redact`, wrapping the redirect-rejection and fetch-failure paths), so no metadata error — and thus no provider log line — can carry the raw token, regardless of what the endpoint echoes. This complements, and does not replace, the primary control (the token is kept out of the runner container's environment entirely, per ADR-002).

The `[extra_specs].allowed_env` field is documented in the config JSON Schema (`internal/config/schema.json`) and [docs/config-reference.md](../config-reference.md); its Go representation is `config.ExtraSpecsPolicy`.
