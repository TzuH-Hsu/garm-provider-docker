package logging

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// logFileMode is group-readable at most because log lines can carry instance
// names, repository names and error strings.
const logFileMode os.FileMode = 0o640

// NewWithFile is New with every record also appended to the file at path, which
// must be absolute and whose directory must exist.
//
// The file can never fail the GARM call: if opening it or a later write fails,
// one warning goes to stderr and logging continues there alone. The returned
// closer is never nil and is safe to call more than once.
func NewWithFile(path string) (*slog.Logger, io.Closer) {
	return NewWithFileAt(path, LevelFromEnv())
}

// NewWithFileAt is NewWithFile with an explicit level, as NewAt is to New.
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

// openLogFile refuses a relative path, which would resolve against whatever
// working directory GARM exec'd the provider in.
func openLogFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("path must be absolute")
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, logFileMode)
}

// teeWriter writes to primary, then secondary, and returns only primary's result,
// so a failing file cannot make slog report an error.
type teeWriter struct {
	primary   io.Writer
	secondary io.Writer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.primary.Write(p)
	_, _ = t.secondary.Write(p)
	return n, err
}

// fileSink appends records to the log file. Each record is one write(2) on an
// O_APPEND file, so on a local filesystem concurrent provider invocations cannot
// interleave inside a line.
type fileSink struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	warn   *slog.Logger
	failed bool
}

// Write always reports success. The first failure warns once on stderr and drops
// the file.
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

// Close releases the file and drops later Writes.
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
