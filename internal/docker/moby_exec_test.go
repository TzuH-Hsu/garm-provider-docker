package docker

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// pipeExecConn is a net.Pipe-backed execConn fake, so streamExec's
// cancellation and leak-freedom behavior can be exercised without a Docker
// daemon (NEW-3). Its output() read end blocks until the peer writes or the
// connection is closed; Close() closes both ends, which is what a ctx-driven
// force-close must do to unblock a StdCopy that is waiting on it.
type pipeExecConn struct {
	out     net.Conn // streamExec reads exec output from here
	outPeer net.Conn // the write side; closing it yields EOF, closing out yields ErrClosedPipe

	writes     atomic.Int64
	closeWrite atomic.Int64
	closes     atomic.Int64
}

func newPipeExecConn() *pipeExecConn {
	r, w := net.Pipe()
	return &pipeExecConn{out: r, outPeer: w}
}

// Write discards stdin (the delivery tar), recording that it was called.
func (p *pipeExecConn) Write(b []byte) (int, error) {
	p.writes.Add(1)
	return len(b), nil
}

func (p *pipeExecConn) output() io.Reader { return p.out }

func (p *pipeExecConn) CloseWrite() error {
	p.closeWrite.Add(1)
	return nil
}

func (p *pipeExecConn) Close() {
	p.closes.Add(1)
	p.out.Close()
	p.outPeer.Close()
}

// TestStreamExecCancellationUnblocks proves that cancelling ctx while
// streamExec is blocked draining the exec output force-closes the hijacked
// connection, unblocks the drain, and surfaces ctx.Err().
func TestStreamExecCancellationUnblocks(t *testing.T) {
	conn := newPipeExecConn()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- streamExec(ctx, "c1", conn, nil) }()

	// Cancel and require a prompt return: if streamExec did not force-close on
	// cancellation it would block on StdCopy forever (the pipe never gets a
	// writer), and this would hang until the safety timeout fires.
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("streamExec err = %v, want it to wrap context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamExec did not return after cancellation — blocked or leaked")
	}

	if conn.closes.Load() == 0 {
		t.Error("expected the hijacked connection to be force-closed on cancellation")
	}
}

// TestStreamExecSuccessNoLeak proves the happy path: a clean EOF from the exec
// output makes streamExec return nil, close the connection exactly once, and
// half-close stdin — with no dependence on cancellation.
func TestStreamExecSuccessNoLeak(t *testing.T) {
	conn := newPipeExecConn()

	// Close the output peer from a goroutine so StdCopy sees a clean EOF (the
	// exec produced no stdout), the way a finished credential-delivery tar
	// would.
	go conn.outPeer.Close()

	if err := streamExec(context.Background(), "c1", conn, nil); err != nil {
		t.Fatalf("streamExec success path returned error: %v", err)
	}
	if got := conn.closeWrite.Load(); got != 1 {
		t.Errorf("CloseWrite called %d times, want 1 (stdin half-closed exactly once)", got)
	}
	if got := conn.closes.Load(); got != 1 {
		t.Errorf("Close called %d times, want 1 (connection closed exactly once)", got)
	}
}
