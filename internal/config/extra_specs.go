package config

// ExtraSpecsPolicy is the operator's [extra_specs] table (ADR-005 H2): the
// operator-owned controls over what the narrower GARM-admin extra_specs trust
// tier may do. Today it holds only the extra_env ALLOWLIST — the switch from a
// denylist-only model (which accepted any non-denylisted variable) to a
// fail-closed allowlist: nothing extra reaches the runner container unless the
// operator has explicitly opted the NAME in here.
type ExtraSpecsPolicy struct {
	// AllowedEnv is the operator's allowlist of environment-variable NAMES a
	// pool's extra_specs.extra_env may set on the runner container. It defaults
	// to EMPTY, which is fail-closed: with no allowlist, a pool cannot inject
	// ANY extra environment variable. A name that is not in this list is
	// rejected at CreateInstance/ValidatePoolInfo time (extraspecs.Resolve),
	// before any Docker operation.
	//
	// The allowlist NEVER widens the provider's hard-reserved set: names the
	// provider itself relies on for the runner contract or interpreter/entrypoint
	// control (RUNNER_*, DOCKER_*, GARM_*, RUN_AS_ROOT, PATH, LD_*, ... — see
	// extraspecs.reservedEnvName) are rejected even if an operator mistakenly
	// lists them here. The hard-reserved check runs first (in extraspecs.Parse),
	// so listing a reserved name here can never make it settable.
	AllowedEnv []string `toml:"allowed_env"`
}

// EnvAllowed reports whether the operator has allowlisted the env-var name for
// extra_specs.extra_env. Matching is EXACT and case-sensitive: environment
// variable names are case-sensitive, and an operator lists exactly the names
// they intend. An empty allowlist allows nothing (fail-closed).
func (p ExtraSpecsPolicy) EnvAllowed(name string) bool {
	for _, allowed := range p.AllowedEnv {
		if allowed == name {
			return true
		}
	}
	return false
}
