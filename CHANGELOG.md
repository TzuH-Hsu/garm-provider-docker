# Changelog

Notable changes are listed here, in the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. The project
follows [Semantic Versioning](https://semver.org/). Before `v1.0.0`, a minor
release may include breaking changes.

Earlier versions are described on the
[releases page](https://github.com/TzuH-Hsu/garm-provider-docker/releases).

## [Unreleased]

### Added

- The runner images now run a job-started hook before the first step of every
  job. Its first check fails a job whose persistent pnpm store holds a 0-byte
  `pnpm` binary, the damage that a bootstrap pnpm older than 11.22.0 can leave
  behind (`pnpm/action-setup` before v6.1.0). Before, every `pnpm` command in
  such a job printed nothing and exited 0, so install, lint, test and build
  steps passed without running. Now the job fails at "Set up runner" with an
  error annotation that names the cause and the fix (a warning annotation when
  `job_hooks` is `warn`). The operator still has to remove
  the repository's pnpm cache volume; see `docs/package-store-installs.md`.
  Other checks can be added as files in `/opt/garm/hooks/job-started.d/` of a
  derived image. The hook needs a runner image built from this change.
- `job_hooks` (`fail`, the default, `warn` or `off`) sets what the hook does
  when a check finds damage. It is a top-level key of the provider config and
  can be overridden per pool in `extra_specs`. The provider passes the result
  to the runner as `GARM_JOB_HOOKS`. Use `warn` or `off` to keep jobs running
  while a damaged store is replaced.

## [0.2.11] - 2026-11-09

### Security

- Pools that set `disable_jit_config` no longer leave the registration token
  on the runner. The provider delivers the token to `/run/garm/registration-token`
  and the runner image's entrypoint read it, but nothing removed it, so it stayed
  readable by job steps for the life of the runner. A registration token can
  register new runners in the same repository, organization or enterprise
  for as long as it is valid, which is one hour. The entrypoint now
  deletes the file as soon as it has read it, before `config.sh` runs, and
  stops the runner if it cannot. JIT pools and scale sets do not deliver the
  file and were not affected. Use a runner image built from this change; runners
  from earlier images keep the old behavior until they are replaced. During
  registration, `config.sh` still receives the token on its command line, but
  that runs before any job starts.

### Added

- Go updates are now proposed automatically. Renovate opens a pull request for
  a new Go release that moves the `toolchain` line in `go.mod` and the
  `golang` image in the Dockerfile together, and Dependabot opens a monthly
  pull request for Go module updates. Nothing merges on its own.
- CI fails when the `go` line in `go.mod` is not the expected language version
  (`1.25.0`), so a module update cannot raise it unnoticed.
- CI uploads the full `govulncheck` report to code scanning on pushes to
  `main` and weekly, so each advisory appears as an alert in the Security tab.
  The pass or fail rule of the `govulncheck` check is unchanged.

### Fixed

- The orphan sweep no longer removes a paused or restarting runner. It treated
  only a `running` runner as live, so a paused runner was removed once its
  allocation was 20 minutes old, and a runner that had run before and was
  paused or restarting could be removed after 2 minutes. The provider never
  pauses a runner or sets a restart policy, so this affected only runners that
  an operator had paused with `docker pause` or given a restart policy with
  `docker update`. If the sweep cannot inspect a runner that is not live, it
  now logs the error and removes nothing, and a later sweep decides.
- The `repo-scoped caches resolved` log line now names the cache volumes the
  runner mounts. After the provider worked around a volume it does not own, the
  line named the volume it had wanted instead.
- The warning for a status watcher that could not start now says the runner
  stays `pending`, which is what GARM shows. It said `installing`.
- An instance name that would give a one-character container or volume name is
  now rejected before anything is created. Docker requires at least two
  characters. The error for a name over 255 bytes now says the limit comes from
  the filesystem, since a local volume's name is a directory name, and not from
  Docker.

## [0.2.10] - 2026-11-07

### Security

- Release binaries and the provider image are now built with Go 1.27.3 instead
  of Go 1.25.0, so they include the standard library security fixes released
  since. `go.mod` keeps `go 1.25.0` as the language version and adds
  `toolchain go1.27.3`. Before this, CI, the release workflow and the
  Dockerfile all built with the language version, 1.25.0.
- The remaining advisories in `github.com/docker/docker` v28.5.2 have no fixed
  version on the module path this project uses, and are unchanged.

### Added

- CI runs `govulncheck` on pushes, pull requests and weekly. It fails only for
  an advisory that is reachable from this code and has a fixed version.
- CI fails when the Dockerfile's `golang` tag, the installed Go, and the
  `toolchain` line in `go.mod` disagree.

## [0.2.9] - 2026-11-07

### Added

- Exit reports for a runner that stopped on its own now include
  `runner_phase`, which says how far the runner got, for example
  `listening_no_job`, `job_started` or `job_completed`.
- `DeleteInstance` now reports a runner that had already exited before removing
  it, so the report is not lost when GARM deletes the runner before the orphan
  sweep runs. A normal exit after a completed job logs at debug level.
- Contributing guide, code of conduct, security policy, support guide, issue
  forms and pull request template.

### Fixed

- The status watcher now writes to the log file set by `[logging] file`. Its
  output was previously discarded.
- A runner that never started no longer produces an exit report on delete.

[Unreleased]: https://github.com/TzuH-Hsu/garm-provider-docker/compare/v0.2.11...HEAD
[0.2.11]: https://github.com/TzuH-Hsu/garm-provider-docker/compare/v0.2.10...v0.2.11
[0.2.10]: https://github.com/TzuH-Hsu/garm-provider-docker/compare/v0.2.9...v0.2.10
[0.2.9]: https://github.com/TzuH-Hsu/garm-provider-docker/compare/v0.2.8...v0.2.9
