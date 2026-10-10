//go:build linux

package provider

import (
	"errors"
	"testing"
)

func TestOrphansAreReapedReadsPID1(t *testing.T) {
	prev := readPID1Comm
	t.Cleanup(func() { readPID1Comm = prev })
	for _, tt := range []struct {
		comm string
		err  error
		want bool
	}{
		{"docker-init\n", nil, true},
		{"garm\n", nil, false},
		{"", errors.New("no access"), true},
	} {
		readPID1Comm = func() (string, error) { return tt.comm, tt.err }
		if got, _ := orphansAreReaped(); got != tt.want {
			t.Errorf("PID 1 %q (%v): orphansAreReaped = %v, want %v", tt.comm, tt.err, got, tt.want)
		}
	}
}
