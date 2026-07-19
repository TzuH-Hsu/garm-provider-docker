package docker

import "testing"

func TestNewMobyClient(t *testing.T) {
	tests := []struct {
		name       string
		dockerHost string
		wantErr    bool
	}{
		{
			name:       "unix socket host",
			dockerHost: "unix:///var/run/docker.sock",
		},
		{
			name:       "tcp host",
			dockerHost: "tcp://127.0.0.1:2375",
		},
		{
			name:       "host with no scheme separator is rejected",
			dockerHost: "not-a-valid-host",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli, err := NewMobyClient(tt.dockerHost)
			if tt.wantErr {
				if err == nil {
					t.Fatal("NewMobyClient succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewMobyClient returned unexpected error: %v", err)
			}
			if cli == nil {
				t.Fatal("NewMobyClient returned a nil client with no error")
			}
		})
	}
}
