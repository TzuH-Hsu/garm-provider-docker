package provider

import "github.com/cloudbase/garm-provider-common/params"

// mapContainerStatus maps a Docker container's state to GARM's
// InstanceStatus wire vocabulary (research.md §1.E), per the WP8 table:
//
//	running           -> running
//	exited            -> stopped
//	created           -> pending_create
//	dead / OOM-killed  -> error
//	anything else      -> unknown
//
// OOMKilled and Dead are checked first: a container killed for OOM often
// still reports its state string as "exited", but an OOM kill is an error,
// not a clean stop, so it must not be reported as "stopped".
func mapContainerStatus(state string, oomKilled, dead bool) params.InstanceStatus {
	if oomKilled || dead {
		return params.InstanceError
	}
	switch state {
	case "running":
		return params.InstanceRunning
	case "exited":
		return params.InstanceStopped
	case "created":
		return params.InstancePendingCreate
	default:
		return params.InstanceStatusUnknown
	}
}
