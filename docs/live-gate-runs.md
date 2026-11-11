# Live gate: recorded runs

Each section below records one run of the [live gate](live-gate.md), newest
first. A record says what that run covered and nothing more, and it is not
rewritten when the gate changes, so read it with the revision of the script it
ran. The script is `scripts/live-gate.sh` from 950607f on and was
`scripts/demo-m0.sh` at every earlier commit recorded here, so use the path of
that commit to read it, for example `git show 92d6c85:scripts/demo-m0.sh`.

| Run | Date | Runner image | `verify`, `none` | `verify`, `--dind` | `verify`, `--pool --non-jit` | `postjob` | Script at |
|---|---|---|---|---|---|---|---|
| [`feat/job-started-hooks` at 85c5542](#featjob-started-hooks-at-85c5542-2026-11-10), local provider build with Go 1.27.3 and runner image built from the branch | 2026-11-10 | Ubuntu 26.04, not digest-pinned | 28 PASS | not run | not run | not run | 85c5542 |
| [`main` at 950607f](#main-at-950607f-2026-11-09), local provider build with Go 1.27.3 and runner image built from the checkout | 2026-11-09 | Ubuntu 26.04, not digest-pinned | 28 PASS | 59 PASS, with mirror | 30 PASS | all PASS | 950607f |
| [`main` at 4146fa1](#main-at-4146fa1-2026-11-07), local build with Go 1.27.3 | 2026-11-07 | Ubuntu 26.04 | 28 PASS | 59 PASS, with mirror | not run | all PASS | 4146fa1 |
| [`main` at 0540477](#main-at-0540477-2026-11-07), local build, the code for `v0.2.9` | 2026-11-07 | Ubuntu 26.04 | 28 PASS | 59 PASS, with mirror | not run | all PASS | 0540477 |
| [`main` at 1b9320c](#main-at-1b9320c-2026-11-01), local build, first run with the container job | 2026-11-01 | Ubuntu 26.04 | 28 PASS | 59 PASS, with mirror | not run | all PASS | 1b9320c |
| [`v0.2.5`](#v025-2026-10-27) release asset | 2026-10-27 | Ubuntu 26.04 | 28 PASS | 58 PASS, with mirror | not run | 1 PASS, 1 FAIL per run: a false positive in the script, not a leak | e4a37fd |
| [`v0.2.4`](#v024-2026-10-12) release | 2026-10-12 | Ubuntu 26.04 | 28 PASS | 53 PASS | not run | all PASS | 0cf2a76 |
| [`v0.2.3`](#v023-2026-10-12) release | 2026-10-12 | Ubuntu 24.04 and 26.04 | 28 PASS on each | 50 PASS, 26.04 only | not run | all PASS | 92d6c85 |
| `v0.2.2` release (see [Earlier runs](#v023-2026-10-12)) | not recorded | not recorded | 28 PASS | 50 PASS | not run | not recorded | efe5014 |
| `v0.2.1` release (see [Earlier runs](#v023-2026-10-12)) | not recorded | not recorded | 26 PASS | 48 PASS | not run | not recorded | fa4351c |

Every `verify` above had 0 FAIL. Counts grow as the gate gains checks, so they
compare only between runs of the same revision:

- `v0.2.0` is the recorded failing baseline: it does not work against GARM
  v0.2.x (see "First attempt failed" under `v0.2.3`).
- `v0.2.1` fails the IPv6 checks added for `v0.2.2`.
- `v0.2.2` ran the revision before `--runner-image` existed, `v0.2.3` the one
  before the shared network namespace and published-port checks, `v0.2.4` the
  one before the registry mirror check, and `v0.2.5` the one before the
  container job.
- No published release asset was run at 950607f, 4146fa1, 0540477 or 1b9320c,
  so those four results do not cover one.
- 950607f is the first run that covers the non-JIT registration path, and the
  first that dispatches through a pool. Older rows never ran
  `--pool --non-jit`, because the gate had no such variant, so their cell in
  that column reads "not run".
- 85c5542 is the head of the job-started hook change, merged as df1d0fc with
  the same tree. That run used the script only for its preflight and the
  `none` `verify` of one runner. The job-hook cases were dispatched by hand,
  so they are not part of the script's `verify` counts. No published release
  asset or image was run at 85c5542.
- The Ubuntu 24.04 image has not been gated since `v0.2.3`.

After the records come the [known upstream issue](#known-upstream-issue) they
refer to and [what is still unverified](#still-unverified).

## `feat/job-started-hooks` at 85c5542, 2026-11-10

The job-started hook from #37 has been run end to end against a live GARM with
a build of the branch at `85c55429e39ff66082fb670552b1aaf953399f46` (merged as
df1d0fc, whose tree is the same), on the Ubuntu 26.04 image, on 2026-11-10.
This run covers the hook dispatcher, the pnpm store check, the `job_hooks`
setting and its `extra_specs` override, on scale sets and on a non-JIT pool.
The Ubuntu 24.04 image was not part of this run.

The script ran in source mode (`--from-source`) for its preflight and for
`verify` of the first runner only. Every other case was a job of a small
workflow in the test repository, dispatched by hand to the scale set or pool
the case needed. To damage the store, a 0-byte file named `pnpm` was placed in
the repository's pnpm cache volume at the path the check looks for, while no
container mounted the volume.

**Environment**

- GARM v0.2.1; provider `interface_version` v0.1.0. Two scale sets (no
  webhook), one without `extra_specs` and one with `{"job_hooks":"warn"}`, and
  a pool with min-idle 1 and max 1 on a separate `[[provider]]` entry with
  `disable_jit_config = true`.
- Docker Engine 29.9.3, amd64, Ubuntu 26.04 host, kernel 7.0.
- Provider: a local build of the branch at 85c5542 from a clean tree, using
  the flags in `release.yml` and Go 1.27.3, whose `GetVersion` reports
  `v0.2.11-5-g85c5542`, sha256
  `f3179a6985a4e8bcbea4c71545f12db3ea73f3ff8f80eca0f801284f263bf59a`. The
  provider config did not set `job_hooks`, so the default `fail` applied
  wherever `extra_specs` did not set it.
- Runner image: built locally from the branch (`--runner-image resolute`,
  Ubuntu 26.04). It is not digest-pinned, because source mode sets
  `allow_unpinned_runner_image=true`. No published image was run.
- The test repository's pnpm store is mounted at `/opt/pnpm-store`.

**Results**

| Case | Runs on | Store | Result |
|---|---|---|---|
| `job_hooks` unset (default `fail`) | scale set | healthy | job succeeded; the hook ran in `Set up runner` and passed; no annotations; `verify` 28 PASS, 0 FAIL, 0 WARN |
| Default mode | scale set | 0-byte `pnpm` planted | job failed at `Set up runner` and every later step was skipped; two error annotations, `Job-started check failed: pnpm-store` (naming the planted path and linking the docs) and `Job stopped by a job-started check`, besides the runner's own `Process completed with exit code 1.`; no warning |
| `extra_specs` `{"job_hooks":"warn"}` | scale set | same damaged store | job succeeded and ran all its steps; exactly one annotation, the warning `Job-started check found damage: pnpm-store`; no error |
| Default mode, non-JIT | pool | same damaged store | `JIT_CONFIG_ENABLED=false` in the runner; job failed at `Set up runner` with the same two error annotations |
| Default mode | scale set | planted file removed | job succeeded; no annotations |
| Default mode | scale set | the whole pnpm cache volume removed | job succeeded; the provider recreated the volume under the same name, with cache kind `pnpm` |
| Runner environment (`docker inspect`) | the runners of the healthy, `warn` and non-JIT cases | n/a | `GARM_JOB_HOOKS` carried the resolved value (`fail` on the default scale set and on the pool, `warn` on the other scale set); `ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/garm/hooks/job-started.sh`; `pnpm_config_store_dir` and `npm_config_store_dir` set to `/opt/pnpm-store` |

- **In the image, without GARM.** Run directly in the built image against the
  damaged store, the hook exited 1 with the two error annotations under
  `fail`, exited 0 with the one warning under `warn`, and exited 0 with no
  output under `off`. An unknown value (`wran`) printed an
  `Unknown GARM_JOB_HOOKS value` warning and then behaved as `fail`. Against a
  healthy store under `fail`, it exited 0 with no output.
- **`extra_specs` validation.** Called by hand, `ValidatePoolInfo` accepted
  `fail`, `warn` and `off`, and rejected `bogus`, `WARN`, an empty string and
  `null`. GARM v0.2.1 on interface v0.1.0 does not call it, as
  [Interface versions](how-it-works.md#interface-versions) says: a scale set
  with `{"job_hooks":"bogus"}` was created without an error. It was deleted
  before any runner started, so what such a pool does when it creates a
  runner is not covered.
- **Non-JIT cleanup.** As at 950607f, the pool replaced its runner after the
  job. Disabling the pool, setting min-idle to 0 and `garm-cli runner rm`
  removed the replacement from GARM, but its GitHub registration stayed
  (idle) until it was deleted through the GitHub API.
- **Teardown.** The scale sets, the pool, the locally built runner image and
  the externals cache volume keyed on that image were removed by name. No
  managed container, network or job volume was left behind, and the test
  repository had no runner registered. The repository's pnpm cache volume
  existed before the run and was kept.

## `main` at 950607f, 2026-11-09

The gate has been run end to end against a live GARM with a build of `main` at
950607f, in three variants on the Ubuntu 26.04 image (`dind_mode=none`, `--dind`
with a registry pull-through mirror, and `--pool --non-jit`), on 2026-11-09,
using the gate script at the same commit. This run covers the non-JIT
registration path added in #87, and the runner image entrypoint change from #85.
The Ubuntu 24.04 image was not part of this run; its last recorded gate is the
`v0.2.3` one below.

The gate ran in source mode (`--from-source`), because #85 changed the runner
image entrypoint and no published image has it yet: the runner image was built
locally from the checkout at 950607f.

**Environment**

- GARM v0.2.1; provider `interface_version` v0.1.0. The `none` and `--dind`
  variants used a scale set (no webhook); the non-JIT variant used a pool.
- Docker Engine 29.9.3, amd64, Ubuntu 26.04 host, kernel 7.0.
- Provider: a local build of `main` at 950607f from a clean tree, using the flags
  in `release.yml` and Go 1.27.3, whose `GetVersion` reports
  `v0.2.10-21-g950607f`, sha256
  `d80443fe6f8304017f5c81556bc71b0e6e255c93c24d3aa4588bee6894fdabb0`, passed with
  `LOCAL_PROVIDER_BIN`, `LOCAL_PROVIDER_SHA256` and `-y`. No published release
  asset was run, and the release build is not expected to match this binary byte
  for byte.
- Runner image: built locally from the checkout at 950607f (`--runner-image
  resolute`, Ubuntu 26.04). It is not digest-pinned, because source mode sets
  `allow_unpinned_runner_image=true`. No published image was run.
- DinD image `docker:dind@sha256:08a98015fce594af7c382883bea080ea206de74402a95da9dc52b6428c8cd121`.
- A Docker Hub pull-through mirror at a LAN address reachable from the sidecar.

**Results**

| Variant | `verify` | GitHub jobs | `postjob` |
|---|---|---|---|
| Ubuntu 26.04, `none`, scale set | 28 PASS, 0 FAIL, 0 WARN | succeeded | 2 PASS, 0 FAIL |
| Ubuntu 26.04, `--dind` + registry mirror, scale set | 59 PASS, 0 FAIL, 0 WARN | `demo` and `container-job` succeeded | 2 PASS, 0 FAIL |
| Ubuntu 26.04, `--pool --non-jit` | 30 PASS, 0 FAIL, 0 WARN | succeeded | 2 PASS, 0 FAIL |

- **Non-JIT pool.** The pool sat on a separate `[[provider]]` entry with
  `disable_jit_config = true`, with min-idle 1 and max 1. `verify` confirmed
  `JIT_CONFIG_ENABLED=false`, so the non-JIT path was taken, and that
  `/run/garm/registration-token` was absent. The job's own step
  `test ! -e /run/garm/registration-token` succeeded. The runner unregistered
  itself from GitHub after its job, as an ephemeral runner does.
- **Pending status.** GARM listed the non-JIT runner as `pending` for its whole
  life, idle, during the job and after it, because the provider's runner status
  reporting is JIT-only. This is the known non-JIT limitation, not a failure.
- **Slow deletion.** After the job, the exited runner container was removed about
  2.5 minutes later by an orphan sweep (it removes exited managed containers on
  the host), not by a `DeleteInstance`. GARM kept the stale instance until the
  pool's runner bootstrap timeout (20 minutes), then called `DeleteInstance`,
  which returned "instance already gone" (exit 30, logged at INFO), and then
  created the replacement idle runner. With max-runners 1, no replacement could
  start for those 17.5 minutes or so. This too is the known limitation.
- **Cleanup.** Scaling the pool to zero and disabling it did not remove the idle
  replacement runner. See "Teardown" under
  [The non-JIT variant](live-gate.md#the-non-jit-variant) for what removes it,
  and the runner it leaves behind on GitHub.
- **Provider label.** In source mode the script's generated `[[provider]]`
  description and remembered modes still mention the default release version
  (`v0.2.5`), which means nothing in source mode. The binary is identified by
  its sha256 above. A follow-up fixes the label.
- **Logs.** The provider log has only INFO lines, no warnings or errors.
- **Secrets.** A scan of the provider log for token, JIT and key material found
  none.

## `main` at 4146fa1, 2026-11-07

The gate has been run end to end against a live GARM with a build of `main` at
4146fa1, in two variants on the Ubuntu 26.04 image (`dind_mode=none`, and
`--dind` with a registry pull-through mirror), on 2026-11-07, using
the gate script at the same commit. This run covers the move of the release
toolchain from Go 1.25.0 to Go 1.27.3. The Ubuntu 24.04 image was not part of
this run; its last recorded gate is the `v0.2.3` one below.

**Environment**

- GARM v0.2.1 (scale-set dispatch, no webhook); provider `interface_version`
  v0.1.0.
- Docker Engine 29.9.3, amd64, Ubuntu 26.04 host, kernel 7.0.
- Provider: a local build of `main` at 4146fa1 from a clean tree, using the flags
  in `release.yml` and Go 1.27.3, with the version set to `v0.2.10` by hand to
  match the tag build, sha256
  `2097a279cd107c24d4e9b2b25dd3e22d6dcf557397d94602cc9b1a6709ebdba5`, passed with
  `LOCAL_PROVIDER_BIN` and `-y`. No published release asset was run, and the
  release build is not expected to match this binary byte for byte.
- Runner image, digest-pinned in the generated config (the `v0.2.5` digest),
  with actions/runner 2.338.0:
  - `ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:f9f20d3727653ff89afe75791a9bcfec965b11b13c0347f071ae2dcab802d2af`
- DinD image `docker:dind@sha256:08a98015fce594af7c382883bea080ea206de74402a95da9dc52b6428c8cd121`,
  daemon 29.9.3.
- A Docker Hub pull-through mirror at a LAN address reachable from the sidecar.

**Results**

| Variant | `verify` | GitHub jobs | `postjob` |
|---|---|---|---|
| Ubuntu 26.04, `--dind` + registry mirror | 59 PASS, 0 FAIL | `demo` and `container-job` succeeded | 2 PASS, 0 FAIL |
| Ubuntu 26.04, `none` | 28 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |

- **A failed first attempt.** The first `--dind` attempt stopped in preflight
  with `failed to resolve a digest for docker:dind`, before any runner or job
  existed. The script does not show the underlying error, so the cause is not
  known. The same digest resolved by hand seconds later, and the retry is the
  result above.
- **Logs.** The provider log has no warnings or errors, and the same sequence of
  lines as the `0540477` run.
- **Not exercised.** The default Docker unix socket and GARM's HTTP callback URL
  were used, so TLS was not exercised. The host sets no CPU quota, so the
  runtime's cgroup-aware `GOMAXPROCS` had no limit to react to. The exit report
  cases from the `0540477` run were not repeated. `go 1.25.0` stays in `go.mod`,
  which keeps Go 1.25 `GODEBUG` defaults; the binary's build info records them.
- **Secrets.** A scan of the provider log for token, JIT and key material found
  none.

## `main` at 0540477, 2026-11-07

The gate has been run end to end against a live GARM with a build of `main` at
0540477, in two variants on the Ubuntu 26.04 image (`dind_mode=none`, and
`--dind` with a registry pull-through mirror), on 2026-11-07, using
the gate script at the same commit. This is the code for `v0.2.9`. The
Ubuntu 24.04 image was not part of this run; its last recorded gate is the
`v0.2.3` one below.

**Environment**

- GARM v0.2.1 (scale-set dispatch, no webhook); provider `interface_version`
  v0.1.0.
- Docker Engine 29.9.3, amd64, Ubuntu 26.04 host, kernel 7.0.
- Provider: a local build of `main` at 0540477 from a clean tree, whose
  `GetVersion` reports `v0.2.8-19-g0540477`, sha256
  `9cab1f6d72449198b8c0859a72742806fcc03bafa8786f31eb07451e15650ee1`, passed with
  `LOCAL_PROVIDER_BIN` and `-y`. No published release asset was run, so this says
  nothing about one until its `SHA256SUMS` entry matches or the release is gated.
- Runner image, digest-pinned in the generated config (the `v0.2.5` digest),
  with actions/runner 2.338.0:
  - `ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:f9f20d3727653ff89afe75791a9bcfec965b11b13c0347f071ae2dcab802d2af`
- DinD image `docker:dind@sha256:08a98015fce594af7c382883bea080ea206de74402a95da9dc52b6428c8cd121`,
  daemon 29.9.3.
- A Docker Hub pull-through mirror at a LAN address reachable from the sidecar.

**Results**

| Variant | `verify` | GitHub jobs | `postjob` |
|---|---|---|---|
| Ubuntu 26.04, `--dind` + registry mirror | 59 PASS, 0 FAIL | `demo` and `container-job` succeeded | 2 PASS, 0 FAIL |
| Ubuntu 26.04, `none` | 28 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |

- **Exit reports.** Killing an idle runner logged
  `runner_phase=listening_no_job`, once from `DeleteInstance` and once from the
  orphan sweep. Killing a runner during a job logged `runner_phase=job_started`
  and exit code 137 from `DeleteInstance`; that phase was not observed on the
  sweep line. A completed job logged `job_completed` at debug level only, and
  scaling down a live idle runner logged no exit line.
- **`DeleteInstance` time.** Scaling down a live idle runner took about 10.2 s
  on this build and on `v0.2.8`, four cycles each, which is the container stop
  grace period.
- **Status watcher.** Its status lines reached the `[logging] file` log.
- **Secrets.** A scan of the provider log for token, JIT and key material found
  none.

## `main` at 1b9320c, 2026-11-01

The gate has been run end to end against a live GARM with a build of `main` at
1b9320c, in two variants on the Ubuntu 26.04 image (`dind_mode=none`, and
`--dind` with a registry pull-through mirror), on 2026-11-01, using
the gate script at the same commit. That commit adds the DinD sidecar's
read-only mount of the runner's externals volume (#36), and the gate's sample
workflow gained the `container-job` that depends on it. It is the first gate
run with a container job, so unlike the `v0.2.5` record below it covers
unreleased code; the next release will carry it. The Ubuntu 24.04 image was
not part of this run; its last recorded gate is the `v0.2.3` one below.

**Environment**

- GARM v0.2.1 (scale-set dispatch, no webhook); provider `interface_version`
  v0.1.0.
- Docker Engine 29.x, amd64.
- Provider: a local build of `main` at 1b9320c, whose `GetVersion` reports
  `v0.2.5-4-g1b9320c`, passed with `LOCAL_PROVIDER_BIN` (see
  [Release mode with a locally built binary](live-gate.md#release-mode-with-a-locally-built-binary)).
  No published release asset was run, so this says nothing about one until the release that carries the change is gated.
- Runner image, digest-pinned in the generated config (the `v0.2.5` digest,
  which this change does not rebuild):
  - `ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:f9f20d3727653ff89afe75791a9bcfec965b11b13c0347f071ae2dcab802d2af`
- DinD image `docker:dind` 29.x, digest-pinned by the preflight.
- A Docker Hub pull-through mirror (`registry:3` in proxy mode) at a LAN
  address reachable from the sidecar, with no `insecure_registries` entry.

**Results**

| Variant | `verify` | GitHub jobs | `postjob` |
|---|---|---|---|
| Ubuntu 26.04, `--dind` + registry mirror | 59 PASS, 0 FAIL | `demo` and `container-job` succeeded | 2 PASS, 0 FAIL |
| Ubuntu 26.04, `none` | 28 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |

- **Container job.** In the `--dind` run `container-job`, which runs in
  `container: debian:trixie-slim` and starts with `actions/checkout@v4`,
  succeeded. `verify` grew from 58 to 59 checks: the sidecar mount-set check now
  expects the runner's externals volume, and a new check asserts that it is
  mounted read-only at the runner's externals path. The `debian:trixie-slim`
  image has no `git`, so `actions/checkout` used its REST archive fallback
  instead of a clone. The run therefore proves that a JavaScript action starts
  from `/__e` in the job container, which is what the mount is for, and not that
  a git clone works there.
- **Registry mirror.** The mirror's access log showed the pulls, including the
  one for the job container's `debian` image, so the sidecar used it for
  container-job images too.
- **`postjob`.** Both runs passed both checks. The diagnostic-log prune stamp
  is exempted by its `role=diag-prune-stamp` label, as of the `v0.2.5` record
  below, and listed informationally.

## `v0.2.5`, 2026-10-27

The gate has been run end to end against a live GARM with the `v0.2.5` release
binary, in two variants on the Ubuntu 26.04 image (`dind_mode=none`, and
`--dind` with a registry pull-through mirror), on 2026-10-27, using
the gate script at e4a37fd. `v0.2.5` adds the persistent-cache and
diagnostic-log changes (#30, #31); the registry mirror check is new to the
gate since the `v0.2.4` run. The Ubuntu 24.04 image was not part of this run;
its last recorded gate is the `v0.2.3` one below.

**Environment**

- GARM v0.2.1 (systemd service, API on loopback), scale-set dispatch (no
  webhook); provider `interface_version` v0.1.0.
- Docker Engine 29.9.2, amd64.
- Provider: the `v0.2.5` release asset
  (`garm-provider-docker_v0.2.5_linux_amd64`, sha256
  `4fd40d5273826e46188291c45c556b3249926f1e77e737d0b7b136d4b8985380`, equal to
  the entry in the release's `SHA256SUMS`); `GetVersion` reports `v0.2.5`. The
  release was still a draft when the gate ran, and release mode cannot
  fetch the asset of a draft release, so the unmodified asset was staged
  with `LOCAL_PROVIDER_BIN` and `LOCAL_PROVIDER_SHA256` (see
  [Release mode with a locally built binary](live-gate.md#release-mode-with-a-locally-built-binary)).
  Release mode otherwise, `-y`.
- Runner image, digest-pinned in the generated config (the `:v0.2.5` digest):
  - `ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:f9f20d3727653ff89afe75791a9bcfec965b11b13c0347f071ae2dcab802d2af`
- DinD image
  `docker:dind@sha256:9e79b879b177fbd72c34376a3dd3d91747da5db49e1f560f76aea59561471977`.

**Results**

| Variant | `verify` | GitHub job | `postjob` |
|---|---|---|---|
| Ubuntu 26.04, `--dind` + registry mirror | 58 PASS, 0 FAIL | succeeded | 1 PASS, 1 FAIL (see below) |
| Ubuntu 26.04, `none` | 28 PASS, 0 FAIL | succeeded | 1 PASS, 1 FAIL (see below) |

- **Registry mirror.** The mirror was a Docker Hub pull-through cache reached
  over plain `http` at the host's LAN address, with no `insecure_registries`
  entry. Its access log showed the pulls, including the ones made inside the
  job, so the sidecar did use it.
- **`postjob`.** In both runs the one FAIL was the diagnostic-log prune stamp
  (`role=diag-prune-stamp`, ADR-003 amendment 2026-10-26) being listed as a
  leftover managed container. That is a false positive in the gate script, not
  a leak: the stamp is a persistent, never-started container kept on purpose,
  like the cache volumes. `postjob` now exempts it by that label and lists it
  informationally; the check that no `role=runner` container remains is
  unchanged. Apart from the stamp, no runner container, sidecar, job network,
  job-scoped volume or socket and `dind-state` volume was left.
- **Persistent pnpm cache (#30).** With the pnpm store mounted,
  `npm_config_cache_dir` and `pnpm_config_cache_dir` were both
  `<store mount>/metadata-cache`.
- **Diagnostic-log prune throttle (#31).** The prune ran one round on the first
  `CreateInstance` and created the stamp, in state `Created` and never started.
  Later `CreateInstance` and `ListInstances` calls created no helper
  containers.

## `v0.2.4`, 2026-10-12

The gate had been run end to end against a live GARM with the published
`v0.2.4` release binary, in two variants on the Ubuntu 26.04 image
(`dind_mode=none` and `--dind`), on 2026-10-12, using the gate script at
0cf2a76. `v0.2.4` changes one thing the gate cares about: in
`privileged-sidecar` mode the sidecar now joins the runner's network namespace
(ADR-001, 2026-10-12 amendment). The Ubuntu 24.04 image was not part of this
run; its last recorded gate is the `v0.2.3` one below.

**Environment**

- GARM v0.2.1 (systemd service, API on loopback), scale-set dispatch (no
  webhook); provider `interface_version` v0.1.0.
- Docker Engine 29.9.0 on Ubuntu 26.04.1, amd64.
- Provider: the `v0.2.4` release binary
  (`garm-provider-docker_v0.2.4_linux_amd64`, sha256
  `f02df425a00c9526f0b7ae6a5d3b19c288503a0e50ee40e08584ff7500e5bd5f`,
  checked against the release's `SHA256SUMS`); `GetVersion` reports
  `v0.2.4`. Release mode, `-y`.
- Runner image, digest-pinned in the generated config (the `:v0.2.4` digest,
  which `:latest` also pointed at; the release workflow rebuilt it, so it
  differs from the `v0.2.3` one):
  - `ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:1c48eb526328814ff4b76a2e4dbd50d1f77135cb04054578ab225fbf934d484a`
- DinD image
  `docker:dind@sha256:0b6d18a4a222e71c88be6034926b4f6cde2fc33c2218643d03ccfddc65995470`
  (unchanged).

**Results**

| Variant | `verify` | GitHub job | `postjob` |
|---|---|---|---|
| Ubuntu 26.04, `--dind` | 53 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |
| Ubuntu 26.04, `none` | 28 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |

- No WARN lines in either `verify`. `postjob` reports "no managed runner
  container remains" and "no leftover job-scoped garm.docker/managed=true
  containers, networks, or volumes"; the persistent cache volumes are reported
  as informational.
- The `none` count matches the `v0.2.3` run (28). The `--dind` count is 53
  against 50: the two checks that the sidecar sits on the runner's per-job
  network are gone, and five are new (see below).
- Each variant was preflighted separately (`preflight --version v0.2.4
  --runner-image resolute --dind`, then the same without `--dind` and with the
  `none` scale set), and `verify` was started only after the runner logged
  `Listening for Jobs` (GARM status `idle`). The sample workflows the script
  prints were used (60 s and 30 s holds).

What is new against the `v0.2.3` run (28 and 50 PASS, script at 92d6c85):

- **Shared network namespace.** The sidecar's `NetworkMode` is
  `container:<runner id>`, it has no network attachment of its own, and the
  runner is the only container on the per-job network. In the live `--dind`
  run `docker inspect` showed the sidecar `Privileged=true` with no sysctls and
  the runner with `net.ipv6.conf.all.disable_ipv6=1` (the sysctl is applied
  once, by the namespace owner); the runner's `/etc/hosts` still has only the
  `127.0.0.1 localhost` line, no `::1`. `verify` reads the same network
  namespace inode from both containers.
- **Published ports reach the runner's localhost.** `verify` starts a nested
  `busybox` httpd with an ephemeral published port through the runner's
  `DOCKER_HOST` and fetches it from the runner at `127.0.0.1` and at
  `localhost` (2 of the 5 new checks; the other 3 are the namespace checks
  above). The workflow's new step does the same inside the job, and the job
  log shows `127.0.0.1:<port> -> published-ok` and
  `localhost:<port> -> published-ok`. The `v0.2.3` gate had no such check;
  the `ECONNREFUSED` this fixes is described in the ADR-001 amendment.
- **Runner status.** In both runs GARM's runner list showed `pending`,
  `installing`, `idle` (3 s after `installing`), and GARM logged the job as
  started 37 s (`--dind`) and 33 s (`none`) after `idle`. GARM's log has no
  `invalid runner status transition` line over the run windows, and no WARN
  or ERROR line for the gate's scale sets (see "Known upstream issue").
- **Teardown.** GARM began deleting the instance within 2 s of logging the
  job completed in both runs, and `postjob` found no leftover container,
  sidecar, job network, job-scoped volume or socket and `dind-state` volume.
- **Release plumbing.** The release carries the `linux/amd64` and
  `linux/arm64` binaries and `SHA256SUMS`, and the release-triggered
  "Container Images" run succeeded. `:latest` and `:v0.2.4` resolve to the same
  digest for `garm-runner-noble` (`14599ec9...`), `garm-runner-resolute`
  (`1c48eb52...`) and `garm-provider-docker` (`621d34ee...`).

## `v0.2.3`, 2026-10-12

The gate had been run end to end against a live GARM with the published
`v0.2.3` release binary, in three variants (Ubuntu 24.04 image with
`dind_mode=none`, Ubuntu 26.04 image with `none`, and Ubuntu 26.04 image with
`--dind`), on 2026-10-12, using the gate script at 92d6c85. That revision
had neither the shared network namespace nor the published-port check, so this
run says nothing about either.

**Environment**

- GARM v0.2.1 (systemd service, API on loopback), scale-set dispatch (no
  webhook); provider `interface_version` v0.1.0.
- Docker Engine 29.9.0 on Ubuntu 26.04.1, amd64.
- Provider: the `v0.2.3` release binary
  (`garm-provider-docker_v0.2.3_linux_amd64`, sha256
  `f3786f450296ce68f9c6b731a32cdeb1f902bd36aa3172b24f24c47f2221499e`,
  checked against the release's `SHA256SUMS`); `GetVersion` reports
  `v0.2.3`. Release mode, `-y`.
- Runner images, digest-pinned in the generated config (the `:v0.2.3`
  digests, which `:latest` also pointed at):
  - `ghcr.io/tzuh-hsu/garm-runner-noble@sha256:a1f5beab548f690e6da742c11f885024a40539eb9ba23cfbea83691409624824`
  - `ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:805d733f90dfe872d531d00d2b8fea92f5977e9de2d46be7b8f528e2f1b77dea`
- DinD image
  `docker:dind@sha256:0b6d18a4a222e71c88be6034926b4f6cde2fc33c2218643d03ccfddc65995470`.

**Results**

| Variant | `verify` | GitHub job | `postjob` |
|---|---|---|---|
| Ubuntu 24.04, `none` | 28 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |
| Ubuntu 26.04, `none` | 28 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |
| Ubuntu 26.04, `--dind` | 50 PASS, 0 FAIL | succeeded | 2 PASS, 0 FAIL |

- No WARN lines in any `verify`. `postjob` reports "no managed runner
  container remains" and "no leftover job-scoped garm.docker/managed=true
  containers, networks, or volumes"; the persistent cache volumes are reported
  as informational.
- In the `--dind` run the job's `docker run hello-world` and `docker build`
  worked, and the shared-workspace step printed `sidecar saw: ok` and
  `runner read back: nested`.
- The counts match the `v0.2.2` run (28 and 50).

Each variant was preflighted separately (`preflight --version v0.2.3
--runner-image noble`, then `--runner-image resolute`, then `--dind
--runner-image resolute`), because the saved variant and image are part of the
remembered modes. The `none` runs used the `none` scale set and the `--dind`
run the scale set created with the DinD `extra_specs`; the sample workflows
the script prints were used (30 s and 60 s holds).

**One earlier attempt, not counted.** The first Ubuntu 24.04 run reported 27
PASS and 1 FAIL: check (d) found no `/actions-runner/.runner`. `verify` had
been started within seconds of the runner container appearing, while GARM
still listed the runner as `installing`; the entrypoint links the credential
files into the install directory shortly after the container starts, and the
link was in place when listed shortly afterwards. The runner then started,
the job succeeded and `postjob` passed (2 PASS). The run above was started
again, with `verify` held until the runner logged `Listening for Jobs`. See
[Step 4(a-d)](live-gate.md#step-4a-d--verify) and the
[troubleshooting table](live-gate.md#troubleshooting).

What is new against the `v0.2.2` run (28 and 50 PASS, efe5014):

- **Ubuntu 26.04 runner image.** `ghcr.io/tzuh-hsu/garm-runner-resolute` is
  published from `v0.2.3`. Inside the live runner, as the `runner` user:

  | | Ubuntu 24.04 (`noble`) | Ubuntu 26.04 (`resolute`) |
  |---|---|---|
  | `gcc` | 13.3.0 | 15.2.0 |
  | `clang-format`, `clang-tidy` | 18.1.3 | 22.1.2 |
  | `cmake` | 3.28.3 | 4.2.3 |
  | `pipx` | 1.4.3 | 1.8.0 |

  On both images the runner user can write to `/usr/local/bin` and
  `sudo -n true` succeeds (the runner user is in the `sudo` group, as
  `runner-images/README.md` documents). The runner's PID 1 (`run.sh`, after the entrypoint) has an
  open-file soft limit of 65536 against a 524288 hard limit; `Runner.Listener`
  shows 524288. `/etc/hosts` has only the `127.0.0.1 localhost` line, no `::1`.
- **Caches keyed on the image's OS release.** The toolcache and pnpm volumes
  now carry the OS release in their name, so the two images never share one:
  `garm-cache-toolcache-<repo-key>-ubuntu-24.04-1` and
  `garm-cache-toolcache-<repo-key>-ubuntu-26.04-1` (likewise
  `garm-cache-pnpm-<repo-key>-ubuntu-24.04-9` and `...-ubuntu-26.04-9`)
  existed side by side after the runs, next to the unkeyed volumes that earlier
  releases created, which `v0.2.3` leaves alone. The externals cache is
  keyed by image digest, so each image got its own
  (`garm-cache-externals-<digest>`). The persistent cache volumes grew from 24
  to 27 over the first Ubuntu 26.04 run (its toolcache, pnpm and externals
  volumes).
- **Runner status.** In all runs GARM's runner list showed `pending`,
  `installing`, `idle`, then `active` once the job started (`idle` came 4 to 5 s
  after `installing`, and `active` 15 s (the earlier attempt), 34 s, 32 s and
  35 s after `idle`), and GARM's log has no `invalid runner status transition`
  line and no WARN or ERROR line over the run windows (see "Known upstream
  issue").
- **Teardown.** The runner container (and the sidecar in `--dind`) was gone
  within 1 to 2 s of GARM marking the instance `terminated`, with no leftover
  container, network or job-scoped volume, and no defunct process.
- **Release plumbing.** The release carries the `linux/amd64` and
  `linux/arm64` binaries and `SHA256SUMS`, and the release-triggered
  "Container Images" run succeeded. `:latest` and `:v0.2.3` resolve to the same
  digest for `garm-runner-noble` (`a1f5beab...`), `garm-runner-resolute`
  (`805d733f...`) and `garm-provider-docker` (`70d4bee4...`).

**First attempt failed, which is why the gate exists.** With the v0.2.0
release binary, `CreateInstance` rejected the `runner_install_template`
that GARM injects, and `DeleteInstance`/`GetInstance` failed with
"missing pool ID" for scale-set instances. A separate GARM v0.1.1
`ListInstances` bug was also hit. The first two are fixed in `v0.2.1`.

**Earlier runs** (superseded by the results above): `v0.2.2` passed the
revision at efe5014 with 28 PASS for `none` and 50 for `--dind` (runner image
digest `sha256:de8647b0...`), `v0.2.1` passed an earlier one with 26 and 48
(digest `sha256:10b82dc4...`), and before that the gate was run repeatedly
against pre-release builds as checks were added. Each passed when first added.

Verified as a consequence: tmpfs credential delivery against a real
daemon, a symlinked `.runner` being accepted by `actions/runner` in JIT
mode, the scale-set path against a live GARM, and the `--dind` variant
against a real daemon.

## Known upstream issue

Root cause (GARM v0.2.1, scale sets): GARM's runner-status state machine
has no `pending` to `active` edge (`RunnerStatusTransitions` in
`params/params.go`: `pending` may only go to `failed`, `installing` or
`terminated`; `active` is reachable only via `idle`). With runner
callbacks disabled (the default), the runner stays `pending`. The
scale-set `JobStarted` handler uses the transition-checked update, so it
fails with `invalid runner status transition from pending to active`. The
scale-set listener then does not acknowledge the message and refetches it,
so the scale set processes no further messages (no new job pickup, no
scale-up, `JobCompleted` delayed) until GARM's periodic consolidation
removes the runner, up to about 5 minutes after the job. The job itself
still succeeds. Pools are unaffected, because they use a forced update.

Fixed upstream in cloudbase/garm#817 (commit `1a9aeff2ae`, "Allow pending
and installing runners to become active"), present on `main` and
`release/v0.2` but not in a tagged release as of 2026-10-11.

**Narrowed by this provider since v0.2.1.** The provider reports the
runner `installing` and then `idle` to GARM itself (`[status_report]`, on
by default; `idle` follows `Listening for Jobs`, so it is not instant). This
shrinks the stall to a short window rather than removing it. If GARM
delivers `JobStarted` while the row is still `installing` (the runner can
start polling before `idle` is posted), the checked update fails with
`invalid runner status transition from installing to active`, because
`RunnerStatusTransitions` allows `installing` only to `failed`, `idle`,
`terminated` or `installing`. The scale-set listener
(`handleSessionMessage` and `loop` in `workers/scaleset/scaleset_listener.go`)
does not acknowledge the message and refetches it immediately, so once the
watcher posts `idle` the retried `JobStarted` applies: a brief, self-clearing
burst of that error. The original stall (until GARM's consolidation, up to
about 5 minutes) returns whenever `idle` is not posted: the wait times out, reporting is
disabled (`timeout_seconds = 0`), a callback still fails after its retries (a
failed `installing` report skips the wait, a failed `idle` report leaves the
row `installing`), the watcher cannot be started, or GARM restarts while the
watcher waits (see
[`[status_report]`](config-reference.md#status_report-runner-status-reported-to-garm)).
The gate runs above showed
`idle` 15 to 37 s before `active` and no transition error in GARM's log, but
that ordering is not guaranteed. Upstream cloudbase/garm#817 removes the
race and the stall entirely once it is in a release.

## Still unverified

- The Ubuntu 24.04 runner image with `v0.2.4` or `v0.2.5` (it was gated with
  `v0.2.3`; both releases rebuilt it).
- Published ports on the runner's `localhost` under `sysbox-runc`, whose
  sidecar keeps its own network namespace (ADR-001, 2026-10-12 amendment).
- A published runner image that has the #85 entrypoint change. The `950607f`
  run used an image built locally from the checkout.
- An arm64 host.
- The job-started hook on the Ubuntu 24.04 image, which copies the same hook
  files but was not built or run at 85c5542.
- `job_hooks` set in the provider config rather than in `extra_specs`, and
  `off` through GARM (it ran only directly in the image).
- A pnpm store damaged by a real pnpm run rather than a planted file, the hook
  under the DinD modes, and a published runner image that carries the hook.
- What a pool with an invalid `job_hooks` does when it creates a runner on
  GARM v0.2.1, which does not call `ValidatePoolInfo`.
- Webhook-driven pool dispatch, and a pool on a JIT provider entry. The pool at
  950607f was non-JIT, ran one idle runner and received no webhook.
