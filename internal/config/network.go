package config

// Network is the [network] table (ADR-001/ADR-005): per-job bridge network
// behavior.
type Network struct {
	// EnableJobNetwork toggles per-job network creation. Defaults to true:
	// ADR-001 provisions a labeled bridge network for every allocation, in
	// every DinD mode.
	//
	// Neither ADR-001 nor plan.md specifies what disabling this actually
	// means operationally (no network resource at all, vs. falling back to
	// Docker's default bridge) — flagged in the WP1 report as an open
	// question WP2/WP3 must resolve before this field is consulted by any
	// code; it is parsed and validated here but not yet read anywhere.
	EnableJobNetwork bool `toml:"enable_job_network"`

	// Internal sets Docker's `internal` flag on every per-job network,
	// denying it a route to the external network beyond what the
	// runner/DinD containers explicitly need (ADR-001). Defaults to true:
	// defense-in-depth alongside the extra_specs reserved-env denylist
	// (ADR-005) and the structural "never mount the host socket" guarantee.
	// Operators who need broader job egress (e.g. package registry access)
	// can set this false; it is a config-only knob, never an extra_specs
	// one, consistent with ADR-001's operator-config-outranks-extra_specs
	// trust-tier ordering.
	//
	// This flag alone does not close every network exposure: ADR-001
	// separately recommends operators ensure the HOST Docker daemon itself
	// is never configured to listen on TCP (dockerd -H tcp://... or an
	// equivalent daemon.json hosts entry). A TCP-exposed host daemon socket
	// is a host-level exposure that bypasses every per-job containment
	// guarantee this provider creates — internal=true scopes only the
	// per-job bridge network's own egress and has no reach into that host-
	// level surface at all.
	Internal bool `toml:"internal"`
}
