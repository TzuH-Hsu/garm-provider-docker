# Changelog

Notable changes are listed here, in the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. The project
follows [Semantic Versioning](https://semver.org/). Before `v1.0.0`, a minor
release may include breaking changes.

Earlier versions are described on the
[releases page](https://github.com/TzuH-Hsu/garm-provider-docker/releases).

## [Unreleased]

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

[Unreleased]: https://github.com/TzuH-Hsu/garm-provider-docker/compare/v0.2.9...HEAD
[0.2.9]: https://github.com/TzuH-Hsu/garm-provider-docker/compare/v0.2.8...v0.2.9
