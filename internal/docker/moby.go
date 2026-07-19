package docker

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// credentialExecUser is the user the credential-delivery exec runs as: the
// runner user's numeric uid:gid (spec.RunnerUID/RunnerGID). Running the
// `tar -x` as the runner user means the extracted credential files are
// owned by that user, so it can read them (through the entrypoint's
// symlinks) after run.sh drops privileges to it (ADR-002 F1/F2). The
// credential tmpfs is mounted owned by the same uid/gid, so this
// unprivileged exec can write into it.
const credentialExecUser = "1001:1001"

// mobyClient wraps the real moby SDK *client.Client so that this package can
// add ExecStream, a higher-level operation the raw SDK does not expose as a
// single call. Every other Client method is satisfied by the embedded
// *client.Client unchanged.
type mobyClient struct {
	*client.Client
}

// NewMobyClient builds a Client backed by the real moby Docker SDK,
// talking to the daemon at dockerHost (config.Config.DockerHost) and
// negotiating the API version against whatever that daemon actually
// speaks, rather than hardcoding a version this provider was built
// against.
//
// This takes a plain string rather than a config.Config so that package
// docker has no dependency on package config; the caller (internal/
// provider, in a later WP) is the one that knows about config.Config.
func NewMobyClient(dockerHost string) (Client, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost(dockerHost),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client for host %q: %w", dockerHost, err)
	}
	return &mobyClient{Client: cli}, nil
}

// Compile-time assertion that the wrapped SDK client satisfies our narrow
// Client interface.
var _ Client = (*mobyClient)(nil)

// ExecStream implements the exec-based credential delivery documented on the
// Client interface: it creates an exec running cmd with stdin attached,
// streams stdin into it, drains the exec's output so it cannot block, and
// returns the exec's exit code from ExecInspect.
func (m *mobyClient) ExecStream(ctx context.Context, containerID string, cmd []string, stdin io.Reader) (int, error) {
	execResp, err := m.Client.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		User:         credentialExecUser,
		Cmd:          cmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to create exec in container %s: %w", containerID, err)
	}

	att, err := m.Client.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to attach to exec in container %s: %w", containerID, err)
	}
	defer att.Close()

	// Write stdin in a goroutine, then signal EOF via CloseWrite so the
	// container-side `tar -x` sees end-of-archive and exits, concurrently
	// with draining the exec's (multiplexed) output below.
	copyDone := make(chan error, 1)
	go func() {
		var werr error
		if stdin != nil {
			_, werr = io.Copy(att.Conn, stdin)
		}
		if cerr := att.CloseWrite(); werr == nil {
			werr = cerr
		}
		copyDone <- werr
	}()

	// Demultiplex and drain output so the exec cannot block on a full pipe.
	// StdCopy blocks until the exec closes its streams (i.e. the process
	// exits); the credential-delivery tar produces no stdout worth keeping.
	if _, err := stdcopy.StdCopy(io.Discard, io.Discard, att.Reader); err != nil {
		return 0, fmt.Errorf("failed to read exec output from container %s: %w", containerID, err)
	}
	if werr := <-copyDone; werr != nil {
		return 0, fmt.Errorf("failed to stream stdin to exec in container %s: %w", containerID, werr)
	}

	insp, err := m.Client.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return 0, fmt.Errorf("failed to inspect exec in container %s: %w", containerID, err)
	}
	if insp.Running {
		return 0, fmt.Errorf("exec in container %s still running after its output was drained", containerID)
	}
	return insp.ExitCode, nil
}
