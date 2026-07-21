package config

// Network is the [network] table (ADR-001/ADR-005): per-job bridge network
// behavior.
type Network struct {
	// EnableJobNetwork toggles per-job network creation. RESERVED and not
	// currently honored: the per-allocation job network is ALWAYS created,
	// in every DinD mode, regardless of this setting, because it is both
	// the per-job isolation guarantee (ADR-001; see Internal below for why
	// isolation lives here and not in the internal flag) and ADR-004's
	// atomic claim marker — a provider that skipped it on request would be
	// skipping the mechanism duplicate-create detection depends on. Setting
	// this false does NOT skip network creation: internal/provider/
	// create.go logs a warning and creates the isolated network anyway.
	// Defaults to true.
	//
	// Neither ADR-001 nor plan.md specifies what disabling this would mean
	// operationally if it were ever actually wired up (no network resource
	// at all, vs. falling back to Docker's default bridge) — flagged in the
	// WP1 report as an open question; WP2 resolved the INTERIM behavior
	// documented above (always-on, warn-only) so the field is not an
	// undocumented gap, but the "what would disabling it even mean" design
	// question remains open for a future milestone.
	EnableJobNetwork bool `toml:"enable_job_network"`

	// Internal sets Docker's `internal` flag on every per-job network. When
	// true, Docker gives that network NO route to the external network at
	// all — not reduced egress, none — which also cuts off the runner's own
	// GitHub registration and action/dependency downloads, and any DinD
	// sidecar's registry pulls. Defaults to FALSE as of the 2026-07-21
	// owner ruling (see ADR-001's Amendment): real-daemon testing showed
	// the previous internal=true default made the provider non-functional
	// for real jobs, and per-job isolation does not actually depend on this
	// flag — it comes structurally from every allocation getting its own
	// separate network (ADR-001's Decision), which internal=false does not
	// change at all. A job on network A cannot reach a job on network B by
	// IP regardless of either allocation's `internal` setting.
	//
	// This field is retained, not removed: an operator who wants a fully
	// airgapped job (no egress whatsoever, e.g. paired with some other,
	// out-of-band package mirror) can still set this true, and it remains
	// the building block for a RESERVED, not-yet-implemented future
	// opt-in — an internal job network plus a forward-proxy sidecar that
	// restricts the runner to an allowlist of GitHub/registry endpoints via
	// HTTP(S)_PROXY — for advanced/enterprise deployments that want
	// stricter egress control than "on or off" (see ADR-001's Amendment).
	// It is a config-only knob, never an extra_specs one, consistent with
	// ADR-001's operator-config-outranks-extra_specs trust-tier ordering.
	//
	// This flag was never the only layer, and matters even less as a
	// containment boundary now that it defaults off: ADR-001 requires
	// operators ensure the HOST Docker daemon itself is never configured to
	// listen on TCP (dockerd -H tcp://... or an equivalent daemon.json
	// hosts entry) — a TCP-exposed host daemon socket is a host-level
	// exposure this flag, at either setting, has no reach into. F8 (this
	// provider never lets extra_specs or the workflow payload override
	// DOCKER_HOST — ADR-005) plus that host-level "no TCP listener"
	// operational requirement are what actually mitigate that concern now;
	// see ADR-001's Amendment for the full reasoning.
	Internal bool `toml:"internal"`
}
