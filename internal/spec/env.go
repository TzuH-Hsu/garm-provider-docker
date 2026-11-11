package spec

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// The variables that point pnpm at its store. pnpm <= 10 reads only the npm_config_
// spelling and pnpm >= 11 only the pnpm_config_ one, so the provider sets both. npm 11
// warns "Unknown env config store-dir" about the former; that is cosmetic (ADR-003).
const (
	PnpmStoreDirEnvNpm  = "npm_config_store_dir"
	PnpmStoreDirEnvPnpm = "pnpm_config_store_dir"
)

// The variables that point pnpm at its metadata cache, split the same way as the
// store dir. Either overrides XDG_CACHE_HOME.
const (
	PnpmCacheDirEnvNpm  = "npm_config_cache_dir"
	PnpmCacheDirEnvPnpm = "pnpm_config_cache_dir"
)

// RunnerJobHooksEnv carries the resolved job-hook mode (fail, warn or off) to the runner
// image's job-started hook, which reads it. The provider always sets it, from the
// config's job_hooks and the pool's extra_specs.job_hooks. extra_env cannot set it
// (GARM_ is reserved), and a custom image that sets it directly still works.
const RunnerJobHooksEnv = "GARM_JOB_HOOKS"

// PnpmMetadataCacheSubdir holds pnpm's metadata cache, directly under the store mount.
// It must not be the mount root: `pnpm store prune` removes what it finds under
// cache-dir, and on pnpm 11 that deleted a whole store (ADR-003). Here it can drop only
// metadata, which is fetched again on demand.
const PnpmMetadataCacheSubdir = "metadata-cache"

// PnpmMetadataCacheDir returns the metadata cache directory for a store mounted at
// storeDir, or "" when storeDir is empty. It is never storeDir itself.
func PnpmMetadataCacheDir(storeDir string) string {
	if storeDir == "" {
		return ""
	}
	return path.Join(storeDir, PnpmMetadataCacheSubdir)
}

// EntityScope is the GitHub entity level a bootstrap payload's repo_url refers to.
type EntityScope string

const (
	EntityRepo       EntityScope = "repo"
	EntityOrg        EntityScope = "org"
	EntityEnterprise EntityScope = "enterprise"
)

// Entity is a repo_url parsed into the scope the base image's RUNNER_ORG, RUNNER_REPO
// and RUNNER_ENTERPRISE variables expect. GitHub only.
type Entity struct {
	Scope      EntityScope
	Org        string // set for EntityRepo and EntityOrg
	Repo       string // set for EntityRepo only
	Enterprise string // set for EntityEnterprise only
}

// ParseEntity derives the entity scope from repo_url's path depth, GitHub shapes only:
//
//	https://github.com/enterprises/<name>  -> enterprise
//	https://github.com/<org>               -> org
//	https://github.com/<org>/<repo>        -> repo
//
// A trailing "/" or ".git" is tolerated, and case is preserved. It returns an error
// unless repo_url is an absolute http(s) URL with a host: url.Parse("github.com/acme")
// yields a relative two-segment path, which would classify an org as a repo and hand
// it writable per-repo caches.
func ParseEntity(repoURL string) (Entity, error) {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return Entity{}, fmt.Errorf("failed to parse repo_url %q: %w", repoURL, err)
	}

	switch scheme := strings.ToLower(u.Scheme); scheme {
	case "http", "https":
		// ok
	default:
		return Entity{}, fmt.Errorf("repo_url %q is not an absolute http(s) URL (scheme %q); refusing to classify its entity scope", repoURL, u.Scheme)
	}
	if u.Host == "" {
		return Entity{}, fmt.Errorf("repo_url %q has no host; refusing to classify its entity scope", repoURL)
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

// RunnerEnvOptions holds the inputs of BuildRunnerEnv.
type RunnerEnvOptions struct {
	// JITConfigEnabled mirrors BootstrapInstance.JitConfigEnabled.
	JITConfigEnabled bool

	// GitHubURL is the base GitHub URL, always set.
	GitHubURL string

	// RunnerWorkDir is the runner's working directory, always set.
	RunnerWorkDir string

	// DockerHost is emitted as DOCKER_HOST in DinD modes. When empty, the entrypoint
	// skips its Docker readiness wait.
	DockerHost string

	// ToolCacheDir is emitted as RUNNER_TOOL_CACHE. It is set whenever the cache
	// feature is on, even without a persistent volume, so the path stays consistent.
	ToolCacheDir string

	// PnpmStoreDir is emitted as both store-dir variables, with both cache-dir variables
	// at its PnpmMetadataCacheSubdir. It is set only when a persistent store volume is
	// mounted, so an ineligible allocation leaves pnpm on its defaults.
	PnpmStoreDir string

	// Entity is the parsed scope, used only without JIT.
	Entity Entity

	// RunnerName is the runner's name, used only without JIT.
	RunnerName string

	// RunnerGroup mirrors BootstrapInstance.GitHubRunnerGroup, used only without JIT.
	RunnerGroup string

	// Labels are GARM's runner labels, which it populates only without JIT.
	Labels []string
}

// BuildRunnerEnv returns the runner container's environment (ADR-002). It can never
// emit the instance token, the metadata URL or the callback URL: RunnerEnvOptions has
// no field for them, which makes credential invisibility a property of this function's
// structure rather than something to review per field.
func BuildRunnerEnv(opts RunnerEnvOptions) []string {
	env := []string{
		"JIT_CONFIG_ENABLED=" + strconv.FormatBool(opts.JITConfigEnabled),
		"RUNNER_WORKDIR=" + opts.RunnerWorkDir,
		"GITHUB_URL=" + opts.GitHubURL,
		"DISABLE_RUNNER_UPDATE=true",
	}
	if opts.DockerHost != "" {
		env = append(env, "DOCKER_HOST="+opts.DockerHost)
		env = append(env, DindSocketGIDEnv+"="+DindSocketGID)
	}
	// Like DOCKER_HOST, these apply in both delivery modes. extra_env cannot override
	// RUNNER_TOOL_CACHE: the RUNNER_ prefix is reserved (ADR-005).
	if opts.ToolCacheDir != "" {
		env = append(env, "RUNNER_TOOL_CACHE="+opts.ToolCacheDir)
	}
	if opts.PnpmStoreDir != "" {
		env = append(env,
			PnpmStoreDirEnvNpm+"="+opts.PnpmStoreDir,
			PnpmStoreDirEnvPnpm+"="+opts.PnpmStoreDir,
		)
		// The metadata cache shares the store's volume, scope and lifecycle.
		cacheDir := PnpmMetadataCacheDir(opts.PnpmStoreDir)
		env = append(env,
			PnpmCacheDirEnvNpm+"="+cacheDir,
			PnpmCacheDirEnvPnpm+"="+cacheDir,
		)
	}

	if opts.JITConfigEnabled {
		// GARM bakes the name, labels, group and ephemeral flag into the credential files
		// and run.sh reads them there, so setting them here would only mislead (ADR-002).
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
