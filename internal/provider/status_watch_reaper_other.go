//go:build !linux

package provider

// orphansAreReaped assumes orphans are reaped: outside Linux the provider is not
// expected to run in a container under a non-init PID 1.
func orphansAreReaped() (bool, string) { return true, "" }
