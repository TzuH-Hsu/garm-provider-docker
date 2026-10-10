//go:build linux

package provider

import "os"

// readPID1Comm returns the name of PID 1 in this process's PID namespace.
var readPID1Comm = func() (string, error) {
	b, err := os.ReadFile("/proc/1/comm")
	return string(b), err
}

// orphansAreReaped reports whether a process orphaned by the provider (the
// detached watcher, once the provider exits) will be waited for, judged by
// PID 1 of this PID namespace, and returns PID 1's name for logging. A
// child-subreaper between the provider and PID 1 cannot be detected and would
// also reap; this check errs towards running in-process.
func orphansAreReaped() (bool, string) {
	comm, err := readPID1Comm()
	return reaperVerdict(comm, err), comm
}
