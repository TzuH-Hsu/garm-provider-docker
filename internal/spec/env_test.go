package spec

import (
	"reflect"
	"testing"
)

func TestParseEntity(t *testing.T) {
	tests := []struct {
		name    string
		repoURL string
		want    Entity
		wantErr bool
	}{
		{
			name:    "repo URL",
			repoURL: "https://github.com/octo-org/octo-repo",
			want:    Entity{Scope: EntityRepo, Org: "octo-org", Repo: "octo-repo"},
		},
		{
			name:    "repo URL with trailing slash",
			repoURL: "https://github.com/octo-org/octo-repo/",
			want:    Entity{Scope: EntityRepo, Org: "octo-org", Repo: "octo-repo"},
		},
		{
			name:    "repo URL with trailing .git",
			repoURL: "https://github.com/octo-org/octo-repo.git",
			want:    Entity{Scope: EntityRepo, Org: "octo-org", Repo: "octo-repo"},
		},
		{
			name:    "repo URL with trailing slash and .git",
			repoURL: "https://github.com/octo-org/octo-repo.git/",
			want:    Entity{Scope: EntityRepo, Org: "octo-org", Repo: "octo-repo"},
		},
		{
			name:    "org URL",
			repoURL: "https://github.com/octo-org",
			want:    Entity{Scope: EntityOrg, Org: "octo-org"},
		},
		{
			name:    "org URL with trailing slash",
			repoURL: "https://github.com/octo-org/",
			want:    Entity{Scope: EntityOrg, Org: "octo-org"},
		},
		{
			name:    "enterprise URL",
			repoURL: "https://github.com/enterprises/octo-enterprise",
			want:    Entity{Scope: EntityEnterprise, Enterprise: "octo-enterprise"},
		},
		{
			name:    "enterprise URL with trailing slash",
			repoURL: "https://github.com/enterprises/octo-enterprise/",
			want:    Entity{Scope: EntityEnterprise, Enterprise: "octo-enterprise"},
		},
		{
			name:    "uppercase org and repo names are preserved, not lowercased",
			repoURL: "https://github.com/OctoOrg/OctoRepo",
			want:    Entity{Scope: EntityRepo, Org: "OctoOrg", Repo: "OctoRepo"},
		},
		{
			name:    "uppercase enterprise name is preserved",
			repoURL: "https://github.com/enterprises/OctoEnterprise",
			want:    Entity{Scope: EntityEnterprise, Enterprise: "OctoEnterprise"},
		},
		{
			name:    "empty path is rejected",
			repoURL: "https://github.com",
			wantErr: true,
		},
		{
			name:    "empty path with trailing slash is rejected",
			repoURL: "https://github.com/",
			wantErr: true,
		},
		{
			name:    "too many path segments is rejected",
			repoURL: "https://github.com/a/b/c",
			wantErr: true,
		},
		{
			name:    "malformed enterprise URL (no name) is rejected",
			repoURL: "https://github.com/enterprises",
			wantErr: true,
		},
		{
			name:    "unparseable URL is rejected",
			repoURL: "://not-a-url",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseEntity(tt.repoURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseEntity(%q) succeeded with %+v, want error", tt.repoURL, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseEntity(%q) returned unexpected error: %v", tt.repoURL, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseEntity(%q) = %+v, want %+v", tt.repoURL, got, tt.want)
			}
		})
	}
}

func TestBuildRunnerEnvJITMode(t *testing.T) {
	env := BuildRunnerEnv(RunnerEnvOptions{
		JITConfigEnabled: true,
		GitHubURL:        "https://github.com",
		RunnerWorkDir:    "/runner/_work",
		Entity:           Entity{Scope: EntityRepo, Org: "octo-org", Repo: "octo-repo"},
		RunnerName:       "my-instance",
		RunnerGroup:      "Default",
		Labels:           []string{"self-hosted", "docker"},
	})

	mustContain(t, env, "JIT_CONFIG_ENABLED=true")
	mustContain(t, env, "RUNNER_WORKDIR=/runner/_work")
	mustContain(t, env, "GITHUB_URL=https://github.com")
	mustContain(t, env, "DISABLE_RUNNER_UPDATE=true")

	forbidden := []string{
		"RUNNER_ORG=", "RUNNER_REPO=", "RUNNER_ENTERPRISE=", "RUNNER_GROUP=",
		"RUNNER_NAME=", "RUNNER_LABELS=", "RUNNER_NO_DEFAULT_LABELS=", "RUNNER_EPHEMERAL=",
	}
	for _, prefix := range forbidden {
		mustNotContainPrefix(t, env, prefix)
	}

	assertNeverLeaksCredentials(t, env)
}

func TestBuildRunnerEnvNonJITMode(t *testing.T) {
	tests := []struct {
		name   string
		entity Entity
		want   []string
	}{
		{
			name:   "repo scope sets RUNNER_ORG and RUNNER_REPO",
			entity: Entity{Scope: EntityRepo, Org: "octo-org", Repo: "octo-repo"},
			want:   []string{"RUNNER_ORG=octo-org", "RUNNER_REPO=octo-repo"},
		},
		{
			name:   "org scope sets only RUNNER_ORG",
			entity: Entity{Scope: EntityOrg, Org: "octo-org"},
			want:   []string{"RUNNER_ORG=octo-org"},
		},
		{
			name:   "enterprise scope sets only RUNNER_ENTERPRISE",
			entity: Entity{Scope: EntityEnterprise, Enterprise: "octo-enterprise"},
			want:   []string{"RUNNER_ENTERPRISE=octo-enterprise"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := BuildRunnerEnv(RunnerEnvOptions{
				JITConfigEnabled: false,
				GitHubURL:        "https://github.com",
				RunnerWorkDir:    "/runner/_work",
				Entity:           tt.entity,
				RunnerName:       "my-instance",
				RunnerGroup:      "Default",
				Labels:           []string{"self-hosted", "docker"},
			})

			for _, w := range tt.want {
				mustContain(t, env, w)
			}
			mustContain(t, env, "RUNNER_GROUP=Default")
			mustContain(t, env, "RUNNER_NAME=my-instance")
			mustContain(t, env, "RUNNER_LABELS=self-hosted,docker")
			mustContain(t, env, "RUNNER_NO_DEFAULT_LABELS=true")
			mustContain(t, env, "RUNNER_EPHEMERAL=true")
			mustContain(t, env, "JIT_CONFIG_ENABLED=false")

			assertNeverLeaksCredentials(t, env)
		})
	}
}

func TestBuildRunnerEnvNonJITModeOmitsOptionalFields(t *testing.T) {
	env := BuildRunnerEnv(RunnerEnvOptions{
		JITConfigEnabled: false,
		GitHubURL:        "https://github.com",
		RunnerWorkDir:    "/runner/_work",
		Entity:           Entity{Scope: EntityOrg, Org: "octo-org"},
		RunnerName:       "my-instance",
		// RunnerGroup and Labels intentionally left zero-valued.
	})

	mustNotContainPrefix(t, env, "RUNNER_GROUP=")
	mustNotContainPrefix(t, env, "RUNNER_LABELS=")
	mustContain(t, env, "RUNNER_NAME=my-instance")
}

func mustContain(t *testing.T, env []string, want string) {
	t.Helper()
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Errorf("env %v does not contain %q", env, want)
}

func mustNotContainPrefix(t *testing.T, env []string, prefix string) {
	t.Helper()
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			t.Errorf("env %v unexpectedly contains an entry with prefix %q: %q", env, prefix, e)
		}
	}
}

// assertNeverLeaksCredentials is the direct table-test expression of
// ADR-002's central design goal: BEARER_TOKEN, METADATA_URL, and
// CALLBACK_URL must never appear in the runner container's environment,
// in either JIT or non-JIT mode.
func assertNeverLeaksCredentials(t *testing.T, env []string) {
	t.Helper()
	forbidden := []string{"BEARER_TOKEN", "METADATA_URL", "CALLBACK_URL", "INSTANCE_TOKEN"}
	for _, e := range env {
		for _, f := range forbidden {
			if len(e) >= len(f) && e[:len(f)] == f {
				t.Errorf("env %v must never contain a %q entry, found %q", env, f, e)
			}
		}
	}
}
