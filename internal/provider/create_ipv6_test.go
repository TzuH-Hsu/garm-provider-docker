package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// runnerCreates returns the recorded ContainerCreate calls for the runner of
// instanceName, in order (a create the daemon rejected is not recorded).
func runnerCreates(fake *docker.FakeClient, instanceName string) []docker.CreatedContainer {
	var out []docker.CreatedContainer
	for _, c := range fake.Created {
		if c.Name == spec.RunnerContainerName(instanceName) {
			out = append(out, c)
		}
	}
	return out
}

const (
	warnHostHasNoIPv6 = "the Docker host has no IPv6"
	warnIPv6MayRemain = "IPv6 may remain enabled in the runner"
)

func TestCreateInstanceRunnerGetsIPv6Sysctl(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance returned unexpected error: %v", err)
	}

	creates := runnerCreates(fake, inst.Name)
	if len(creates) != 1 {
		t.Fatalf("runner creates = %d, want 1 (no retry on a host with IPv6)", len(creates))
	}
	if got := creates[0].Sysctls[spec.RunnerIPv6Sysctl]; got != "1" {
		t.Errorf("runner Sysctls[%s] = %q, want 1", spec.RunnerIPv6Sysctl, got)
	}
	if len(fake.RemoveOrder) != 0 {
		t.Errorf("RemoveOrder = %v, want nothing removed", fake.RemoveOrder)
	}
}

// A host without IPv6 rejects the sysctl when runc applies it, at start: the
// runner is removed and recreated once without the sysctl, and everything
// after the start (delivery, running check) uses the second container.
func TestCreateInstanceRetriesWithoutIPv6SysctlWhenStartRejectsIt(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.HostLacksIPv6 = true
	logs := captureLogs(t)

	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance returned unexpected error: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, warnHostHasNoIPv6) || strings.Contains(out, warnIPv6MayRemain) {
		t.Errorf("want the host-has-no-IPv6 warning only, got logs:\n%s", out)
	}

	creates := runnerCreates(fake, inst.Name)
	if len(creates) != 2 {
		t.Fatalf("runner creates = %d, want 2 (one retry)", len(creates))
	}
	if got := creates[0].Sysctls[spec.RunnerIPv6Sysctl]; got != "1" {
		t.Errorf("first runner create Sysctls[%s] = %q, want 1", spec.RunnerIPv6Sysctl, got)
	}
	if _, ok := creates[1].Sysctls[spec.RunnerIPv6Sysctl]; ok {
		t.Errorf("retried runner create still carries %s: %v", spec.RunnerIPv6Sysctl, creates[1].Sysctls)
	}
	nonce := creates[0].Labels[spec.LabelCreateNonce]
	if nonce == "" || creates[1].Labels[spec.LabelCreateNonce] != nonce {
		t.Errorf("retried runner create-nonce = %q, want the attempt's nonce %q", creates[1].Labels[spec.LabelCreateNonce], nonce)
	}

	runner := inspectRunner(t, fake, inst.Name)
	if !runner.State.Running {
		t.Errorf("runner state = %q, want running", runner.State.Status)
	}
	// Only the retried runner exists: the rejected one was removed, and it is
	// the only thing that was removed.
	if n := listAll(t, p); n != 1 {
		t.Errorf("containers = %d, want 1 (the rejected runner must be removed)", n)
	}
	if len(fake.RemoveOrder) != 1 || !strings.HasPrefix(fake.RemoveOrder[0], "container:") || fake.RemoveOrder[0] == "container:"+runner.ID {
		t.Errorf("RemoveOrder = %v, want exactly the first runner container removed", fake.RemoveOrder)
	}
	if len(fake.Execs) != 1 || fake.Execs[0].ContainerID != runner.ID {
		t.Errorf("credential delivery execs = %+v, want one to the retried runner %s", fake.Execs, runner.ID)
	}
}

// A daemon that validates the sysctl at create rejects it before anything is
// recorded: retried once without it, nothing to remove.
func TestCreateInstanceRetriesWithoutIPv6SysctlWhenCreateRejectsIt(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.HostLacksIPv6 = true
	fake.IPv6SysctlFailsAtCreate = true

	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance returned unexpected error: %v", err)
	}
	creates := runnerCreates(fake, inst.Name)
	if len(creates) != 1 {
		t.Fatalf("recorded runner creates = %d, want 1 (the rejected create records nothing)", len(creates))
	}
	if _, ok := creates[0].Sysctls[spec.RunnerIPv6Sysctl]; ok {
		t.Errorf("retried runner create still carries %s", spec.RunnerIPv6Sysctl)
	}
	if !inspectRunner(t, fake, inst.Name).State.Running {
		t.Error("runner is not running after the retry")
	}
	if len(fake.RemoveOrder) != 0 {
		t.Errorf("RemoveOrder = %v, want nothing removed", fake.RemoveOrder)
	}
}

// A sysctl that exists but may not be written (permission denied) is also
// retried once without it, but the warning names the degraded case: IPv6 may
// stay enabled in the runner.
func TestCreateInstanceRetriesWithoutIPv6SysctlWhenDeniedAndWarnsDegraded(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.IPv6SysctlDenied = true
	logs := captureLogs(t)

	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance returned unexpected error: %v", err)
	}
	out := logs.String()
	for _, want := range []string{warnIPv6MayRemain, "localhost can resolve to ::1", "ADR-002", "permission denied"} {
		if !strings.Contains(out, want) {
			t.Errorf("degraded warning lacks %q; logs:\n%s", want, out)
		}
	}
	if strings.Contains(out, warnHostHasNoIPv6) {
		t.Errorf("a refused sysctl must not be logged as a host without IPv6; logs:\n%s", out)
	}

	creates := runnerCreates(fake, inst.Name)
	if len(creates) != 2 {
		t.Fatalf("runner creates = %d, want 2 (one retry)", len(creates))
	}
	if _, ok := creates[1].Sysctls[spec.RunnerIPv6Sysctl]; ok {
		t.Errorf("retried runner create still carries %s", spec.RunnerIPv6Sysctl)
	}
	if !inspectRunner(t, fake, inst.Name).State.Running {
		t.Error("runner is not running after the retry")
	}
	if n := listAll(t, p); n != 1 {
		t.Errorf("containers = %d, want 1 (the rejected runner must be removed)", n)
	}
}

// An unrelated start failure is not retried: one runner create, and the
// creation guard rolls the allocation back.
func TestCreateInstanceDoesNotRetryUnrelatedStartFailure(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.StartErr = errors.New("failed to start container: OCI runtime create failed: unable to apply cgroup configuration")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on start, got nil")
	}
	if n := len(runnerCreates(fake, "Test-Instance-01")); n != 1 {
		t.Errorf("runner creates = %d, want 1 (no retry for an unrelated start error)", n)
	}
	assertNoLeftovers(t, fake)
}

// The retry happens at most once: if the retried container is rejected with
// the same error, the create fails and is rolled back.
func TestCreateInstanceIPv6SysctlRetryHappensAtMostOnce(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.StartErr = docker.IPv6SysctlUnsupportedError(spec.RunnerIPv6Sysctl)

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail, got nil")
	}
	if n := len(runnerCreates(fake, "Test-Instance-01")); n != 2 {
		t.Errorf("runner creates = %d, want 2 (the first attempt and one retry)", n)
	}
	if len(fake.Execs) != 0 {
		t.Errorf("credentials delivered despite the failed start: %d execs", len(fake.Execs))
	}
	assertNoLeftovers(t, fake)
}

// When the rejected runner cannot be removed, the create fails rather than
// colliding on the runner's name.
func TestCreateInstanceIPv6SysctlRetryStopsWhenRemovalFails(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.HostLacksIPv6 = true
	removeErr := errors.New("removal of container is already in progress")
	fake.RemoveErr = removeErr

	_, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err == nil {
		t.Fatal("expected CreateInstance to fail, got nil")
	}
	if !errors.Is(err, removeErr) {
		t.Errorf("error %v does not carry the removal failure", err)
	}
	if n := len(runnerCreates(fake, "Test-Instance-01")); n != 1 {
		t.Errorf("runner creates = %d, want 1 (no retry without removing the first)", n)
	}
}

func TestClassifyIPv6SysctlError(t *testing.T) {
	const key = "net.ipv6.conf.all.disable_ipv6"
	cases := []struct {
		name string
		err  error
		want ipv6SysctlRejection
	}{
		{"nil", nil, ipv6SysctlNotRejected},
		{"runc 1.3.4, no IPv6 (Docker 29.1.3, ipv6.disable=1)", errors.New("Error response from daemon: failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: error during container init: open sysctl net.ipv6.conf.all.disable_ipv6 file: unsafe procfs detected: openat2 fsmount:fscontext:proc/./sys/net/ipv6/conf/all/disable_ipv6: no such file or directory"), ipv6SysctlMissing},
		{"fake no-IPv6 host", docker.IPv6SysctlUnsupportedError(key), ipv6SysctlMissing},
		{"older runc, no IPv6", errors.New("OCI runtime create failed: container_linux.go:380: starting container process caused: process_linux.go:545: container init caused: write sysctl key net.ipv6.conf.all.disable_ipv6: open /proc/sys/net/ipv6/conf/all/disable_ipv6: no such file or directory: unknown"), ipv6SysctlMissing},
		{"path only, capitalized ENOENT", errors.New("OCI runtime error: open `/proc/sys/net/ipv6/conf/all/disable_ipv6`: No such file or directory"), ipv6SysctlMissing},
		{"permission denied", docker.IPv6SysctlDeniedError(key), ipv6SysctlRefused},
		{"older runc, permission denied", errors.New("write sysctl key net.ipv6.conf.all.disable_ipv6: open /proc/sys/net/ipv6/conf/all/disable_ipv6: permission denied"), ipv6SysctlRefused},
		{"read-only file system", errors.New("open sysctl net.ipv6.conf.all.disable_ipv6 file: reason: open /proc/sys/net/ipv6/conf/all/disable_ipv6: read-only file system"), ipv6SysctlRefused},
		{"operation not permitted", errors.New("write sysctl key net.ipv6.conf.all.disable_ipv6: operation not permitted"), ipv6SysctlRefused},
		{"policy rejection", errors.New("sysctl \"net.ipv6.conf.all.disable_ipv6\" is not allowed by the runtime policy"), ipv6SysctlRefused},
		{"unrelated start error", errors.New("failed to start container: network job-net not found"), ipv6SysctlNotRejected},
		{"unrelated ENOENT", errors.New("failed to start container: open /etc/foo: no such file or directory"), ipv6SysctlNotRejected},
		{"other sysctl", errors.New("open sysctl net.ipv4.ip_unprivileged_port_start file: no such file or directory"), ipv6SysctlNotRejected},
		{"default key only", errors.New("open sysctl net.ipv6.conf.default.disable_ipv6 file: no such file or directory"), ipv6SysctlNotRejected},
		{"canceled while starting", fmt.Errorf("starting with %s: no such file or directory: %w", key, context.Canceled), ipv6SysctlNotRejected},
		{"deadline while starting", fmt.Errorf("starting with %s: %w", key, context.DeadlineExceeded), ipv6SysctlNotRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyIPv6SysctlError(tc.err); got != tc.want {
				t.Errorf("classifyIPv6SysctlError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestWithoutIPv6SysctlLeavesInputUntouched(t *testing.T) {
	_, in := spec.BuildRunnerContainer(spec.RunnerContainerSpec{Image: "img", NetworkName: "net", WorkspaceVolumeName: "ws"})
	in.Sysctls["net.core.somaxconn"] = "1024"

	out := withoutIPv6Sysctl(in)
	if _, ok := out.Sysctls[spec.RunnerIPv6Sysctl]; ok {
		t.Errorf("copy still carries %s", spec.RunnerIPv6Sysctl)
	}
	if out.Sysctls["net.core.somaxconn"] != "1024" {
		t.Errorf("copy dropped an unrelated sysctl: %v", out.Sysctls)
	}
	if in.Sysctls[spec.RunnerIPv6Sysctl] != "1" {
		t.Errorf("input was modified: %v", in.Sysctls)
	}
	if len(out.Mounts) != len(in.Mounts) || len(out.Tmpfs) != len(in.Tmpfs) {
		t.Error("copy lost mounts or tmpfs")
	}
}
