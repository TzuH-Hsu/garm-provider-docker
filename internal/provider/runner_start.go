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

// isIPv6SysctlUnsupported reports whether a ContainerCreate or ContainerStart
// error says the daemon could not apply spec.RunnerIPv6Sysctl, which happens
// when the host kernel has no IPv6 (ipv6.disable=1, IPv6 compiled out) and
// /proc/sys/net/ipv6 does not exist. The create succeeds and runc fails the
// start; Docker 29.1.3 with runc 1.3.4 on a kernel booted with ipv6.disable=1
// reports "OCI runtime create failed: runc create failed: unable to start
// container process: error during container init: open sysctl
// net.ipv6.conf.all.disable_ipv6 file: unsafe procfs detected: openat2
// fsmount:fscontext:proc/./sys/net/ipv6/conf/all/disable_ipv6: no such file
// or directory" (older runc: "write sysctl key net.ipv6.conf.all.disable_ipv6:
// open /proc/sys/net/ipv6/conf/all/disable_ipv6: ..."). The match is
// deliberately narrow: the error must name this sysctl, by key or by its path
// below /proc, and must not be a context cancellation or deadline, so no
// unrelated create or start failure is ever retried. A create-time rejection
// is matched the same way, for a daemon or runtime that checks earlier.
func isIPv6SysctlUnsupported(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, spec.RunnerIPv6Sysctl) || strings.Contains(msg, ipv6SysctlPath)
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
// volumes it references, and starts it. If the daemon rejects the IPv6 sysctl
// (a host without IPv6, see isIPv6SysctlUnsupported) at create or at start, it
// removes the container that attempt left behind and tries exactly once more
// without the sysctl: on such a host IPv6 cannot be enabled in the container
// anyway, so `localhost` is already IPv4-only. Any other failure is returned
// as is. On error the returned ID, when non-empty, is a container that still
// exists; the caller's nonce-keyed rollback removes it either way.
func (p *Provider) createAndStartRunner(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig, instanceName string, plan cachePlan) (string, error) {
	id, err := p.createAndStartRunnerOnce(ctx, cfg, hostCfg, instanceName, plan)
	if err == nil || !isIPv6SysctlUnsupported(err) || hostCfg.Sysctls[spec.RunnerIPv6Sysctl] == "" {
		return id, err
	}

	slog.WarnContext(ctx, "CreateInstance: the Docker host cannot apply the runner's IPv6 sysctl (no IPv6 in the host kernel); recreating the runner once without it, which leaves localhost IPv4-only on such a host",
		"instance", instanceName, "sysctl", spec.RunnerIPv6Sysctl, "error", err)
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
