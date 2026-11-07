package config

import _ "embed"

// configSchemaJSON is the draft-07 JSON Schema for the TOML config, returned by
// the GetConfigJSONSchema command. It is documentation only: Load does not
// validate against it, so it is kept in sync with Config by review, and a test
// checks that it parses.
//
//go:embed schema.json
var configSchemaJSON string

// JSONSchema returns the provider-config JSON Schema.
func JSONSchema() string { return configSchemaJSON }
