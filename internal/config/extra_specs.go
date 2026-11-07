package config

// ExtraSpecsPolicy is the [extra_specs] table: the operator's controls over what
// a pool's extra_specs may set.
type ExtraSpecsPolicy struct {
	// AllowedEnv lists the environment variable names a pool's
	// extra_specs.extra_env may set. It is empty by default, so a pool can set
	// none. It never widens the hard-reserved names in internal/extraspecs,
	// which are rejected first even when listed here.
	AllowedEnv []string `toml:"allowed_env"`
}

// EnvAllowed reports whether name is in AllowedEnv. Matching is exact and
// case-sensitive, as environment variable names are.
func (p ExtraSpecsPolicy) EnvAllowed(name string) bool {
	for _, allowed := range p.AllowedEnv {
		if allowed == name {
			return true
		}
	}
	return false
}
