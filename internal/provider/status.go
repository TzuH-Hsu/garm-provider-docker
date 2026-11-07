package provider

import "github.com/cloudbase/garm-provider-common/params"

// mapContainerStatus maps a Docker container state to GARM's InstanceStatus
// (docs/garm-contract.md). OOMKilled and dead are checked first: an OOM-killed
// container often still reports "exited", and that is an error, not a clean stop.
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
