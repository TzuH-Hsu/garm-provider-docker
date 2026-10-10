//go:build !unix

package provider

import (
	"context"
	"errors"
)

// startDetachedWatcher is unsupported off unix (the provider ships for Linux
// only); CreateInstance then leaves the runner installing.
func startDetachedWatcher(context.Context, statusWatchRequest) error {
	return errors.New("the detached status watcher is only supported on unix")
}
