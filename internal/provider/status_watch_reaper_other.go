//go:build !linux

package provider

// orphansAreReaped: outside Linux the provider is not expected to run in a
// container as a child of a non-init PID 1 (launchd, for instance, reaps), so
// orphans are assumed to be reaped.
func orphansAreReaped() (bool, string) { return true, "" }
