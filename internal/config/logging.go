package config

// Logging is the [logging] table: where the provider's log output goes in
// addition to stderr. GARM keeps a provider's stderr only when the call
// fails, so everything logged by a successful call is lost unless a file is
// configured here.
type Logging struct {
	// File is an absolute path the provider appends every log line to, in the
	// same text format and at the same level as stderr. Empty (the default)
	// disables the file entirely and stderr output is unchanged. The file is
	// opened per invocation with O_APPEND|O_CREATE and mode 0640; its
	// directory must already exist. An unusable path (relative, unwritable)
	// never fails a GARM call: the provider warns once on stderr and carries
	// on. See internal/logging.NewWithFile.
	File string `toml:"file"`
}
