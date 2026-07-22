package config

import _ "embed"

// configSchemaJSON is the published draft-07 JSON Schema describing this
// provider's TOML configuration (ADR-005). It is embedded so the v0.1.1
// self-description command GetConfigJSONSchema can return the provider's own
// config contract in machine-readable form, matching the published-schema
// practice of the LXD provider. It is documentation-grade: the loader (Load)
// tolerates unknown TOML keys and does not validate the file against this
// schema, so the schema is kept in sync with the Config struct by review, and a
// config_test asserts it stays valid, parseable JSON.
//
//go:embed schema.json
var configSchemaJSON string

// JSONSchema returns the raw provider-config JSON Schema bytes.
func JSONSchema() string { return configSchemaJSON }
