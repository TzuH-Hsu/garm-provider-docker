package config

// Logging is the [logging] table. GARM keeps a provider's stderr only when the
// call fails, so a log file is the only record of a successful call.
type Logging struct {
	// File is an absolute path the provider appends every log line to, in
	// addition to stderr. Empty disables it. An unusable path never fails a
	// GARM call. See internal/logging.NewWithFile.
	File string `toml:"file"`
}
