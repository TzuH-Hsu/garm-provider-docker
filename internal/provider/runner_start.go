package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// ipv6SysctlPath is spec.RunnerIPv6Sysctl as a path below /proc, the form a
// runtime names when it cannot open it ("sys/net/ipv6/conf/all/disable_ipv6").
var ipv6SysctlPath = "sys/" + strings.ReplaceAll(spec.RunnerIPv6Sysctl, ".", "/")

// ipv6SysctlRejection classifies a ContainerCreate/ContainerStart error with
// respect to spec.RunnerIPv6Sysctl.
type ipv6SysctlRejection int

const (
	// ipv6SysctlNotRejected: the error is not about the IPv6 sysctl (or there
	// is no error, or it is a context cancellation/deadline). Never retried.
	ipv6SysctlNotRejected ipv6SysctlRejection = iota
	// ipv6SysctlMissing: the sysctl does not exist because the host kernel has
	// no IPv6 (ipv6.disable=1, IPv6 built out). IPv6 cannot be enabled in the
	// runner either, so dropping the sysctl loses nothing.
	ipv6SysctlMissing
	// ipv6SysctlRefused: the sysctl exists or may exist but could not be
	// applied (permission denied, read-only /proc/sys, a runtime or security
	// policy). Dropping it can leave IPv6, and ::1, usable in the runner.
	ipv6SysctlRefused
)

// classifyIPv6SysctlError decides whether a create or start error is the
// daemon failing to apply spec.RunnerIPv6Sysctl, and why. The error must name
// the sysctl, by key or by its path below /proc, and must not be a context
// cancellation or deadline; anything else is ipv6SysctlNotRejected. A named
// error is ipv6SysctlMissing only when it also carries the ENOENT text "no
// such file or directory" (any case); every other reason is
// ipv6SysctlRefused.
//
// On a kernel booted with ipv6.disable=1, Docker 29.1.3 with runc 1.3.4
// creates the container and fails the start with "OCI runtime create failed:
// runc create failed: unable to start container process: error during
// container init: open sysctl net.ipv6.conf.all.disable_ipv6 file: unsafe
// procfs detected: openat2 fsmount:fscontext:proc/./sys/net/ipv6/conf/all/
// disable_ipv6: no such file or directory"; older runc says "write sysctl key
// net.ipv6.conf.all.disable_ipv6: open /proc/sys/net/ipv6/conf/all/
// disable_ipv6: no such file or directory". A create-time rejection is
// classified the same way, for a daemon or runtime that checks earlier.
func classifyIPv6SysctlError(err error) ipv6SysctlRejection {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ipv6SysctlNotRejected
	}
	msg := err.Error()
	if !strings.Contains(msg, spec.RunnerIPv6Sysctl) && !strings.Contains(msg, ipv6SysctlPath) {
		return ipv6SysctlNotRejected
	}
	if strings.Contains(strings.ToLower(msg), "no such file or directory") {
		return ipv6SysctlMissing
	}
	return ipv6SysctlRefused
}

// withoutIPv6Sysctl returns a copy of hostCfg without spec.RunnerIPv6Sysctl,
// leaving hostCfg itself untouched.
func withoutIPv6Sysctl(hostCfg *container.HostConfig) *container.HostConfig {
	out := *hostCfg
	out.Sysctls = nil
	for k, v := range hostCfg.Sysctls {
		if k == spec.RunnerIPv6Sysctl {
			continue
		}
		if out.Sysctls == nil {
			out.Sysctls = map[string]string{}
		}
		out.Sysctls[k] = v
	}
	return &out
}

// createAndStartRunner creates the runner container, revalidates the cache
// volumes it references, and starts it. If the daemon cannot apply the IPv6
// sysctl at create or at start (classifyIPv6SysctlError), it logs a warning,
// removes the container that attempt left behind, and tries exactly once more
// without the sysctl. Both classes are retried, because failing every
// allocation on such a host, with no knob to turn the sysctl off, is worse
// than running the runner without it; the warning says which case applies:
//
//   - ipv6SysctlMissing: the host kernel has no IPv6, so ::1 cannot exist in
//     the runner and `localhost` stays IPv4-only. Nothing is lost.
//   - ipv6SysctlRefused: the sysctl could not be applied for another reason.
//     IPv6 may stay enabled in the runner, so `localhost` can resolve to ::1
//     and a server bound to localhost may be unreachable via 127.0.0.1 (the
//     split ADR-002's IPv6 amendment describes). This degraded case is logged
//     with the daemon's error and flagged by the live checks.
//
// Any other failure is returned as is. On error the returned ID, when
// non-empty, is a container that still exists; the caller's nonce-keyed
// rollback removes it either way.
func (p *Provider) createAndStartRunner(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig, instanceName string, plan cachePlan) (string, error) {
	id, err := p.createAndStartRunnerOnce(ctx, cfg, hostCfg, instanceName, plan)
	if err == nil || hostCfg.Sysctls[spec.RunnerIPv6Sysctl] == "" {
		return id, err
	}
	switch classifyIPv6SysctlError(err) {
	case ipv6SysctlMissing:
		slog.WarnContext(ctx, "CreateInstance: the Docker host has no IPv6 (the runner's IPv6 sysctl does not exist); recreating the runner once without it. ::1 cannot exist in the runner, so localhost stays IPv4-only",
			"instance", instanceName, "sysctl", spec.RunnerIPv6Sysctl, "error", err)
	case ipv6SysctlRefused:
		slog.WarnContext(ctx, "CreateInstance: the Docker daemon could not apply the runner's IPv6 sysctl for a reason other than missing IPv6; recreating the runner once without it. IPv6 may remain enabled in the runner, so localhost can resolve to ::1 and a server bound to localhost may be unreachable via 127.0.0.1 (see ADR-002, the runner IPv6 amendment)",
			"instance", instanceName, "sysctl", spec.RunnerIPv6Sysctl, "error", err)
	default:
		return id, err
	}
	if id != "" {
		if rerr := p.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); rerr != nil && !errdefs.IsNotFound(rerr) {
			return id, errors.Join(err, fmt.Errorf("failed to remove runner container %s before retrying without the IPv6 sysctl: %w", id, rerr))
		}
	}
	return p.createAndStartRunnerOnce(ctx, cfg, withoutIPv6Sysctl(hostCfg), instanceName, plan)
}

// createAndStartRunnerOnce is one create → revalidate → start attempt. A
// container whose referenced caches fail revalidation is removed before the
// error is returned (it must never start), so only a start failure returns a
// non-empty ID with an error.
func (p *Provider) createAndStartRunnerOnce(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig, instanceName string, plan cachePlan) (string, error) {
	created, err := p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.RunnerContainerName(instanceName))
	if err != nil {
		// We hold the claim network exclusively, so a container-create failure
		// (including an ambiguous daemon leak of a nonce-tagged container) is
		// handled by the caller's nonce-keyed rollback: it removes any container
		// this attempt leaked along with our network and volume, and never
		// touches a stale/foreign container carrying a different nonce.
		return "", fmt.Errorf("failed to create container for %q: %w", instanceName, err)
	}

	// Revalidate the cache volumes the runner now references (ADR-003 W2
	// structural redesign). The externals volume was ensured+seeded, and the
	// toolcache/pnpm/diag volumes ensured, BEFORE this ContainerCreate. The
	// provider's own opportunistic GC never removes a cache volume itself — it
	// only DETECTS a stale/superseded one and LOGS the operator's manual purge
	// command (ADR-003's cache-GC-safety amendment); but an operator-run manual
	// `docker volume prune` (or some other external actor) could still remove a
	// still-current cache in that ensure→mount gap — before this container pins
	// it in use — and real Moby then AUTO-CREATES the missing named volume
	// UNLABELED during ContainerCreate, so the runner would mount an EMPTY
	// read-only externals tree (or an empty cache). Confirm every referenced
	// cache volume still carries our full identity; if any does not, remove the
	// just-created (not-yet-started) runner and FAIL the allocation CLOSED —
	// WITHOUT deleting the mismatched volume (it is left as cruft, never
	// name-deleted — a Moby auto-created unlabeled volume and a foreign one are
	// indistinguishable, B2). GARM's retry then reconciles AROUND the slot
	// (EnsureCacheVolume label-as-identity), so there is no wedge. A retry
	// without the IPv6 sysctl runs this again, since the caches were unpinned
	// between the two creates.
	if rerr := p.revalidateReferencedCaches(ctx, plan); rerr != nil {
		p.bestEffortRemoveContainer(ctx, created.ID)
		return "", rerr
	}

	// Start it; the entrypoint now blocks waiting for the delivery marker.
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return created.ID, fmt.Errorf("failed to start container for %q: %w", instanceName, err)
	}
	return created.ID, nil
}
