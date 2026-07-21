# ADR-005: Config and extra_specs Schema

Status: Accepted (2026-07-19)

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
dind_mode = "privileged-sidecar"  # none | privileged-sidecar | sysbox-runc — the operator's own default/fallback
allowed_dind_modes = ["none", "privileged-sidecar", "sysbox-runc"]  # ceiling on what extra_specs may select (ADR-001)
storage_driver = "overlay2"      # or "vfs"
enable_runner_callbacks = false

[resources]
runner_memory = "8GiB"
dind_memory = "4GiB"
# No cpu or pids limits by design — see Rationale.

[cache]
enabled = true
generation = "1"
pnpm_major = "9"
toolcache_path = "/opt/hostedtoolcache"
# pnpm_store_path is TBD — see ADR-003 open questions.
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

**`dind_mode` is bounded by the operator's `allowed_dind_modes` ceiling (see ADR-001 for rationale).** Provider config defines `allowed_dind_modes`, a list defaulting to all three modes. Every `CreateInstance` call's `extra_specs.dind_mode`, whatever a pool requests, is validated against this list at the same JSON-Schema-validation step described above; a request for a mode outside `allowed_dind_modes` is rejected exactly like any other schema violation — a pool cannot escalate to a mode the operator has not allowed, full stop. On a shared or security-sensitive host, an operator can set `allowed_dind_modes = ["none"]` to forbid privileged workloads outright, regardless of what any pool's `extra_specs` requests.

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

- Whether out-of-range `runner_memory`/`dind_memory` requests from `extra_specs` should be rejected outright or clamped to the configured maximum.
- Whether `storage_driver` should remain operator-only (config-only) rather than being on the `extra_specs` allowlist at all.
- The final shape of the `[flavors.*]` map, including whether flavors may inherit from one another.
- The exact default value of `allowed_dind_modes` (currently all three modes, matching the previously-documented default `dind_mode` behavior) versus a more conservative out-of-the-box ceiling — see ADR-001's Open questions for the same trade-off framed from the residual-risk angle.
- Whether the reserved-env denylist should be operator-extensible (an allowlist of additional reserved names beyond the fixed provider-contract set) or kept fixed and provider-defined only, as specified above.

See research.md §1 for the `GARM_POOL_EXTRASPECS` environment-variable gap that governs where `extra_specs` must be read from, and §2 for the schema-validation practices of prior-art providers this ADR responds to. See ADR-001 for `dind_mode`/`allowed_dind_modes`/`storage_driver` semantics and the residual-risk framing behind the ceiling, ADR-002 for the flavor-map-only image-selection ruling and the full reserved runner-contract environment names, ADR-003 for the `[cache]` block fields including `allow_org_shared`, and ADR-004 for the `garm.docker/*` managed-label set that `extra_specs` can never touch.
