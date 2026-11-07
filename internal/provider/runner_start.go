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

// classifyIPv6SysctlError decides whether a create or start error is the daemon failing
// to apply spec.RunnerIPv6Sysctl, and why. The error must name the sysctl, by key or by
// its path below /proc, and it is ipv6SysctlMissing only if it also says "no such file
// or directory". Context cancellation and deadlines are ipv6SysctlNotRejected.
func classifyIPv6SysctlError(err error) ipv6SysctlRejection {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ipv6SysctlNotRejected
	}
	// On an ipv6.disable=1 kernel, Docker 29.1.3 with runc 1.3.4 fails the start with
	// "open sysctl net.ipv6.conf.all.disable_ipv6 file: unsafe procfs detected: openat2
	// fsmount:fscontext:proc/./sys/net/ipv6/conf/all/disable_ipv6: no such file or
	// directory". Older runc says "write sysctl key net.ipv6.conf.all.disable_ipv6:
	// open /proc/sys/net/ipv6/conf/all/disable_ipv6: no such file or directory".
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

// createAndStartRunner creates the runner, revalidates its cache volumes and starts it.
// If the daemon cannot apply the IPv6 sysctl, it logs a warning, removes the rejected
// container and tries once more without the sysctl: failing every allocation on such a
// host is worse than a runner without it (ADR-002, IPv6 is disabled in the runner). Any
// other failure is returned as is. On error a non-empty ID is a container that still
// exists; the caller's rollback removes it either way.
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

// createAndStartRunnerOnce is one create, revalidate and start attempt. Only a start
// failure returns a non-empty ID with an error: a container whose caches fail
// revalidation must never start, so it is removed first.
func (p *Provider) createAndStartRunnerOnce(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig, instanceName string, plan cachePlan) (string, error) {
	created, err := p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.RunnerContainerName(instanceName))
	if err != nil {
		// The caller's rollback removes any container the daemon recorded despite this
		// error.
		return "", fmt.Errorf("failed to create container for %q: %w", instanceName, err)
	}

	// The create pinned the volumes, and it would have recreated an externally removed
	// one empty and unlabelled. A retry without the IPv6 sysctl repeats this, because
	// the caches were unpinned between the two creates.
	if rerr := p.revalidateReferencedCaches(ctx, plan); rerr != nil {
		p.bestEffortRemoveContainer(ctx, created.ID)
		return "", rerr
	}

	// The entrypoint now blocks waiting for the delivery marker.
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return created.ID, fmt.Errorf("failed to start container for %q: %w", instanceName, err)
	}
	return created.ID, nil
}
