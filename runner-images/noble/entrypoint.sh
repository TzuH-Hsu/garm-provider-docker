#!/usr/bin/env bash
# entrypoint.sh - garm-provider-docker runner image entrypoint.
#
# Replaces the myoung34/github-runner base image's own config.sh-based
# entrypoint entirely (ADR-002). This script never fetches JIT config or a
# registration token itself: the provider fetches those out-of-band and
# streams them into this container's memory-backed tmpfs (/run/garm) via a
# `docker exec`-run `tar -x` after start (ADR-002 F1). The contract
# implemented here is:
#
#   1. Wait for the provider's atomic delivery marker (/run/garm/.delivered).
#   2. In DinD modes (DOCKER_HOST set), wait for the Docker daemon to answer,
#      independently of and concurrently with step 1.
#   3. Install the credentials and exec the runner:
#      - JIT: symlink the tmpfs files into the install dir and exec run.sh
#        directly (no config.sh, no --jitconfig). All JIT files are delivered
#        up front, so the symlinks are never dangling.
#      - non-JIT: run config.sh (as the runner user via gosu) to register,
#        letting it write .runner/.credentials/.credentials_rsaparams into the
#        install dir normally; THEN, only after config.sh SUCCEEDS, move those
#        files onto the tmpfs and replace them with symlinks, so steady-state
#        credentials are tmpfs-resident. This is a post-config move, NOT a
#        pre-link: the real .NET runner's `Runner.Listener configure` reads
#        .credentials in its startup HostContext constructor, so a symlink
#        pre-created before config.sh — whose tmpfs target does not exist yet
#        in non-JIT mode (only registration-token is delivered) — makes that
#        read fail (FileNotFoundException, exit 134) BEFORE registration. A
#        scrub-on-failure trap guards the phase so no failure path leaves
#        credentials on the writable layer.
#
# Credentials never land on the container's disk-backed writable layer: they
# stay resident on the memory-backed tmpfs, reached from the install dir only
# through symlinks (ADR-002 F2). Never echo/log credential file contents.

set -euo pipefail

readonly CRED_DIR="/run/garm"
readonly RUNNER_DIR="/actions-runner"
readonly RUNNER_USER="runner"

# ReadyMarker: the provider writes this file as the LAST entry of the
# credential tar, so it appears only once every credential file is fully
# delivered (ADR-002 F1). Waiting for it — instead of the individual files —
# means this entrypoint never observes a partial credential set.
readonly READY_MARKER=".delivered"

readonly CRED_WAIT_SECONDS="${GARM_CRED_WAIT_SECONDS:-120}"
readonly DOCKER_WAIT_SECONDS="${WAIT_FOR_DOCKER_SECONDS:-120}"
# The externals seed can copy ~380MB on a cold volume (the provider bounds its own
# seed at 10 min), so this consumer-side wait is generous headroom over that while
# still bounding a wedged seed. On the common warm path the marker is already
# present and the wait returns immediately.
readonly EXTERNALS_WAIT_SECONDS="${GARM_EXTERNALS_WAIT_SECONDS:-600}"
readonly POLL_INTERVAL_SECONDS=1

log() {
  printf '[entrypoint] %s\n' "$*" >&2
}

fail() {
  log "ERROR: $*"
  exit 1
}

# wait_for_files polls CRED_DIR, bounded by $1 seconds, until every filename
# in the remaining arguments exists there.
wait_for_files() {
  local timeout="$1"
  shift
  local elapsed=0
  local all_present
  local f

  while true; do
    all_present=true
    for f in "$@"; do
      [[ -e "${CRED_DIR}/${f}" ]] || { all_present=false; break; }
    done
    [[ "${all_present}" == "true" ]] && return 0
    (( elapsed >= timeout )) && return 1
    sleep "${POLL_INTERVAL_SECONDS}"
    elapsed=$(( elapsed + POLL_INTERVAL_SECONDS ))
  done
}

# wait_for_docker_ready polls `docker info`, bounded by $1 seconds, until the
# daemon reachable via DOCKER_HOST answers successfully.
wait_for_docker_ready() {
  local timeout="$1"
  local elapsed=0

  while ! docker info >/dev/null 2>&1; do
    (( elapsed >= timeout )) && return 1
    sleep "${POLL_INTERVAL_SECONDS}"
    elapsed=$(( elapsed + POLL_INTERVAL_SECONDS ))
  done
  return 0
}

# require_gosu fails CLOSED: if gosu is not on PATH we cannot drop privileges,
# so we refuse to run rather than silently continue as root (ADR-002 F11).
require_gosu() {
  command -v gosu >/dev/null 2>&1 && return 0
  fail "gosu is required to drop privileges to ${RUNNER_USER} but was not found on PATH; refusing to run as root (set RUN_AS_ROOT=true to override deliberately)"
}

# ensure_docker_socket_group makes the unprivileged runner user able to reach
# the shared DinD daemon socket (F1). In DinD modes the provider launches the
# sidecar's dockerd with `--group ${DOCKER_SOCK_GID}`, so /run/docker.sock is
# group-owned by that GID and group-writable. gosu re-derives the runner's
# supplementary groups from /etc/group when it drops privileges, so the runner
# user must be a MEMBER of a group with that GID here (the container's
# HostConfig.GroupAdd alone does not survive gosu's initgroups) — otherwise a
# `docker` call as the runner user is permission-denied. No-op when
# DOCKER_SOCK_GID is unset (none mode). Fails CLOSED in DinD mode: without the
# membership the runner cannot use Docker at all, which the job needs.
ensure_docker_socket_group() {
  local gid="${DOCKER_SOCK_GID:-}"
  [[ -z "${gid}" ]] && return 0

  # Looking up the group is CONTROL FLOW, not an error condition: on a clean
  # image no group owns this GID (the NORMAL case), so getent exits non-zero.
  # Under `set -euo pipefail` (line 35) an un-guarded `grp="$(getent … | cut …)"`
  # assignment would abort the whole entrypoint here — before the groupadd branch
  # below ever runs (F1). Suppress getent's diagnostics and neutralize the
  # pipeline's exit status with `|| true`, so a missing group yields an empty
  # `grp` and we fall through to CREATE it, while a pre-existing group (custom
  # image already owning this GID) is REUSED by name — acceptable for socket
  # access. groupadd/usermod remain fail-closed: a genuine failure of either
  # still aborts, because without the membership the runner cannot use Docker.
  local grp
  grp="$(getent group "${gid}" 2>/dev/null | cut -d: -f1 || true)"
  if [[ -z "${grp}" ]]; then
    grp="dockersock"
    groupadd -g "${gid}" "${grp}" \
      || fail "could not create group ${grp} for DinD socket GID ${gid}"
  fi
  usermod -aG "${grp}" "${RUNNER_USER}" \
    || fail "could not add ${RUNNER_USER} to group ${grp} (GID ${gid}) for DinD socket access"
  log "added ${RUNNER_USER} to group ${grp} (GID ${gid}) for DinD socket access"
}

# Step 1: credential wait. Both modes wait for the single atomic delivery
# marker the provider writes last (ADR-002 F1).
wait_for_credentials() {
  log "waiting up to ${CRED_WAIT_SECONDS}s for the delivery marker ${CRED_DIR}/${READY_MARKER}"
  wait_for_files "${CRED_WAIT_SECONDS}" "${READY_MARKER}" \
    || fail "timed out waiting for credential delivery marker ${READY_MARKER} under ${CRED_DIR}"
}

# Step 2: Docker readiness, DinD modes only. Independent of, and run
# concurrently with, step 1 (ADR-001, ADR-002) - skipped entirely when
# DOCKER_HOST is unset (none mode).
maybe_wait_for_docker() {
  if [[ -z "${DOCKER_HOST:-}" ]]; then
    log "DOCKER_HOST unset (none mode): skipping Docker readiness wait"
    return 0
  fi

  log "waiting up to ${DOCKER_WAIT_SECONDS}s for Docker daemon at ${DOCKER_HOST}"
  wait_for_docker_ready "${DOCKER_WAIT_SECONDS}" \
    || fail "timed out waiting for Docker daemon readiness at ${DOCKER_HOST}"
}

# wait_for_externals_seeded is the CONSUMER-side externals seed gate (ADR-003 W2):
# it blocks until the seed-completion marker (GARM_EXTERNALS_SEEDED_MARKER, set by
# the provider ONLY when an externals cache is mounted) exists in the read-only
# externals mount, bounded by $EXTERNALS_WAIT_SECONDS. The provider seeds the
# externals volume under an in-volume flock and writes the atomic `.garm-seeded`
# marker LAST, so the marker's presence proves the tree is fully populated — and the
# flock guarantees the marker EVENTUALLY appears even if a peer/GC reincarnated the
# volume in the provider's unpin→runner-pin gap. Waiting here, at the point of use,
# makes a half-seeded start IMPOSSIBLE regardless of provider-side pin timing — the
# robust backstop the provider-side seed alone cannot guarantee. No-op when the env
# is unset (cache disabled / no externals mount). Fails CLOSED on timeout so a
# never-seeded externals tree never runs the runner against empty Node runtimes.
wait_for_externals_seeded() {
  local marker="${GARM_EXTERNALS_SEEDED_MARKER:-}"
  [[ -z "${marker}" ]] && return 0

  log "waiting up to ${EXTERNALS_WAIT_SECONDS}s for the externals seed marker ${marker}"
  local elapsed=0
  while [[ ! -e "${marker}" ]]; do
    (( elapsed >= EXTERNALS_WAIT_SECONDS )) \
      && fail "timed out waiting for the externals cache to be seeded (marker ${marker} absent after ${EXTERNALS_WAIT_SECONDS}s); refusing to start the runner against a half-seeded externals tree"
    sleep "${POLL_INTERVAL_SECONDS}"
    elapsed=$(( elapsed + POLL_INTERVAL_SECONDS ))
  done
  log "externals cache is fully seeded (${marker} present)"
}

# run_concurrent_waits backgrounds both waits and fails fast: as soon as
# either job fails, the other is killed rather than left to run out its own
# timeout.
run_concurrent_waits() {
  wait_for_credentials &
  local cred_pid=$!
  maybe_wait_for_docker &
  local docker_pid=$!

  local status=0
  wait -n || status=$?
  if (( status != 0 )); then
    kill "${cred_pid}" "${docker_pid}" 2>/dev/null || true
    exit "${status}"
  fi

  status=0
  wait -n || status=$?
  (( status != 0 )) && exit "${status}"
  return 0
}

# prepare_diag ensures the mounted diagnostic-logs directory (GARM_DIAG_DIR, set
# by the provider ONLY when a persistent diag-logs volume is mounted, ADR-003 W2)
# exists and is owned by the runner user before privileges are dropped: a fresh
# named volume mounts root-owned, so without this the unprivileged runner could
# not write its _diag logs into it. This is DIRECTORY SETUP only — retention and
# pruning are enforced provider-side, out of band, NEVER in this untrusted
# entrypoint (ADR-003 F14). The chown is non-recursive: logs from prior jobs are
# already runner-owned, and recursing a full log history every job would be
# needless. No-op when GARM_DIAG_DIR is unset (cache disabled or ineligible).
prepare_diag() {
  local dir="${GARM_DIAG_DIR:-}"
  [[ -z "${dir}" ]] && return 0
  mkdir -p "${dir}"
  chown "${RUNNER_USER}:${RUNNER_USER}" "${dir}"
  log "prepared diagnostic-logs dir ${dir} (owned by ${RUNNER_USER})"
}

# prepare_cache_dirs ensures the mounted persistent-cache directories exist and
# are owned by the runner user BEFORE privileges are dropped (ADR-003 W2, H4). A
# fresh named volume mounts ROOT-owned 0755 whenever its target path is absent
# from the image (Moby copies image ownership onto an empty volume only when the
# path already exists), so without this a real `pnpm install` as the unprivileged
# runner (uid 1001) fails EACCES on the pnpm store. It is driven by the env the
# provider sets — npm_config_store_dir (the pnpm store, set ONLY when a persistent
# store volume is actually mounted) and RUNNER_TOOL_CACHE (the toolcache) — so it
# honors the OPERATOR-CONFIGURED store path, not a hardcoded one. This is
# directory OWNERSHIP setup only; retention/pruning is never done in this
# untrusted entrypoint (ADR-003 F14). The chown is non-recursive: a fresh volume
# is empty and a warm one's contents are already runner-owned from prior jobs, so
# recursing a full cache every job would be needless. No-op for any dir whose env
# is unset (cache disabled or ineligible).
prepare_cache_dirs() {
  local dir
  for dir in "${npm_config_store_dir:-}" "${RUNNER_TOOL_CACHE:-}"; do
    [[ -z "${dir}" ]] && continue
    mkdir -p "${dir}"
    chown "${RUNNER_USER}:${RUNNER_USER}" "${dir}"
    log "prepared cache dir ${dir} (owned by ${RUNNER_USER})"
  done
}

# resolve_workdir mirrors the base image's own convention: an absolute
# RUNNER_WORKDIR is used as-is, a relative one is relative to RUNNER_DIR
# (which is also the entrypoint's cwd by the time this runs).
resolve_workdir() {
  local workdir="${RUNNER_WORKDIR:-_work}"
  case "${workdir}" in
    /*) printf '%s' "${workdir}" ;;
    *) printf '%s/%s' "${RUNNER_DIR}" "${workdir}" ;;
  esac
}

# prepare_workdir honors RUNNER_WORKDIR (always set per ADR-002) by ensuring
# the directory exists and is owned by the runner user, even in JIT mode where
# the actual job workdir is baked into the JIT config rather than read from
# this env var at run.sh startup.
prepare_workdir() {
  local workdir
  workdir="$(resolve_workdir)"
  mkdir -p "${workdir}"
  chown "${RUNNER_USER}:${RUNNER_USER}" "${workdir}"
}

# install_jit_credentials points the runner install dir at the provider-
# delivered credential files via SYMLINKS. The credential files themselves
# stay resident on the memory-backed tmpfs — only the symlinks live on the
# container's writable layer, so no credential ever lands on host-backed disk
# (ADR-002 F2). No config.sh, no --jitconfig. run.sh reads .runner /
# .credentials / .credentials_rsaparams and follows the symlinks; the tmpfs
# files are owned by (and readable by) the runner user the provider's delivery
# exec created them as.
install_jit_credentials() {
  log "linking JIT credential files from ${CRED_DIR} into ${RUNNER_DIR}"
  ln -sfn "${CRED_DIR}/runner" "${RUNNER_DIR}/.runner"
  ln -sfn "${CRED_DIR}/credentials" "${RUNNER_DIR}/.credentials"
  ln -sfn "${CRED_DIR}/credentials_rsaparams" "${RUNNER_DIR}/.credentials_rsaparams"
  prepare_workdir
}

# build_non_jit_url reconstructs the GitHub scope URL config.sh expects from
# GITHUB_URL (host only, per internal/spec/env.go's BuildRunnerEnv) plus
# whichever of RUNNER_ENTERPRISE / RUNNER_ORG+RUNNER_REPO / RUNNER_ORG the
# provider set for this entity scope.
build_non_jit_url() {
  local host="${GITHUB_URL:?GITHUB_URL is required}"
  host="${host%/}"

  if [[ -n "${RUNNER_ENTERPRISE:-}" ]]; then
    printf '%s/enterprises/%s' "${host}" "${RUNNER_ENTERPRISE}"
  elif [[ -n "${RUNNER_REPO:-}" ]]; then
    printf '%s/%s/%s' "${host}" "${RUNNER_ORG:?RUNNER_ORG is required with RUNNER_REPO}" "${RUNNER_REPO}"
  elif [[ -n "${RUNNER_ORG:-}" ]]; then
    printf '%s/%s' "${host}" "${RUNNER_ORG}"
  else
    fail "none of RUNNER_ENTERPRISE, RUNNER_ORG/RUNNER_REPO, or RUNNER_ORG set for non-JIT registration"
  fi
}

# own_runner_dir_as_runner chowns the install dir's top-level entries, skipping
# the large image-shipped bin/externals trees that already ship runner-owned
# (mirrors the base image's own optimization: recursing over those defeats
# overlay copy-up performance for no ownership benefit). It runs before
# config.sh so the runner user (via gosu) can write .runner/.credentials* into
# the install dir.
own_runner_dir_as_runner() {
  chown "${RUNNER_USER}:${RUNNER_USER}" "${RUNNER_DIR}"
  find "${RUNNER_DIR}" -mindepth 1 -maxdepth 1 \
    ! -name bin ! -name externals \
    -exec chown -R "${RUNNER_USER}:${RUNNER_USER}" {} +
}

# run_config_sh runs config.sh with privilege handling that satisfies the base
# runner's own root guard (ADR-002 F4): by default it drops to the runner user
# via gosu (config.sh refuses to run as root without RUNNER_ALLOW_RUNASROOT);
# only when RUN_AS_ROOT=true is set deliberately does it run as root with
# RUNNER_ALLOW_RUNASROOT=1. Fails CLOSED when gosu is missing (F11).
#
# config.sh runs BACKGROUNDED, with the function then `wait`-ing on it,
# rather than as a plain foreground command — deliberately: bash only acts on
# a trapped signal (the HUP/INT/TERM traps install_non_jit_registration sets
# around this call) once it regains control between commands. A signal
# arriving while bash is synchronously blocked on a *foreground* child is not
# acted on until that child exits on its own; `wait` is the one bash builtin
# that IS interrupted immediately by a trapped signal, returning early so the
# trap runs promptly instead of only after config.sh finishes (verified
# empirically against bash 3.2 and 5.3 — this is not version-specific).
# `wait`'s own exit status becomes run_config_sh's return value unchanged.
run_config_sh() {
  local pid
  if [[ "${RUN_AS_ROOT:-false}" == "true" ]]; then
    log "RUN_AS_ROOT=true: running config.sh as $(id -un) with RUNNER_ALLOW_RUNASROOT=1"
    RUNNER_ALLOW_RUNASROOT=1 ./config.sh "$@" &
    pid=$!
  else
    require_gosu
    log "running config.sh as ${RUNNER_USER} via gosu"
    gosu "${RUNNER_USER}" ./config.sh "$@" &
    pid=$!
  fi
  wait "${pid}"
}

# move_non_jit_credentials_to_tmpfs relocates the .runner/.credentials/
# .credentials_rsaparams that config.sh generated on the disk-backed writable
# layer onto the memory-backed tmpfs, replacing each with a symlink pointing at
# its tmpfs bare-name target (exactly like install_jit_credentials's steady
# state). It runs ONLY after config.sh SUCCEEDS, so at steady state the non-JIT
# credentials live only on tmpfs (ADR-002 F2), reached from the install dir
# through symlinks.
#
# This is deliberately a post-config move rather than a pre-config symlink: the
# real .NET runner's `Runner.Listener configure` reads .credentials in its
# startup HostContext constructor, so a symlink pre-created before config.sh —
# whose tmpfs target does not exist yet in non-JIT mode (only registration-token
# is delivered; .credentials is CREATED by config.sh during registration) —
# makes that read fail (FileNotFoundException, exit 134) BEFORE registration
# runs (verified against the real runner on a live daemon).
#
# The move runs as root (the entrypoint has not dropped privileges yet), so it
# can write into the runner-owned, mode-0700 tmpfs; config.sh generated the
# files as the runner user, and mv preserves that ownership, so the runner can
# still read them through the symlinks after run.sh drops to it. The per-file
# guard tolerates a runner that (for any scope) did not emit one of the files.
move_non_jit_credentials_to_tmpfs() {
  local f bare
  for f in .runner .credentials .credentials_rsaparams; do
    # ${f#.} strips the leading dot to the tmpfs bare name (runner, etc.).
    bare="${f#.}"
    if [[ -e "${RUNNER_DIR}/${f}" ]]; then
      mv -f "${RUNNER_DIR}/${f}" "${CRED_DIR}/${bare}"
      ln -sfn "${CRED_DIR}/${bare}" "${RUNNER_DIR}/${f}"
    fi
  done
}

# scrub_non_jit_credentials unconditionally removes the credential-pattern
# files — the dotted install-dir names AND their tmpfs bare-name
# counterparts — so no failure path leaves credentials on the container's
# disk-backed writable layer (ADR-002 F2). Shared by the EXIT-trap and
# signal-trap handlers below.
scrub_non_jit_credentials() {
  local f
  for f in .runner .credentials .credentials_rsaparams; do
    # ${f#.} strips the leading dot to the tmpfs bare name (runner, etc.).
    rm -f "${RUNNER_DIR}/${f}" "${CRED_DIR}/${f#.}"
  done
}

# scrub_non_jit_credentials_on_failure is the EXIT-trap belt-and-braces for the
# non-JIT config phase: if config.sh fails, it removes any credential-pattern
# files so no failure path leaves credentials on the container's disk-backed
# writable layer (ADR-002 F2). It preserves the triggering exit code so a
# failed config still exits non-zero for GARM.
#
# This does NOT cover an unhandled TERM/INT/HUP: bash only runs the EXIT trap
# on normal exit (including the `exit` builtin), never when the process is
# killed by a signal it has no trap for — that terminates the shell
# immediately (exit 143/130/129) without running EXIT-trap commands at all.
# scrub_non_jit_credentials_on_signal below covers that case explicitly.
scrub_non_jit_credentials_on_failure() {
  local code=$?
  if (( code == 0 )); then
    return 0
  fi
  scrub_non_jit_credentials
  log "config.sh failed (exit ${code}); scrubbed credential-pattern files from the writable layer"
  return "${code}"
}

# scrub_non_jit_credentials_on_signal is the HUP/INT/TERM-trap handler for the
# non-JIT config phase: it scrubs the same credential-pattern files as the
# EXIT trap above, then exits with the conventional 128+signum status
# (143/130/129) so the container's exit code still reports "killed by
# signal" to GARM/the container runtime. It clears every trap this phase set
# first, so the `exit` call below triggers the EXIT trap only once (as a
# no-op — the scrub already ran) rather than re-running
# scrub_non_jit_credentials_on_failure with a stale $?.
scrub_non_jit_credentials_on_signal() {
  local signum="$1"
  trap - EXIT HUP INT TERM
  scrub_non_jit_credentials
  log "received signal ${signum} during config.sh; scrubbed credential-pattern files from the writable layer"
  exit "$(( 128 + signum ))"
}

# install_non_jit_registration registers the runner with config.sh (as the
# runner user via gosu by default, ADR-002 F4), appending --disableupdate
# unconditionally because the image is digest-managed (ADR-002 F9). config.sh
# writes .runner/.credentials/.credentials_rsaparams into the install dir
# normally; ONLY after it SUCCEEDS are those files moved onto the tmpfs and
# replaced with symlinks (move_non_jit_credentials_to_tmpfs), so steady-state
# credentials are tmpfs-resident. The credentials are deliberately NOT
# pre-linked before config.sh: the real runner's configure step reads
# .credentials in its startup constructor and a dangling pre-created symlink
# (its tmpfs target is not delivered in non-JIT mode) crashes it before
# registration. The config phase is guarded by an EXIT trap plus HUP/INT/TERM
# traps so no failure path — including an unhandled signal — leaves credentials
# on the writable layer.
install_non_jit_registration() {
  local token url
  token="$(cat "${CRED_DIR}/registration-token")"
  url="$(build_non_jit_url)"

  local args=(
    --unattended
    --url "${url}"
    --token "${token}"
    --name "${RUNNER_NAME:?RUNNER_NAME is required}"
    --work "${RUNNER_WORKDIR:-_work}"
    --disableupdate
  )
  [[ -n "${RUNNER_GROUP:-}" ]] && args+=(--runnergroup "${RUNNER_GROUP}")
  [[ -n "${RUNNER_LABELS:-}" ]] && args+=(--labels "${RUNNER_LABELS}")
  [[ "${RUNNER_NO_DEFAULT_LABELS:-false}" == "true" ]] && args+=(--no-default-labels)
  [[ "${RUNNER_EPHEMERAL:-false}" == "true" ]] && args+=(--ephemeral)

  own_runner_dir_as_runner

  # Scrub credential-pattern files from the writable layer (and tmpfs) if
  # config.sh fails, or if the container receives HUP/INT/TERM while it is
  # running (an unhandled signal would otherwise bypass the EXIT trap
  # entirely — see scrub_non_jit_credentials_on_signal above), then clear
  # every trap on success so the tmpfs-resident credentials are kept.
  trap scrub_non_jit_credentials_on_failure EXIT
  trap 'scrub_non_jit_credentials_on_signal 1' HUP
  trap 'scrub_non_jit_credentials_on_signal 2' INT
  trap 'scrub_non_jit_credentials_on_signal 15' TERM
  log "registering runner via config.sh (non-JIT)"
  run_config_sh "${args[@]}"
  # config.sh SUCCEEDED (set -e would have exited into the EXIT-trap scrub
  # otherwise): relocate the generated credentials onto the tmpfs and symlink
  # them, so steady-state credentials are tmpfs-resident like the JIT path.
  move_non_jit_credentials_to_tmpfs
  trap - EXIT HUP INT TERM

  prepare_workdir
}

# exec_as_runner drops root via gosu (mirroring the base image's own
# convention) unless RUN_AS_ROOT=true, then execs the runner in the foreground
# so it receives signals directly. Fails CLOSED when gosu is missing (F11).
exec_as_runner() {
  if [[ "${RUN_AS_ROOT:-false}" == "true" ]]; then
    log "RUN_AS_ROOT=true: running as $(id -un)"
    exec "$@"
  fi

  require_gosu
  log "dropping privileges to ${RUNNER_USER} via gosu"
  exec gosu "${RUNNER_USER}" "$@"
}

main() {
  cd "${RUNNER_DIR}"

  run_concurrent_waits

  # DinD modes: make the runner user a member of the DinD socket group before
  # dropping privileges, so `docker` works as the unprivileged runner (F1).
  ensure_docker_socket_group

  # W2: own the mounted diagnostic-logs dir (if any) so the unprivileged runner
  # can write into the persistent diag volume (ADR-003; no-op when unset).
  prepare_diag

  # H4: own the mounted pnpm store / toolcache dirs (if any) so the unprivileged
  # runner can write into a fresh, root-owned persistent-cache volume (ADR-003;
  # no-op when the corresponding env is unset).
  prepare_cache_dirs

  if [[ "${JIT_CONFIG_ENABLED:-false}" == "true" ]]; then
    install_jit_credentials
  else
    install_non_jit_registration
  fi

  # W2 consumer-side externals seed gate: block until the read-only externals
  # cache is fully seeded (ADR-003) before launching run.sh, which executes the
  # Node runtimes THROUGH that mount. No-op when no externals cache is mounted.
  wait_for_externals_seeded

  exec_as_runner ./run.sh
}

# Run main only when EXECUTED directly (the container's ENTRYPOINT), not when
# SOURCED. Sourcing loads the functions without running the full flow, so the
# live non-root verification harness (internal/verify) can invoke the REAL
# ensure_docker_socket_group under this file's own `set -euo pipefail` — proving
# F1 against the production code verbatim rather than a re-implementation. In the
# production container ${BASH_SOURCE[0]} == ${0} == /entrypoint.sh, so main runs
# exactly as before.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
