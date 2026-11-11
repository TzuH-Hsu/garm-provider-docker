package config

import (
	"strings"
	"testing"
)

func TestLoadJobHooks(t *testing.T) {
	t.Run("unset defaults to fail", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, ""))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.JobHooks != JobHooksFail {
			t.Errorf("JobHooks = %q, want the default %q", cfg.JobHooks, JobHooksFail)
		}
	})

	for _, mode := range []string{"fail", "warn", "off"} {
		t.Run("valid "+mode, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, "job_hooks = \""+mode+"\"\n"))
			if err != nil {
				t.Fatalf("Load job_hooks=%q: %v", mode, err)
			}
			if cfg.JobHooks != mode {
				t.Errorf("JobHooks = %q, want %q", cfg.JobHooks, mode)
			}
		})
	}

	// An empty string means the same as an omitted key, like docker_host = "".
	t.Run("empty string is the default", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, "job_hooks = \"\"\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.EffectiveJobHooks(""); got != JobHooksFail {
			t.Errorf("EffectiveJobHooks(\"\") = %q, want %q", got, JobHooksFail)
		}
	})

	for _, bad := range []string{"bogus", "Warn", "FAIL", "true", "disabled", " off"} {
		t.Run("invalid "+bad, func(t *testing.T) {
			_, err := Load(writeConfig(t, "job_hooks = \""+bad+"\"\n"))
			if err == nil {
				t.Fatalf("Load job_hooks=%q = nil error, want a config error", bad)
			}
			if !strings.Contains(err.Error(), "job_hooks") {
				t.Errorf("error %q should name job_hooks", err)
			}
		})
	}
}

func TestEffectiveJobHooksPrecedence(t *testing.T) {
	tests := []struct {
		name, config, pool, want string
	}{
		{"nothing set", "", "", JobHooksFail},
		{"config default applies", JobHooksWarn, "", JobHooksWarn},
		{"pool overrides config", JobHooksWarn, JobHooksOff, JobHooksOff},
		{"pool can turn it back on", JobHooksOff, JobHooksFail, JobHooksFail},
		{"pool without config", "", JobHooksWarn, JobHooksWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig("ghcr.io/example/runner@sha256:"+strings.Repeat("a", 64), false)
			cfg.JobHooks = tt.config
			if got := cfg.EffectiveJobHooks(tt.pool); got != tt.want {
				t.Errorf("EffectiveJobHooks(%q) with config %q = %q, want %q", tt.pool, tt.config, got, tt.want)
			}
		})
	}
}

func TestValidateJobHooks(t *testing.T) {
	cfg := validBaseConfig("ghcr.io/example/runner@sha256:"+strings.Repeat("a", 64), false)
	for _, ok := range []string{"", "fail", "warn", "off"} {
		cfg.JobHooks = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate with job_hooks=%q: %v", ok, err)
		}
	}
	cfg.JobHooks = "sometimes"
	if err := cfg.Validate(); err == nil {
		t.Error("Validate with job_hooks=sometimes = nil error, want reject")
	}
}

func TestConfigSchemaListsJobHooks(t *testing.T) {
	if !strings.Contains(JSONSchema(), `"job_hooks"`) {
		t.Error("the config JSON schema does not describe job_hooks")
	}
}
