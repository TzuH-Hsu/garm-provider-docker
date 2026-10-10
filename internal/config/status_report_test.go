package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeConfig writes a minimal valid config plus extra TOML and returns its path.
func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "runner_image = \"ghcr.io/example/runner@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + "\"\n" + extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestLoadStatusReport(t *testing.T) {
	tests := []struct {
		name        string
		toml        string
		wantErr     bool
		wantEnabled bool
		wantTimeout time.Duration
	}{
		{name: "omitted table defaults to on, 90s", toml: "", wantEnabled: true, wantTimeout: 90 * time.Second},
		{name: "omitted key defaults to on, 90s", toml: "[status_report]\n", wantEnabled: true, wantTimeout: 90 * time.Second},
		{name: "explicit timeout", toml: "[status_report]\ntimeout_seconds = 30\n", wantEnabled: true, wantTimeout: 30 * time.Second},
		{name: "upper bound accepted", toml: "[status_report]\ntimeout_seconds = 600\n", wantEnabled: true, wantTimeout: 600 * time.Second},
		{name: "zero disables", toml: "[status_report]\ntimeout_seconds = 0\n", wantEnabled: false},
		{name: "negative rejected", toml: "[status_report]\ntimeout_seconds = -1\n", wantErr: true},
		{name: "above the ceiling rejected", toml: "[status_report]\ntimeout_seconds = 601\n", wantErr: true},
		{name: "non-integer rejected", toml: "[status_report]\ntimeout_seconds = \"90s\"\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.toml))
			if tt.wantErr {
				if err == nil {
					t.Fatal("Load succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			sr := cfg.StatusReport
			if sr.Enabled() != tt.wantEnabled {
				t.Fatalf("Enabled() = %v, want %v", sr.Enabled(), tt.wantEnabled)
			}
			if tt.wantEnabled && sr.Timeout() != tt.wantTimeout {
				t.Errorf("Timeout() = %v, want %v", sr.Timeout(), tt.wantTimeout)
			}
		})
	}
}

// TestStatusReportZeroValueIsDefault: a hand-built Config (no Load) gets the
// default behavior, not a silently disabled feature.
func TestStatusReportZeroValueIsDefault(t *testing.T) {
	var sr StatusReport
	if !sr.Enabled() || sr.Timeout() != DefaultStatusReportTimeoutSeconds*time.Second {
		t.Fatalf("zero StatusReport: Enabled=%v Timeout=%v, want true/%ds", sr.Enabled(), sr.Timeout(), DefaultStatusReportTimeoutSeconds)
	}
}
