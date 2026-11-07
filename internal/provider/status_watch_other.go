//go:build !unix

package provider

import (
	"context"
	"errors"
)

// startDetachedWatcher is unsupported off unix, since the provider ships for Linux
// only. CreateInstance then logs a warning and GARM keeps the runner pending.
func startDetachedWatcher(context.Context, statusWatchRequest) error {
	return errors.New("the detached status watcher is only supported on unix")
}
