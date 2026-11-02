package docker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
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
// returns the exec's exit code from ExecInspect. The actual stream pumping —
// including honoring ctx cancellation by force-closing the hijacked
// connection — is delegated to streamExec so it is testable without a Docker
// daemon.
func (m *mobyClient) ExecStream(ctx context.Context, containerID string, cmd []string, stdin io.Reader) (int, error) {
	return m.runExec(ctx, containerID, container.ExecOptions{
		User:         credentialExecUser,
		Cmd:          cmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}, stdin, io.Discard)
}

// execOutputLimit caps what ExecOutput keeps of an exec's output. The only
// caller prints a few dozen bytes per image, so the cap is far above any real
// answer; it bounds memory against a runaway command.
const execOutputLimit = 256 << 10

// ExecOutput implements the output-capturing exec documented on the Client
// interface: the same create/attach/drain/inspect sequence as ExecStream, with
// no stdin, the container's default user, and stdout+stderr collected into a
// capped buffer instead of discarded.
func (m *mobyClient) ExecOutput(ctx context.Context, containerID string, cmd []string) (string, int, error) {
	buf := &cappedBuffer{limit: execOutputLimit}
	code, err := m.runExec(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}, nil, buf)
	if err != nil {
		return "", 0, err
	}
	if buf.overflowed {
		return "", 0, fmt.Errorf("exec output from container %s exceeded %d bytes", containerID, execOutputLimit)
	}
	return buf.b.String(), code, nil
}

// cappedBuffer collects up to limit bytes and records, rather than errors on,
// any excess, so the exec output pump never blocks or fails mid-stream.
type cappedBuffer struct {
	b          strings.Builder
	limit      int
	overflowed bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.limit - c.b.Len(); n > room {
		c.overflowed = true
		p = p[:room]
	}
	c.b.Write(p)
	return n, nil
}

// runExec creates an exec with opts, attaches, pumps stdin (if any) and writes
// the exec's output to stdout, and returns the exit code from ExecInspect. The
// actual stream pumping — including honoring ctx cancellation by force-closing
// the hijacked connection — is delegated to streamExecTo.
func (m *mobyClient) runExec(ctx context.Context, containerID string, opts container.ExecOptions, stdin io.Reader, stdout io.Writer) (int, error) {
	execResp, err := m.Client.ContainerExecCreate(ctx, containerID, opts)
	if err != nil {
		return 0, fmt.Errorf("failed to create exec in container %s: %w", containerID, err)
	}

	att, err := m.Client.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to attach to exec in container %s: %w", containerID, err)
	}

	// streamExecTo owns closing the hijacked connection (on success, error, or
	// cancellation), so there is no separate defer att.Close() here.
	if err := streamExecTo(ctx, containerID, sdkHijack{att: att}, stdin, stdout); err != nil {
		return 0, err
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

// execConn is the slice of docker's types.HijackedResponse that streamExec
// drives. The production path adapts a real HijackedResponse (sdkHijack);
// tests supply a net.Pipe-backed fake to exercise the cancellation and
// leak-freedom behavior without a Docker daemon (NEW-3).
type execConn interface {
	// Write feeds bytes to the exec's stdin (the hijacked net.Conn).
	io.Writer
	// output is the exec's multiplexed stdout+stderr stream (stdcopy-framed).
	output() io.Reader
	// CloseWrite half-closes stdin so the remote `tar -x` sees end-of-archive.
	CloseWrite() error
	// Close force-closes the whole connection, unblocking both directions.
	Close()
}

// sdkHijack adapts a moby types.HijackedResponse to execConn.
type sdkHijack struct {
	att types.HijackedResponse
}

func (s sdkHijack) Write(p []byte) (int, error) { return s.att.Conn.Write(p) }
func (s sdkHijack) output() io.Reader           { return s.att.Reader }
func (s sdkHijack) CloseWrite() error           { return s.att.CloseWrite() }
func (s sdkHijack) Close()                      { s.att.Close() }

// streamExec pumps stdin into an attached exec's connection, drains its
// multiplexed output so the exec cannot block on a full pipe, and — critically
// — watches ctx: on cancellation it force-closes the hijacked connection so
// both the stdin copy goroutine and the output drain unblock instead of
// hanging (NEW-3). It returns ctx.Err() when cancellation caused the failure,
// and leaks no goroutine or fd on any path (success, exec error, or cancel):
// the watcher is always stopped via the stop channel, and the connection is
// always closed exactly once via a sync.Once.
//
// Contract on stdin: it must be a finite, in-memory reader whose Read never
// blocks (e.g. the *bytes.Buffer ExecStream's only caller, provider.
// CreateInstance, builds from the credential tar). Force-closing conn on ctx
// cancellation unblocks the StdCopy drain and any pending Write, but it does
// NOT unblock a concurrent stdin.Read() — that call is independent of conn.
// A reader that can block indefinitely (e.g. a live network stream) would
// leave the `werr := <-copyDone` receive below hung on that Read even after
// cancellation, defeating the cancellation guarantee this function otherwise
// provides. This is documentation of an existing invariant every current
// caller already satisfies, not a behavior change.
func streamExec(ctx context.Context, containerID string, conn execConn, stdin io.Reader) error {
	return streamExecTo(ctx, containerID, conn, stdin, io.Discard)
}

// streamExecTo is streamExec with the exec's demultiplexed output (stdout and
// stderr, interleaved) written to stdout instead of discarded.
func streamExecTo(ctx context.Context, containerID string, conn execConn, stdin io.Reader, stdout io.Writer) error {
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(conn.Close) }
	defer closeConn()

	// Watch ctx. On cancellation, force-close the connection to unblock the
	// StdCopy drain and the stdin copy below. On any normal return, the
	// deferred close(stop) tears the watcher down so it never leaks.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			closeConn()
		case <-stop:
		}
	}()

	// Write stdin in a goroutine, then signal EOF via CloseWrite so the
	// container-side `tar -x` sees end-of-archive and exits, concurrently
	// with draining the exec's (multiplexed) output below. The channel is
	// buffered so this goroutine can always send and exit even if a
	// cancellation means nobody is left to read it.
	copyDone := make(chan error, 1)
	go func() {
		var werr error
		if stdin != nil {
			_, werr = io.Copy(conn, stdin)
		}
		if cerr := conn.CloseWrite(); werr == nil {
			werr = cerr
		}
		copyDone <- werr
	}()

	// StdCopy blocks until the exec closes its streams (i.e. the process
	// exits) or the connection is force-closed; the credential-delivery tar
	// produces no stdout worth keeping, so ExecStream passes io.Discard.
	_, copyErr := stdcopy.StdCopy(stdout, stdout, conn.output())
	werr := <-copyDone

	// A cancellation is what unblocked the pumps above (via the force-close),
	// so surface ctx.Err() rather than the resulting "use of closed
	// connection" noise from StdCopy / the stdin copy.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("exec in container %s canceled: %w", containerID, ctxErr)
	}
	if copyErr != nil {
		return fmt.Errorf("failed to read exec output from container %s: %w", containerID, copyErr)
	}
	if werr != nil {
		return fmt.Errorf("failed to stream stdin to exec in container %s: %w", containerID, werr)
	}
	return nil
}
