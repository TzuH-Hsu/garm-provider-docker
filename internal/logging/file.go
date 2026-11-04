package logging

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// logFileMode is the permission bits a newly created log file gets, before
// the process umask narrows them further. The file can carry instance names,
// repository names and error strings, so it is group-readable at most.
const logFileMode os.FileMode = 0o640

// NewWithFile builds the provider's logger like New, and additionally appends
// every record to the file at path. The file output is the same text format at
// the same level as stderr, produced by the same handler, so a line in the
// file is byte-for-byte the line stderr received.
//
// The file is opened per invocation (this provider is exec'd once per GARM
// call, so there is no long-lived handle to rotate) with O_APPEND|O_CREATE and
// mode 0640 before umask. The directory is not created. Rotation is the
// operator's job, e.g. logrotate with copytruncate (see docs/config-reference.md).
//
// The file can never fail the GARM call. If it cannot be opened, NewWithFile
// logs one warning to stderr and returns the same stderr-only logger New would
// have. If a write to it fails later, one warning goes to stderr, the file is
// dropped for the rest of the process, and stderr keeps receiving every line.
//
// The returned closer releases the file; it is never nil and is safe to call
// more than once.
func NewWithFile(path string) (*slog.Logger, io.Closer) {
	return NewWithFileAt(path, LevelFromEnv())
}

// NewWithFileAt is NewWithFile with an explicit level instead of the
// environment's, for a process (the detached status watcher) that does not
// inherit the provider's environment. Everything else is identical.
func NewWithFileAt(path string, level slog.Level) (*slog.Logger, io.Closer) {
	stderrOnly := NewAt(level)

	f, err := openLogFile(path)
	if err != nil {
		stderrOnly.Warn("logging: cannot open the log file; continuing with stderr only",
			"file", path, "error", err.Error())
		return stderrOnly, nopCloser{}
	}

	sink := &fileSink{f: f, path: path, warn: stderrOnly}
	w := &teeWriter{primary: os.Stderr, secondary: sink}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})), sink
}

// openLogFile opens path for appending, creating it if needed. A relative
// path would resolve against whatever working directory GARM exec'd the
// provider in, which is not something an operator controls, so it is refused.
func openLogFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("path must be absolute")
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, logFileMode)
}

// teeWriter forwards each Write to primary and then to secondary. The
// secondary's outcome never reaches the caller and never hides the primary's:
// the result is the primary's alone, so a failing file cannot make slog report
// a write error or short-circuit stderr.
type teeWriter struct {
	primary   io.Writer
	secondary io.Writer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.primary.Write(p)
	_, _ = t.secondary.Write(p)
	return n, err
}

// fileSink is the file half of the tee. It issues exactly one write(2) per
// record: slog's text handler hands the whole line to a single Write, and
// os.File.Write on a regular file does not split it, so the O_APPEND offset
// update and the data land atomically on a local filesystem and concurrent
// invocations cannot interleave inside a line.
type fileSink struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	warn   *slog.Logger
	failed bool
}

// Write appends p to the file. It always reports success: a failure is turned
// into one stderr warning and the file is dropped from then on.
func (s *fileSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed || s.f == nil {
		return len(p), nil
	}
	if _, err := s.f.Write(p); err != nil {
		s.failed = true
		s.warn.Warn("logging: writing the log file failed; continuing with stderr only",
			"file", s.path, "error", err.Error())
	}
	return len(p), nil
}

// Close releases the file. Later Writes are dropped silently.
func (s *fileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
