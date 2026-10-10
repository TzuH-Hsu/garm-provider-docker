package docker

import (
	"context"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
)

// LogStream selects which of a container's output streams a fake log line was
// written to.
type LogStream int

const (
	// LogStdout is the container's standard output.
	LogStdout LogStream = iota
	// LogStderr is the container's standard error.
	LogStderr
)

// fakeLogEntry is one line a fake container "printed".
type fakeLogEntry struct {
	stream LogStream
	at     time.Time
	line   string
}

// AppendLog records that containerID printed line (a trailing newline is
// added, as the daemon stores one log message per line) on stream, and wakes
// any ContainerLogs follower. It is how a test models the runner's own output,
// e.g. the actions/runner "Listening for Jobs" line. Unknown IDs are ignored.
func (f *FakeClient) AppendLog(containerID string, stream LogStream, line string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := f.find(containerID)
	if c == nil {
		return
	}
	c.logs = append(c.logs, fakeLogEntry{stream: stream, at: time.Now(), line: line + "\n"})
	c.wakeLogFollowers()
}

// wakeLogFollowers signals every blocked ContainerLogs follower that the
// container's logs or state changed. Callers hold f.mu.
func (c *fakeContainer) wakeLogFollowers() {
	if c.logNotify != nil {
		close(c.logNotify)
	}
	c.logNotify = make(chan struct{})
}

// ContainerLogs models the daemon's GET /containers/{id}/logs as the moby SDK
// surfaces it, matching what was observed against a real daemon for this
// provider's runner container (Config.Tty=false):
//
//   - the body is stdcopy-multiplexed (an 8-byte header — stream byte, three
//     zero bytes, big-endian uint32 length — then the payload), one frame per
//     log line, so a caller MUST demultiplex with stdcopy.StdCopy; a TTY
//     container's body is the raw bytes instead;
//   - at least one of ShowStdout/ShowStderr is required, and only the selected
//     streams are returned;
//   - without Follow the body ends (EOF) after the lines recorded so far;
//   - with Follow it then stays open, streaming each new line, until the
//     container is no longer running (EOF) or ctx is cancelled (the read
//     fails with the context error, as the SDK's cancelled HTTP body does).
//
// LogsErr, when set, is returned instead (an API-level failure).
func (f *FakeClient) ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.LogsErr != nil {
		return nil, f.LogsErr
	}
	if !options.ShowStdout && !options.ShowStderr {
		return nil, errors.New("bad parameters: you must choose at least one stream")
	}
	c := f.find(containerID)
	if c == nil {
		return nil, notFoundf("No such container: %s", containerID)
	}
	if c.logDriver == "none" {
		// Observed on a real daemon (Docker Engine 29.8.2): GET
		// /containers/{id}/logs answers HTTP 501 {"message":"configured
		// logging driver does not support reading"}, which the SDK surfaces
		// as an errdefs.NotImplemented error. No stream is returned.
		return nil, errdefs.NotImplemented(errors.New("configured logging driver does not support reading"))
	}
	if c.logNotify == nil {
		c.logNotify = make(chan struct{})
	}

	pr, pw := io.Pipe()
	go f.pumpLogs(ctx, c, options, pw)
	return pr, nil
}

// pumpLogs writes c's log lines into pw — framed exactly like the daemon — and,
// when following, waits for more until the container stops or ctx ends.
func (f *FakeClient) pumpLogs(ctx context.Context, c *fakeContainer, options container.LogsOptions, pw *io.PipeWriter) {
	stdout, stderr := io.Writer(pw), io.Writer(pw)
	if !c.tty {
		stdout = stdcopy.NewStdWriter(pw, stdcopy.Stdout)
		stderr = stdcopy.NewStdWriter(pw, stdcopy.Stderr)
	}

	next := 0
	for {
		f.mu.Lock()
		pending := append([]fakeLogEntry(nil), c.logs[next:]...)
		next = len(c.logs)
		running := c.state == "running" && f.containers[c.id] == c
		notify := c.logNotify
		f.mu.Unlock()

		for _, e := range pending {
			if err := writeLogEntry(e, options, stdout, stderr); err != nil {
				return // the reader was closed
			}
		}
		if !options.Follow || !running {
			_ = pw.Close()
			return
		}
		select {
		case <-ctx.Done():
			_ = pw.CloseWithError(ctx.Err())
			return
		case <-notify:
		}
	}
}

// writeLogEntry emits one entry on its stream, honoring the stream selection
// and the Timestamps option (an RFC 3339 nano prefix, as the daemon writes).
func writeLogEntry(e fakeLogEntry, options container.LogsOptions, stdout, stderr io.Writer) error {
	w := stdout
	switch e.stream {
	case LogStdout:
		if !options.ShowStdout {
			return nil
		}
	case LogStderr:
		if !options.ShowStderr {
			return nil
		}
		w = stderr
	}
	line := e.line
	if options.Timestamps {
		line = e.at.UTC().Format(time.RFC3339Nano) + " " + line
	}
	_, err := w.Write([]byte(line))
	return err
}

// topTitles are the column titles the daemon returns for `docker top` with the
// default ps arguments (observed on Docker Engine 29.8.2).
var topTitles = []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}

// SetProcesses sets the command lines ContainerTop reports for containerID
// (by ID or name), replacing any earlier set. Unknown containers are ignored.
func (f *FakeClient) SetProcesses(containerID string, cmdlines ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(containerID); c != nil {
		c.processes = append([]string(nil), cmdlines...)
	}
}

// ContainerTop models `docker top` with the default ps arguments: a missing
// container is NotFound, a container that is not running is a Conflict (the
// daemon's 409 "container ... is not running"), and a running one lists one
// row per SetProcesses command line under topTitles.
func (f *FakeClient) ContainerTop(_ context.Context, containerID string, _ []string) (container.TopResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := f.find(containerID)
	if c == nil {
		return container.TopResponse{}, notFoundf("No such container: %s", containerID)
	}
	if c.state != "running" {
		return container.TopResponse{}, conflictf("container %s is not running", c.id)
	}
	resp := container.TopResponse{Titles: append([]string(nil), topTitles...)}
	for i, cmd := range c.processes {
		resp.Processes = append(resp.Processes, []string{"1001", strconv.Itoa(100 + i), "1", "0", "00:00", "?", "00:00:00", cmd})
	}
	return resp, nil
}
