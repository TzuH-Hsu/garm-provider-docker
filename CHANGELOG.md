# Changelog

Notable changes are listed here, in the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. The project
follows [Semantic Versioning](https://semver.org/). Before `v1.0.0`, a minor
release may include breaking changes.

Earlier versions are described on the
[releases page](https://github.com/TzuH-Hsu/garm-provider-docker/releases).

## [Unreleased]

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

[Unreleased]: https://github.com/TzuH-Hsu/garm-provider-docker/compare/v0.2.10...HEAD
[0.2.10]: https://github.com/TzuH-Hsu/garm-provider-docker/compare/v0.2.9...v0.2.10
[0.2.9]: https://github.com/TzuH-Hsu/garm-provider-docker/compare/v0.2.8...v0.2.9
