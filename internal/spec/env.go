package spec

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// EntityScope identifies which GitHub entity level a bootstrap payload's
// repo_url refers to.
type EntityScope string

const (
	EntityRepo       EntityScope = "repo"
	EntityOrg        EntityScope = "org"
	EntityEnterprise EntityScope = "enterprise"
)

// Entity is the GitHub-only v1 (plan.md §6) result of parsing a
// BootstrapInstance.RepoURL into the org/repo/enterprise scope the
// myoung34/github-runner entrypoint's RUNNER_ORG/RUNNER_REPO/
// RUNNER_ENTERPRISE variables expect (ADR-002; research.md §2.A).
type Entity struct {
	Scope      EntityScope
	Org        string // set for EntityRepo and EntityOrg
	Repo       string // set for EntityRepo only
	Enterprise string // set for EntityEnterprise only
}

// ParseEntity derives the GitHub entity scope from repo_url. The
// detection heuristic is GitHub-only and based on URL path depth alone
// (plan.md's open questions flag this same limitation for other forges):
//
//	https://github.com/enterprises/<name>  -> enterprise
//	https://github.com/<org>               -> org
//	https://github.com/<org>/<repo>        -> repo
//
// A trailing "/" and/or ".git" suffix is tolerated. Case is preserved
// exactly as given in repo_url: GitHub org/repo/enterprise names are
// treated as opaque path segments here, never normalized, since GitHub's
// API accepts whatever case the operator's URL used.
func ParseEntity(repoURL string) (Entity, error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return Entity{}, fmt.Errorf("failed to parse repo_url %q: %w", repoURL, err)
	}

	path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if path == "" {
		return Entity{}, fmt.Errorf("repo_url %q has no path", repoURL)
	}

	segments := strings.Split(path, "/")

	if segments[0] == "enterprises" {
		if len(segments) != 2 || segments[1] == "" {
			return Entity{}, fmt.Errorf("repo_url %q is not a valid enterprise URL", repoURL)
		}
		return Entity{Scope: EntityEnterprise, Enterprise: segments[1]}, nil
	}

	switch len(segments) {
	case 1:
		return Entity{Scope: EntityOrg, Org: segments[0]}, nil
	case 2:
		return Entity{Scope: EntityRepo, Org: segments[0], Repo: segments[1]}, nil
	default:
		return Entity{}, fmt.Errorf("repo_url %q has an unrecognized path depth", repoURL)
	}
}

// RunnerEnvOptions holds everything BuildRunnerEnv needs to compute the
// runner container's environment, per ADR-002's per-delivery-mode
// contract.
type RunnerEnvOptions struct {
	// JITConfigEnabled mirrors BootstrapInstance.JitConfigEnabled. It
	// gates which of the fields below are emitted at all (ADR-002).
	JITConfigEnabled bool

	// GitHubURL is the base GitHub URL (e.g. "https://github.com"),
	// always set for informational/connectivity-check use inside the
	// entrypoint.
	GitHubURL string

	// RunnerWorkDir is the runner's working directory inside the
	// container, always set.
	RunnerWorkDir string

	// Entity is the parsed repo/org/enterprise scope (ParseEntity),
	// consulted only in non-JIT mode.
	Entity Entity

	// RunnerName is the runner's name, consulted only in non-JIT mode.
	RunnerName string

	// RunnerGroup mirrors BootstrapInstance.GitHubRunnerGroup, consulted
	// only in non-JIT mode.
	RunnerGroup string

	// Labels are the GARM-supplied runner labels
	// (BootstrapInstance.Labels), which GARM itself only populates in
	// non-JIT mode (research.md §1.B) — consulted only in non-JIT mode.
	Labels []string
}

// BuildRunnerEnv returns the runner container's environment variables,
// per ADR-002's per-delivery-mode contract.
//
// It never sets the instance token / bearer token, the metadata URL, or
// the callback URL under any circumstance — those three simply have no
// field in RunnerEnvOptions and no code path here that could emit them.
// This is what makes credential-invisibility (ADR-002's central design
// goal, and the M0 go/no-go gate in plan.md) a property of this
// function's structure rather than something that has to be reviewed for
// each new field added to it.
//
// DOCKER_HOST is deliberately not emitted here: ADR-002 only requires it
// "in DinD modes", which M0 (ADR-001's "none" mode only, per plan.md §3)
// never selects. DinD-mode env wiring is added in M1.
func BuildRunnerEnv(opts RunnerEnvOptions) []string {
	env := []string{
		"JIT_CONFIG_ENABLED=" + strconv.FormatBool(opts.JITConfigEnabled),
		"RUNNER_WORKDIR=" + opts.RunnerWorkDir,
		"GITHUB_URL=" + opts.GitHubURL,
		"DISABLE_RUNNER_UPDATE=true",
	}

	if opts.JITConfigEnabled {
		// JIT mode: GARM bakes the runner's name, labels, group, and
		// ephemeral flag into the .runner/.credentials files themselves
		// at JIT-config-generation time (ADR-002; research.md §1.C).
		// run.sh reads them from those files, not from the environment.
		// Setting RUNNER_ORG/REPO/ENTERPRISE/GROUP/NAME/LABELS/
		// NO_DEFAULT_LABELS/EPHEMERAL here would be inert at best,
		// misleading at worst, so they are deliberately absent rather
		// than set-and-ignored.
		return env
	}

	switch opts.Entity.Scope {
	case EntityEnterprise:
		env = append(env, "RUNNER_ENTERPRISE="+opts.Entity.Enterprise)
	case EntityOrg:
		env = append(env, "RUNNER_ORG="+opts.Entity.Org)
	case EntityRepo:
		env = append(env, "RUNNER_ORG="+opts.Entity.Org, "RUNNER_REPO="+opts.Entity.Repo)
	}

	if opts.RunnerGroup != "" {
		env = append(env, "RUNNER_GROUP="+opts.RunnerGroup)
	}
	env = append(env, "RUNNER_NAME="+opts.RunnerName)
	if len(opts.Labels) > 0 {
		env = append(env, "RUNNER_LABELS="+strings.Join(opts.Labels, ","))
	}
	env = append(env,
		"RUNNER_NO_DEFAULT_LABELS=true",
		"RUNNER_EPHEMERAL=true",
	)

	return env
}
