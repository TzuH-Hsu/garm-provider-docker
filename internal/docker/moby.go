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

// credentialExecUser is the runner user's uid:gid (spec.RunnerUID/RunnerGID). Running
// tar as that user makes the extracted credential files readable by the runner after
// run.sh drops privileges (ADR-002). The credential tmpfs is mounted with the same
// uid and gid, so this unprivileged exec can write to it.
const credentialExecUser = "1001:1001"

// mobyClient adds ExecStream and ExecOutput, which the SDK does not offer as single
// calls, to the SDK client. The embedded *client.Client satisfies the rest of Client.
type mobyClient struct {
	*client.Client
}

// NewMobyClient returns a Client for the daemon at dockerHost, negotiating the API
// version with that daemon.
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

var _ Client = (*mobyClient)(nil)

// ExecStream implements Client.ExecStream. The exec runs as credentialExecUser and its
// output is discarded.
func (m *mobyClient) ExecStream(ctx context.Context, containerID string, cmd []string, stdin io.Reader) (int, error) {
	return m.runExec(ctx, containerID, container.ExecOptions{
		User:         credentialExecUser,
		Cmd:          cmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}, stdin, io.Discard)
}

// execOutputLimit caps what ExecOutput keeps of an exec's output. It is far above any
// real answer and bounds memory against a runaway command.
const execOutputLimit = 256 << 10

// ExecOutput implements Client.ExecOutput. It fails if the output exceeds
// execOutputLimit.
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

// cappedBuffer keeps up to limit bytes and records any excess instead of failing, so
// the output pump never stops mid-stream.
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

// runExec creates and attaches an exec, pumps stdin (if any) in and its output to
// stdout, and returns the exit code from ExecInspect.
func (m *mobyClient) runExec(ctx context.Context, containerID string, opts container.ExecOptions, stdin io.Reader, stdout io.Writer) (int, error) {
	execResp, err := m.Client.ContainerExecCreate(ctx, containerID, opts)
	if err != nil {
		return 0, fmt.Errorf("failed to create exec in container %s: %w", containerID, err)
	}

	att, err := m.Client.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to attach to exec in container %s: %w", containerID, err)
	}

	// streamExecTo closes the hijacked connection on every path.
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

// execConn is the part of types.HijackedResponse that streamExecTo drives. Tests
// supply a net.Pipe-backed fake to exercise cancellation without a daemon.
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

// streamExec is streamExecTo with the exec's output discarded.
func streamExec(ctx context.Context, containerID string, conn execConn, stdin io.Reader) error {
	return streamExecTo(ctx, containerID, conn, stdin, io.Discard)
}

// streamExecTo pumps stdin into the exec's connection and writes its demultiplexed
// output (stdout and stderr, interleaved) to stdout. On ctx cancellation it
// force-closes the connection to unblock both pumps and returns an error wrapping
// ctx.Err(). It leaks no goroutine or fd on any path.
//
// stdin must be a finite in-memory reader whose Read never blocks, such as the
// *bytes.Buffer holding the credential tar. The force-close unblocks the output drain
// and any pending Write, but not a concurrent stdin.Read, so a reader that can block
// would leave the call hung after cancellation.
func streamExecTo(ctx context.Context, containerID string, conn execConn, stdin io.Reader, stdout io.Writer) error {
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(conn.Close) }
	defer closeConn()

	// On cancellation, force-close the connection to unblock the drain and the copy below.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			closeConn()
		case <-stop:
		}
	}()

	// Stream stdin in a goroutine, then CloseWrite so the remote tar sees end-of-archive.
	// The channel is buffered so the goroutine exits even if a cancellation leaves nobody
	// to receive.
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

	// Blocks until the exec closes its streams or the connection is force-closed.
	_, copyErr := stdcopy.StdCopy(stdout, stdout, conn.output())
	werr := <-copyDone

	// Report ctx.Err() rather than the "use of closed connection" noise the force-close causes.
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
